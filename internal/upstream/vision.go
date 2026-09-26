// Vision pipeline: image inputs are not sent inline — each image is
// resolved to bytes (data: URL or http download), uploaded to Z.AI's
// file endpoint, and referenced via a top-level "files" array plus a
// file-id rewrite of the message part. Mirrors the chat.z.ai web
// client. Written fresh for this repo.
//
// Uploads require a logged-in session; guests get a clear error.
package upstream

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"path"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"glm-aki-proxy/internal/session"
	"glm-aki-proxy/internal/util"
)

const (
	maxVisionImages = 10
	maxVisionBytes  = int64(50 << 20) // 50 MB per image
	visionDlTimeout = 60 * time.Second
	visionUpTimeout = 120 * time.Second
	visionWorkers   = 4
)

var visionDL = &http.Client{
	Transport: &http.Transport{
		Proxy:               http.ProxyFromEnvironment,
		MaxIdleConns:        50,
		MaxIdleConnsPerHost: 10,
		IdleConnTimeout:     90 * time.Second,
		TLSHandshakeTimeout: 10 * time.Second,
	},
}

type visionImage struct {
	data        []byte
	filename    string
	contentType string
}

// prepareVision scans OpenAI-shaped messages for image_url parts. With
// none present it returns the input untouched (no "files" field ever
// appears for text-only requests). Otherwise every image is resolved,
// uploaded, the part urls are rewritten to file ids, and file entries
// are returned for the chat body.
func prepareVision(ctx context.Context, sess *session.Session, msgs []json.RawMessage) ([]json.RawMessage, []map[string]interface{}, error) {
	type found struct{ msg, part int }
	type msgParts struct {
		msg   map[string]interface{}
		parts []map[string]interface{}
	}
	parsed := make([]msgParts, len(msgs))
	var hits []found
	for i, raw := range msgs {
		var m map[string]interface{}
		if json.Unmarshal(raw, &m) != nil {
			continue
		}
		cr, _ := json.Marshal(m["content"])
		var parts []map[string]interface{}
		if json.Unmarshal(cr, &parts) != nil {
			continue
		}
		parsed[i] = msgParts{msg: m, parts: parts}
		for p, part := range parts {
			if t, _ := part["type"].(string); t != "image_url" {
				continue
			}
			iu, _ := part["image_url"].(map[string]interface{})
			u, _ := iu["url"].(string)
			if strings.TrimSpace(u) == "" {
				continue
			}
			hits = append(hits, found{msg: i, part: p})
		}
	}
	if len(hits) == 0 {
		return msgs, nil, nil
	}
	if len(hits) > maxVisionImages {
		return nil, nil, fmt.Errorf("too many images: %d, max %d per request", len(hits), maxVisionImages)
	}
	token, _, _, _, ready := sess.Snapshot()
	if !ready || token == "" {
		return nil, nil, fmt.Errorf("vision requires a logged-in session")
	}

	refIDs := map[int]string{}
	for _, h := range hits {
		if _, ok := refIDs[h.msg]; !ok {
			refIDs[h.msg] = util.UUIDv4()
		}
	}
	ids := make([]string, len(hits))
	imgs := make([]*visionImage, len(hits))
	errs := make([]error, len(hits))
	sem := make(chan struct{}, visionWorkers)
	var wg sync.WaitGroup
	for i, h := range hits {
		wg.Add(1)
		go func(i int, h found) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			part := parsed[h.msg].parts[h.part]
			iu, _ := part["image_url"].(map[string]interface{})
			u, _ := iu["url"].(string)
			img, err := resolveVisionImage(ctx, u)
			if err != nil {
				errs[i] = err
				return
			}
			id, err := uploadVisionImage(ctx, token, img)
			if err != nil {
				errs[i] = err
				return
			}
			ids[i] = id
			imgs[i] = img
		}(i, h)
	}
	wg.Wait()
	var problems []string
	for _, e := range errs {
		if e != nil {
			problems = append(problems, e.Error())
		}
	}
	if len(problems) > 0 {
		return nil, nil, fmt.Errorf("image processing failed: %s", strings.Join(problems, "; "))
	}

	entries := make([]map[string]interface{}, len(hits))
	for i, h := range hits {
		part := parsed[h.msg].parts[h.part]
		iu, _ := part["image_url"].(map[string]interface{})
		iu["url"] = ids[i]
		entries[i] = visionFileEntry(ids[i], imgs[i].filename, refIDs[h.msg])
	}
	out := make([]json.RawMessage, len(msgs))
	for i, raw := range msgs {
		if parsed[i].msg == nil {
			out[i] = raw
			continue
		}
		m := parsed[i].msg
		if _, ok := m["content"].(string); !ok {
			m["content"] = parsed[i].parts
		}
		b, err := json.Marshal(m)
		if err != nil {
			return nil, nil, fmt.Errorf("re-encode vision message: %w", err)
		}
		out[i] = b
	}
	return out, entries, nil
}

// visionFileEntry shapes one upload into the web client's files entry.
func visionFileEntry(id, name, refMsgID string) map[string]interface{} {
	return map[string]interface{}{
		"type":            "image",
		"file":            map[string]interface{}{"id": id, "filename": name},
		"id":              id,
		"url":             "/api/v1/files/" + id,
		"name":            name,
		"status":          "uploaded",
		"error":           "",
		"itemId":          util.UUIDv4(),
		"media":           "image",
		"uploadedAt":      time.Now().UnixMilli(),
		"ref_user_msg_id": refMsgID,
	}
}

// resolveVisionImage reduces a data: or http(s) URL to bytes.
func resolveVisionImage(ctx context.Context, rawURL string) (*visionImage, error) {
	rawURL = strings.TrimSpace(rawURL)
	if rawURL == "" {
		return nil, fmt.Errorf("empty image URL")
	}
	if strings.HasPrefix(rawURL, "data:") {
		return decodeVisionDataURL(rawURL)
	}
	return downloadVisionImage(ctx, rawURL)
}

func decodeVisionDataURL(rawURL string) (*visionImage, error) {
	comma := strings.Index(rawURL, ",")
	if comma < len("data:") {
		return nil, fmt.Errorf("malformed data URL")
	}
	header, payload := rawURL[len("data:"):comma], rawURL[comma+1:]
	segs := strings.Split(header, ";")
	mime := strings.TrimSpace(segs[0])
	isB64 := false
	for _, s := range segs[1:] {
		if strings.EqualFold(strings.TrimSpace(s), "base64") {
			isB64 = true
		}
	}
	if !isB64 {
		return nil, fmt.Errorf("data URL must be base64-encoded")
	}
	data, err := base64.StdEncoding.DecodeString(payload)
	if err != nil {
		data, err = lenientB64(payload)
		if err != nil {
			return nil, fmt.Errorf("invalid base64 image payload: %w", err)
		}
	}
	if int64(len(data)) > maxVisionBytes {
		return nil, fmt.Errorf("image exceeds %d bytes", maxVisionBytes)
	}
	if mime == "" {
		mime = http.DetectContentType(data)
	}
	return &visionImage{data: data, filename: "image" + visionExt(mime), contentType: mime}, nil
}

func downloadVisionImage(ctx context.Context, rawURL string) (*visionImage, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("invalid image URL: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("unsupported image URL scheme %q", u.Scheme)
	}
	dctx, cancel := context.WithTimeout(ctx, visionDlTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(dctx, "GET", rawURL, nil)
	if err != nil {
		return nil, fmt.Errorf("image download request: %w", err)
	}
	req.Header.Set("User-Agent", session.ChromeUA)
	req.Header.Set("Accept", "image/*,*/*;q=0.8")
	resp, err := visionDL.Do(req)
	if err != nil {
		return nil, fmt.Errorf("image download failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("image download status %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxVisionBytes+1))
	if err != nil {
		return nil, fmt.Errorf("image download read: %w", err)
	}
	if int64(len(data)) > maxVisionBytes {
		return nil, fmt.Errorf("image exceeds %d bytes", maxVisionBytes)
	}
	ct := http.DetectContentType(data)
	if !strings.HasPrefix(ct, "image/") {
		if hct := strings.TrimSpace(strings.SplitN(resp.Header.Get("Content-Type"), ";", 2)[0]); strings.HasPrefix(hct, "image/") {
			ct = hct
		}
	}
	name := sanitizeFilename(strings.TrimSpace(path.Base(u.Path)))
	if name == "" || name == "." || name == "/" {
		name = "image" + visionExt(ct)
	}
	return &visionImage{data: data, filename: name, contentType: ct}, nil
}

// uploadVisionImage POSTs bytes as multipart field "file" and returns id.
func uploadVisionImage(ctx context.Context, token string, img *visionImage) (string, error) {
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	h := make(textproto.MIMEHeader)
	h.Set("Content-Disposition", fmt.Sprintf(`form-data; name="file"; filename=%q`, img.filename))
	if img.contentType != "" {
		h.Set("Content-Type", img.contentType)
	}
	part, err := mw.CreatePart(h)
	if err != nil {
		return "", fmt.Errorf("upload build: %w", err)
	}
	if _, err := part.Write(img.data); err != nil {
		return "", fmt.Errorf("upload write: %w", err)
	}
	if err := mw.Close(); err != nil {
		return "", fmt.Errorf("upload close: %w", err)
	}
	uctx, cancel := context.WithTimeout(ctx, visionUpTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(uctx, "POST", session.BaseURL+"/api/v1/files/", &body)
	if err != nil {
		return "", fmt.Errorf("upload request: %w", err)
	}
	req.Header.Set("authorization", "Bearer "+token)
	req.Header.Set("User-Agent", session.ChromeUA)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", mw.FormDataContentType())
	resp, err := session.SharedClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("upload connection: %w", err)
	}
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	resp.Body.Close()
	switch {
	case resp.StatusCode == 401:
		return "", fmt.Errorf("file upload unauthorized (401) — vision needs a logged-in ZAI_TOKEN")
	case resp.StatusCode < 200 || resp.StatusCode >= 300:
		return "", fmt.Errorf("file upload status %d: %s", resp.StatusCode, truncateVision(raw, 200))
	}
	var obj map[string]interface{}
	if json.Unmarshal(raw, &obj) != nil {
		return "", fmt.Errorf("file upload bad JSON")
	}
	id, _ := obj["id"].(string)
	if id == "" {
		return "", fmt.Errorf("file upload response missing id")
	}
	log.Printf("[vision] uploaded %s (%d bytes) → %s", img.filename, len(img.data), shortVisionID(id))
	return id, nil
}

func visionExt(mime string) string {
	switch strings.ToLower(strings.TrimSpace(mime)) {
	case "image/png":
		return ".png"
	case "image/jpeg", "image/jpg":
		return ".jpg"
	case "image/gif":
		return ".gif"
	case "image/webp":
		return ".webp"
	case "image/bmp":
		return ".bmp"
	case "image/svg+xml":
		return ".svg"
	case "image/avif":
		return ".avif"
	}
	return ".bin"
}

func sanitizeFilename(s string) string {
	var sb strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' || r == '.':
			sb.WriteRune(r)
		default:
			sb.WriteRune('_')
		}
	}
	out := sb.String()
	if len(out) > 100 {
		out = out[:100]
	}
	return out
}

func lenientB64(s string) ([]byte, error) {
	for _, enc := range []*base64.Encoding{
		base64.RawURLEncoding, base64.RawStdEncoding,
		base64.URLEncoding, base64.StdEncoding,
	} {
		if b, err := enc.DecodeString(s); err == nil {
			return b, nil
		}
	}
	return nil, fmt.Errorf("base64 decode failed")
}

func truncateVision(b []byte, n int) string {
	s := string(b)
	for len(s) > n {
		s = s[:len(s)-1]
		if utf8.ValidString(s) {
			break
		}
	}
	if len(string(b)) > len(s) {
		return s + "..."
	}
	return s
}

func shortVisionID(id string) string {
	if len(id) > 8 {
		return id[:8] + "..."
	}
	return id
}
