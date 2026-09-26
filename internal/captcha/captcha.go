// Package captcha mints Aliyun traceless-verification parameters for the
// chat endpoint. One full pass (init -> track payload -> verify) burns a
// single device token collected from a real desktop browser session.
package captcha

import (
	"bytes"
	"compress/zlib"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"glm-aki-proxy/internal/util"
)

// Protocol constants observed from the Z.AI web frontend. They are
// functional interop values, not secrets.
const (
	rpcAccessKey = "LTAI5tSEBwYMwVKAQGpxmvTd"
	rpcSecretKey = "YSKfst7GaVkXwZYvVihJsKF9r89koz"
	sceneID      = "didk33e0"
	rpcVersion   = "2023-03-05"

	initEndpoint   = "https://no8xfe.captcha-open-southeast.aliyuncs.com/"
	verifyEndpoint = "https://no8xfe-verify.captcha-open-southeast.aliyuncs.com/"

	devicePrefix = "no8xfe"
	deviceRegion = "sgp"

	aesPass1Key = "c175a358550d02e2"
	aesPass2Key = "45f8ac1e1de14397"

	argCipherKey  = "4xrihv8zb8tf1mfj"
	dataCipherKey = "3e627e1b4c63f913"

	maxAttempts = 6
)

// sealBox is the 64-entry permutation both stream ciphers start from.
var sealBox = [64]int{
	32, 50, 10, 51, 6, 44, 37, 16, 46, 11, 62, 19, 43, 25, 23, 30,
	60, 33, 53, 34, 7, 26, 12, 48, 5, 2, 20, 4, 61, 13, 47, 49,
	18, 29, 27, 22, 1, 17, 39, 56, 41, 38, 55, 31, 15, 58, 52, 40,
	8, 57, 45, 35, 59, 36, 42, 54, 63, 3, 24, 28, 14, 9, 0, 21,
}

// ivWords builds the fixed AES IV (four big-endian words).
var ivWords = []uint32{808530483, 875902519, 943276354, 1128547654}

// pctEncode percent-encodes everything outside the unreserved set.
func pctEncode(s string) string {
	const upper = "0123456789ABCDEF"
	var b strings.Builder
	b.Grow(len(s) + 16)
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= '0' && c <= '9' || c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' ||
			c == '-' || c == '_' || c == '.' || c == '~' {
			b.WriteByte(c)
			continue
		}
		b.WriteByte('%')
		b.WriteByte(upper[c>>4])
		b.WriteByte(upper[c&15])
	}
	return b.String()
}

// rpcSign signs Aliyun RPC parameters: sorted k=v pairs, then
// POST&<enc / enc(canonical)> HMAC-SHA1 with secret+"&", base64 output.
func rpcSign(params map[string]string, secret string) string {
	keys := make([]string, 0, len(params))
	for k := range params {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var canon strings.Builder
	for i, k := range keys {
		if i > 0 {
			canon.WriteByte('&')
		}
		canon.WriteString(pctEncode(k))
		canon.WriteByte('=')
		canon.WriteString(pctEncode(params[k]))
	}
	toSign := "POST&" + pctEncode("/") + "&" + pctEncode(canon.String())
	mac := hmac.New(sha1.New, []byte(secret+"&"))
	mac.Write([]byte(toSign))
	return base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

// formBody renders sorted params as an x-www-form-urlencoded body.
func formBody(params map[string]string) string {
	keys := make([]string, 0, len(params))
	for k := range params {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for i, k := range keys {
		if i > 0 {
			b.WriteByte('&')
		}
		b.WriteString(pctEncode(k))
		b.WriteByte('=')
		b.WriteString(pctEncode(params[k]))
	}
	return b.String()
}

// postForm POSTs a form body and returns the response text.
func postForm(url, body string) (string, error) {
	req, err := http.NewRequest("POST", url, strings.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded; charset=UTF-8")
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/146.0.0.0 Safari/537.36")
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", err
	}
	return string(data), nil
}

// rawJSON marshals without HTML escaping (the track payload and the
// verify params must byte-match what a browser would send).
func rawJSON(v interface{}) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	raw := buf.Bytes()
	return raw[:len(raw)-1], nil
}

// aesEncryptB64 encrypts with AES-128-CBC (fixed IV, PKCS#7) and returns
// base64 ciphertext.
func aesEncryptB64(key16, plaintext string) (string, error) {
	if len(key16) < 16 || plaintext == "" {
		return "", fmt.Errorf("captcha: bad aes input")
	}
	block, err := aes.NewCipher([]byte(key16)[:16])
	if err != nil {
		return "", err
	}
	iv := make([]byte, 16)
	for i, w := range ivWords {
		iv[i*4], iv[i*4+1], iv[i*4+2], iv[i*4+3] = byte(w>>24), byte(w>>16), byte(w>>8), byte(w)
	}
	plain := []byte(plaintext)
	pad := aes.BlockSize - len(plain)%aes.BlockSize
	padded := make([]byte, len(plain)+pad)
	copy(padded, plain)
	for i := len(plain); i < len(padded); i++ {
		padded[i] = byte(pad)
	}
	out := make([]byte, len(padded))
	cipher.NewCBCEncrypter(block, iv).CryptBlocks(out, padded)
	return base64.StdEncoding.EncodeToString(out), nil
}

// DeviceData builds the device fingerprint string bound to one device
// token: two AES passes over "#"-joined descriptor fields.
func DeviceData(scene, prefix, region, appKey string) (string, error) {
	first := strings.Join([]string{"W.10001.c", "saf-captcha", scene, "captcha-normal", prefix, region}, "#")
	e1, err := aesEncryptB64(aesPass1Key, first)
	if err != nil {
		return "", err
	}
	second := strings.Join([]string{appKey, "W", e1, "W20220202", "CLOUD", ""}, "#")
	return aesEncryptB64(aesPass2Key, second)
}

// Fingerprint extracts the device hash from a base64 "SG_WEB#<hash>-h-…"
// device token; "" when malformed.
func Fingerprint(deviceToken string) string {
	raw, err := base64.StdEncoding.DecodeString(deviceToken)
	if err != nil {
		return ""
	}
	parts := strings.SplitN(string(raw), "#", 3)
	if len(parts) < 2 {
		return ""
	}
	return strings.SplitN(parts[1], "-h-", 2)[0]
}

// initCertify runs InitCaptchaV3 (with device data when provided) and
// returns the CertifyId.
func initCertify(deviceData string) (string, error) {
	params := map[string]string{
		"AccessKeyId":      rpcAccessKey,
		"Action":           "InitCaptchaV3",
		"Format":           "JSON",
		"Language":         "en",
		"Mode":             "popup",
		"SceneId":          sceneID,
		"SignatureMethod":  "HMAC-SHA1",
		"SignatureNonce":   util.UUIDv4(),
		"SignatureVersion": "1.0",
		"Timestamp":        time.Now().UTC().Format("2006-01-02T15:04:05Z"),
		"UpLang":           "true",
		"Version":          rpcVersion,
	}
	if deviceData != "" {
		params["DeviceData"] = deviceData
	}
	params["Signature"] = rpcSign(params, rpcSecretKey)
	resp, err := postForm(initEndpoint, formBody(params))
	if err != nil {
		return "", err
	}
	var out struct {
		CertifyID string `json:"CertifyId"`
	}
	if err := json.Unmarshal([]byte(resp), &out); err != nil {
		return "", fmt.Errorf("captcha: init parse: %w", err)
	}
	if out.CertifyID == "" {
		return "", fmt.Errorf("captcha: init returned no CertifyId")
	}
	return out.CertifyID, nil
}

// sealRun applies the custom 64-state stream cipher keyed by key.
func sealRun(data []byte, key string) []byte {
	r := sealBox
	const n = 64
	i, j := 0, 0
	for i < n {
		j = (((i + j + r[i] + r[j]) >> 1) + int(key[i%len(key)])) & (n - 1)
		if i != j {
			r[i], r[j] = r[j], r[i]
		}
		i++
	}
	out := make([]byte, 0, len(data))
	e, a := 0, 0
	for _, b := range data {
		a = ((e ^ a) + (r[e] ^ r[a])) & (n - 1)
		if e != a {
			r[e], r[a] = r[a], r[e]
		}
		m := int(b) + e + r[e] - a - r[a]
		m ^= r[e] + r[a]
		m ^= r[(r[e]+r[a])&(n-1)]
		out = append(out, byte(m&255))
		e = (e + 1) & (n - 1)
	}
	return out
}

// decodePct decodes %XX sequences (identity for plain strings).
func decodePct(s string) []byte {
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); {
		if s[i] == '%' && i+2 < len(s) {
			out = append(out, hexVal(s[i+1])<<4|hexVal(s[i+2]))
			i += 3
		} else {
			out = append(out, s[i])
			i++
		}
	}
	return out
}

func hexVal(c byte) byte {
	switch {
	case c >= '0' && c <= '9':
		return c - '0'
	case c >= 'A' && c <= 'F':
		return c - 'A' + 10
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10
	}
	return 0
}

// trackArg seals the URL-encoded certify id (the "arg" field).
func trackArg(certifyID string) string {
	return base64.StdEncoding.EncodeToString(sealRun(decodePct(pctEncode(certifyID)), argCipherKey))
}

// mouseTrack is the pointer-activity envelope the SDK would report.
type mouseTrack struct {
	FI        string `json:"fi"`
	KS        string `json:"ks"`
	MC        string `json:"mc"`
	MP        string `json:"mp"`
	MU        string `json:"mu"`
	StartTime int64  `json:"startTime"`
	TC        string `json:"tc"`
	TE        string `json:"te"`
	TMV       string `json:"tmv"`
}

type trackEnvelope struct {
	TrackList      mouseTrack `json:"TrackList"`
	TrackStartTime int64      `json:"TrackStartTime"`
	VerifyTime     int64      `json:"VerifyTime"`
	Arg            string     `json:"arg"`
}

// aliDigest is the custom 16-state hash over the track JSON.
func aliDigest(text, salt string) string {
	var st [16]int
	for i := range st {
		st[i] = (i << 4) + (i % 16)
	}
	i, j := 0, 0
	for i < 16 {
		j = (((i + j + st[i] + st[j]) >> 1) + int(salt[i%len(salt)])) & 15
		st[i], st[j] = st[j], st[i]
		i++
	}
	p, q, k := 0, 0, 0
	for k < len(text) {
		q = ((p ^ q) + (st[p] ^ st[q])) & 15
		st[p], st[q] = st[q], st[p]
		c := (int(text[k]) + p + q) ^ st[p] ^ st[q]
		st[p] = c & 255
		p = (p + 1) & 15
		k++
	}
	for step := 0; step < 32; step++ {
		pos := step % 16
		if pos != 0 {
			st[pos] ^= st[pos-1]
		} else {
			st[0] ^= st[15]
		}
	}
	const hexd = "0123456789abcdef"
	var sb strings.Builder
	sb.Grow(32)
	for _, v := range st {
		sb.WriteByte(hexd[(v>>4)&15])
		sb.WriteByte(hexd[v&15])
	}
	return sb.String()
}

// trackDataValue builds the encrypted "data" field for verification:
// digest + JSON, zlib-compressed, base64ed, then sealed.
func trackDataValue(certifyID string) (string, error) {
	now := time.Now().UnixMilli()
	env := trackEnvelope{
		TrackList:      mouseTrack{StartTime: now},
		TrackStartTime: now,
		VerifyTime:     now + 15 + time.Now().UnixNano()%27,
		Arg:            trackArg(certifyID),
	}
	body, err := rawJSON(env)
	if err != nil {
		return "", err
	}
	combined := aliDigest(string(body), "0000") + string(body)
	var zbuf bytes.Buffer
	zw := zlib.NewWriter(&zbuf)
	if _, err := zw.Write([]byte(combined)); err != nil {
		return "", err
	}
	if err := zw.Close(); err != nil {
		return "", err
	}
	staged := base64.StdEncoding.EncodeToString(zbuf.Bytes())
	return base64.StdEncoding.EncodeToString(sealRun([]byte(staged), dataCipherKey)), nil
}

// verifyPass runs VerifyCaptchaV3 and returns the final base64
// captcha_verify_param ("" with nil error means "try another token").
func verifyPass(certifyID, dataValue, deviceToken string) (string, error) {
	cvp, err := rawJSON(map[string]string{
		"certifyId":   certifyID,
		"data":        dataValue,
		"deviceToken": deviceToken,
		"sceneId":     sceneID,
	})
	if err != nil {
		return "", err
	}
	params := map[string]string{
		"AccessKeyId":        rpcAccessKey,
		"Action":             "VerifyCaptchaV3",
		"Format":             "JSON",
		"SignatureMethod":    "HMAC-SHA1",
		"SignatureVersion":   "1.0",
		"Timestamp":          time.Now().UTC().Format("2006-01-02T15:04:05Z"),
		"Version":            rpcVersion,
		"SceneId":            sceneID,
		"CertifyId":          certifyID,
		"CaptchaVerifyParam": string(cvp),
		"SignatureNonce":     util.UUIDv4(),
	}
	params["Signature"] = rpcSign(params, rpcSecretKey)
	resp, err := postForm(verifyEndpoint, formBody(params))
	if err != nil {
		return "", err
	}
	var out struct {
		Success bool `json:"Success"`
		Result  struct {
			VerifyResult  bool   `json:"VerifyResult"`
			SecurityToken string `json:"securityToken"`
			CertifyID     string `json:"certifyId"`
		} `json:"Result"`
	}
	if err := json.Unmarshal([]byte(resp), &out); err != nil {
		return "", fmt.Errorf("captcha: verify parse: %w", err)
	}
	if !out.Success || !out.Result.VerifyResult {
		return "", nil
	}
	if out.Result.SecurityToken == "" || out.Result.CertifyID == "" {
		return "", nil
	}
	final, err := rawJSON(map[string]interface{}{
		"certifyId":     out.Result.CertifyID,
		"isSign":        true,
		"sceneId":       sceneID,
		"securityToken": out.Result.SecurityToken,
	})
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(final), nil
}

// mintOnce runs a single init -> track -> verify cycle for one device
// token and returns the captcha_verify_param ("" = token unusable).
func mintOnce(deviceToken string) (string, error) {
	fp := Fingerprint(deviceToken)
	dd, err := DeviceData(sceneID, devicePrefix, deviceRegion, fp)
	if err != nil || fp == "" {
		dd, err = "", nil
	}
	certifyID := ""
	if dd != "" {
		certifyID, err = initCertify(dd)
	}
	if certifyID == "" {
		certifyID, err = initCertify("")
	}
	if err != nil {
		return "", err
	}
	dataValue, err := trackDataValue(certifyID)
	if err != nil {
		return "", err
	}
	return verifyPass(certifyID, dataValue, deviceToken)
}

var mintMu sync.Mutex

// TokenTaker abstracts the device-token pool (see internal/pool).
type TokenTaker interface {
	Take() (string, bool)
}

// Solve mints one captcha_verify_param, burning up to maxAttempts
// single-use device tokens. "" means the pool is dry or rejected.
func Solve(take TokenTaker, verbose bool) string {
	mintMu.Lock()
	defer mintMu.Unlock()
	say := func(f string, a ...interface{}) {
		if verbose {
			log.Printf("[captcha] "+f, a...)
		}
	}
	for attempt := 0; attempt < maxAttempts; attempt++ {
		tok, ok := take.Take()
		if !ok {
			say("pool empty on attempt %d", attempt+1)
			return ""
		}
		say("attempt %d with token %s", attempt+1, util.ShortToken(tok))
		param, err := mintOnce(tok)
		if err != nil {
			say("attempt %d error: %v", attempt+1, err)
			time.Sleep(time.Duration(attempt+1) * 150 * time.Millisecond)
			continue
		}
		if param != "" {
			return param
		}
		say("attempt %d rejected by verifier", attempt+1)
		time.Sleep(time.Duration(attempt+1) * 150 * time.Millisecond)
	}
	log.Printf("[captcha] solve failed after %d attempts", maxAttempts)
	return ""
}
