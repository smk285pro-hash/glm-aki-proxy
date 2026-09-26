// Package session keeps one live Z.AI identity: either the login JWT
// from ZAI_TOKEN or a freshly minted guest token. It also scrapes the
// frontend version string and computes the per-request HMAC signature
// the chat endpoint requires.
package session

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/cookiejar"
	"regexp"
	"strings"
	"sync"
	"time"

	fhttp "github.com/bogdanfinn/fhttp"
	"glm-aki-proxy/internal/util"
)

// BaseURL is the Z.AI web origin. Kept as a var so tests can redirect it.
var BaseURL = "https://chat.z.ai"

// ChromeUA is the desktop browser identity sent on every upstream call.
const ChromeUA = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/146.0.0.0 Safari/537.36"

var defaultJar, _ = cookiejar.New(nil)

// SharedClient is the single tuned transport for chat.z.ai traffic:
// connection reuse across session/captcha/chat/vision calls with
// consistent timeouts and automatic cookie management (acw_tc, cdn_sec_tc).
var SharedClient = &http.Client{
	Jar: defaultJar,
	Transport: &http.Transport{
		Proxy:               http.ProxyFromEnvironment,
		MaxIdleConns:        100,
		MaxIdleConnsPerHost: 10,
		IdleConnTimeout:     90 * time.Second,
		TLSHandshakeTimeout: 10 * time.Second,
	},
	CheckRedirect: func(req *http.Request, via []*http.Request) error {
		return http.ErrUseLastResponse
	},
}

// FHTTPDoer represents an HTTP client capable of executing fhttp.Request.
type FHTTPDoer interface {
	Do(req *fhttp.Request) (*fhttp.Response, error)
}

var (
	sharedDoer        FHTTPDoer
	sharedDoerMu      sync.RWMutex
	clientRefresher   func()
	clientRefresherMu sync.Mutex
)

// RegisterClientRefresher sets a callback to rebuild or reset the fingerprinted TLS client.
func RegisterClientRefresher(fn func()) {
	clientRefresherMu.Lock()
	defer clientRefresherMu.Unlock()
	clientRefresher = fn
}

// RegisterSharedDoer sets the TLS fingerprinted client for session operations.
func RegisterSharedDoer(d FHTTPDoer) {
	sharedDoerMu.Lock()
	defer sharedDoerMu.Unlock()
	sharedDoer = d
}

// GetSharedDoer returns the registered TLS client, if any.
func GetSharedDoer() FHTTPDoer {
	sharedDoerMu.RLock()
	defer sharedDoerMu.RUnlock()
	return sharedDoer
}

func doRequest(ctx context.Context, method, urlStr string, body io.Reader, headers map[string]string) (int, []byte, error) {
	if doer := GetSharedDoer(); doer != nil {
		req, err := fhttp.NewRequestWithContext(ctx, method, urlStr, body)
		if err != nil {
			return 0, nil, err
		}
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		resp, err := doer.Do(req)
		if err != nil {
			return 0, nil, err
		}
		defer resp.Body.Close()
		data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		return resp.StatusCode, data, err
	}

	req, err := http.NewRequestWithContext(ctx, method, urlStr, body)
	if err != nil {
		return 0, nil, err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := SharedClient.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return resp.StatusCode, data, err
}

// signSalt is the public HMAC salt the web frontend uses for signatures.
const signSalt = "key-@@@@)))()((9))-xxxx&&&%%%%%"

// fallbackFEVersion is used when the homepage scrape fails.
const fallbackFEVersion = "prod-fe-1.1.96"

var fePattern = regexp.MustCompile(`prod-fe-\d+\.\d+\.\d+`)

// Session is the live upstream identity (safe for concurrent use).
type Session struct {
	mu            sync.Mutex
	token         string
	userID        string
	userName      string
	fe            string
	ready         bool
	cooldownUntil time.Time
}

func (s *Session) Name() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.userName
}

func (s *Session) ShortUID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return util.ShortToken(s.userID)
}

func (s *Session) InCooldown() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return time.Now().Before(s.cooldownUntil)
}

// New returns an empty session; call Init before use.
func New() *Session {
	return &Session{userName: "Guest", fe: fallbackFEVersion}
}

// Pool manages one or more live Z.AI account sessions with round-robin
// distribution, failure detection, and automatic failover.
type Pool struct {
	mu       sync.Mutex
	sessions []*Session
	idx      int
	fe       string
}

// NewPool returns an empty account pool.
func NewPool() *Pool {
	return &Pool{fe: fallbackFEVersion}
}

// Init authenticates all provided login tokens into the pool.
// If tokens is empty or all tokens fail, falls back to a guest session.
func (p *Pool) Init(tokens []string) error {
	p.scrapeFE()
	var valid []*Session
	for _, tok := range tokens {
		tok = strings.TrimSpace(tok)
		if tok == "" {
			continue
		}
		fe := p.FE()
		s := &Session{userName: "Account", fe: fe}
		if s.activate(tok, "login") {
			valid = append(valid, s)
		} else {
			log.Printf("[session] login token invalid or rejected: %.15s...", tok)
		}
	}
	if len(valid) == 0 {
		if len(tokens) > 0 {
			log.Printf("[session] all login tokens rejected, falling back to guest")
		}
		guest := &Session{userName: "Guest", fe: p.FE()}
		if err := guest.initGuest(); err != nil {
			return err
		}
		valid = append(valid, guest)
	}
	p.mu.Lock()
	p.sessions = valid
	p.mu.Unlock()
	log.Printf("[session] account pool ready: %d active session(s)", len(valid))
	return nil
}

// scrapeFE scrapes the frontend version and seeds the CookieJar with acw_tc/cdn_sec_tc cookies.
func (p *Pool) scrapeFE() {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	hdr := map[string]string{
		"User-Agent":                ChromeUA,
		"Accept":                    "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,image/apng,*/*;q=0.8",
		"Accept-Language":           "vi-VN,vi;q=0.9,en-US;q=0.8,en;q=0.7",
		"Sec-Ch-Ua":                 `"Chromium";v="146", "Not A(Brand";v="24", "Google Chrome";v="146"`,
		"Sec-Ch-Ua-Mobile":          "?0",
		"Sec-Ch-Ua-Platform":        `"Windows"`,
		"Sec-Fetch-Dest":            "document",
		"Sec-Fetch-Mode":            "navigate",
		"Sec-Fetch-Site":            "none",
		"Sec-Fetch-User":            "?1",
		"Upgrade-Insecure-Requests": "1",
	}
	_, body, err := doRequest(ctx, "GET", BaseURL, nil, hdr)
	if err != nil {
		log.Printf("[session] scrapeFE error: %v", err)
		return
	}
	if m := fePattern.Find(body); m != nil {
		p.mu.Lock()
		p.fe = string(m)
		for _, s := range p.sessions {
			if s != nil {
				s.mu.Lock()
				s.fe = p.fe
				s.mu.Unlock()
			}
		}
		p.mu.Unlock()
		log.Printf("[session] frontend version: %s (cookies seeded)", m)
	}
}

// FE returns the current frontend version string safely under mutex.
func (p *Pool) FE() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.fe
}

// Refresh re-scrapes the frontend version and refreshes anti-bot cookies in the CookieJar.
func (p *Pool) Refresh() {
	if jar, err := cookiejar.New(nil); err == nil {
		SharedClient.Jar = jar
	}
	clientRefresherMu.Lock()
	refresher := clientRefresher
	clientRefresherMu.Unlock()
	if refresher != nil {
		refresher()
	}
	p.scrapeFE()
}

// TotalCount returns total number of accounts in the pool.
func (p *Pool) TotalCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.sessions)
}

// HealthyCount returns the number of accounts currently not in cooldown.
func (p *Pool) HealthyCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	count := 0
	for _, s := range p.sessions {
		s.mu.Lock()
		if !now.Before(s.cooldownUntil) && s.ready {
			count++
		}
		s.mu.Unlock()
	}
	return count
}

// ReadyCount returns the number of accounts currently ready (not marked fatal).
func (p *Pool) ReadyCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	count := 0
	for _, s := range p.sessions {
		s.mu.Lock()
		if s.ready {
			count++
		}
		s.mu.Unlock()
	}
	return count
}

// ResetCapacityCooldown clears cooldown for all ready accounts so they can be tried with a fallback model.
func (p *Pool) ResetCapacityCooldown() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, s := range p.sessions {
		s.mu.Lock()
		if s.ready {
			s.cooldownUntil = time.Time{}
		}
		s.mu.Unlock()
	}
}

// Pick returns the next healthy session via round-robin.
// If all sessions are in cooldown, it returns the one exiting cooldown earliest.
func (p *Pool) Pick() *Session {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.sessions) == 0 {
		return nil
	}
	if len(p.sessions) == 1 {
		return p.sessions[0]
	}

	now := time.Now()
	n := len(p.sessions)
	for i := 0; i < n; i++ {
		s := p.sessions[(p.idx+i)%n]
		s.mu.Lock()
		inCd := now.Before(s.cooldownUntil)
		ready := s.ready
		s.mu.Unlock()
		if !inCd && ready {
			p.idx = (p.idx + i + 1) % n
			return s
		}
	}

	// All are in cooldown: pick the one that exits cooldown earliest
	var best *Session
	var minCd time.Time
	for _, s := range p.sessions {
		s.mu.Lock()
		cd := s.cooldownUntil
		s.mu.Unlock()
		if best == nil || cd.Before(minCd) {
			best = s
			minCd = cd
		}
	}
	p.idx = (p.idx + 1) % n
	return best
}

// HasAlternative reports whether there is another healthy session available.
func (p *Pool) HasAlternative(current *Session) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.sessions) <= 1 {
		return false
	}
	now := time.Now()
	for _, s := range p.sessions {
		if s == current {
			continue
		}
		s.mu.Lock()
		inCd := now.Before(s.cooldownUntil)
		ready := s.ready
		s.mu.Unlock()
		if !inCd && ready {
			return true
		}
	}
	return false
}

// ReportError marks a session in cooldown based on the error.
func (p *Pool) ReportError(s *Session, err error) {
	if s == nil || err == nil {
		return
	}
	msg := strings.ToLower(err.Error())
	uid := s.ShortUID()
	if strings.Contains(msg, "waf block") || strings.Contains(msg, "405") {
		// Do not set cooldown: let pool rotate cyclically across accounts
		log.Printf("[session] account %s encountered WAF 405, cycling to next account without cooldown", uid)
	} else if strings.Contains(msg, "401") || strings.Contains(msg, "token expired") {
		s.mu.Lock()
		s.ready = false
		s.cooldownUntil = time.Now().Add(24 * time.Hour)
		s.mu.Unlock()
		log.Printf("[session] account %s token expired (401), disabled for 24h", uid)
	} else if strings.Contains(msg, "user_blocked") || strings.Contains(msg, "blocked") {
		s.mu.Lock()
		s.ready = false
		s.cooldownUntil = time.Now().Add(24 * time.Hour)
		s.mu.Unlock()
		log.Printf("[session] account %s BLOCKED by upstream (USER_BLOCKED), disabled for 24h", uid)
	} else if strings.Contains(msg, "capacity") || strings.Contains(msg, "busy") || strings.Contains(msg, "rate limit") || strings.Contains(msg, "too many requests") || strings.Contains(msg, "429") {
		s.mu.Lock()
		s.cooldownUntil = time.Now().Add(15 * time.Second)
		s.mu.Unlock()
		log.Printf("[session] account %s entered capacity cooldown (15s)", uid)
	}
}

// ReportFatalError immediately disables the session for 24h on fatal upstream rejection.
func (p *Pool) ReportFatalError(s *Session, err error) {
	if s == nil {
		return
	}
	uid := s.ShortUID()
	s.mu.Lock()
	s.ready = false
	s.cooldownUntil = time.Now().Add(24 * time.Hour)
	s.mu.Unlock()
	log.Printf("[session] account %s marked fatal (disabled 24h): %v", uid, err)
}

// MarkFatal immediately marks the session as unusable for 24h.
func (s *Session) MarkFatal() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ready = false
	s.cooldownUntil = time.Now().Add(24 * time.Hour)
}

// Snapshot returns summary for /status.
func (p *Pool) Snapshot() (token, userID, userName, fe string, ready bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.sessions) == 0 {
		return "", "", "none", p.fe, false
	}
	s := p.sessions[0]
	token, userID, userName, fe, ready = s.Snapshot()
	if len(p.sessions) > 1 {
		userName = fmt.Sprintf("%s (%d accounts)", userName, len(p.sessions))
	}
	return token, userID, userName, fe, ready
}

// Init authenticates: login token when valid, otherwise a guest token.
// It also refreshes the frontend version string.
func (s *Session) Init(loginToken string) error {
	s.scrapeFE()
	if strings.TrimSpace(loginToken) != "" {
		if s.activate(loginToken, "login") {
			return nil
		}
		log.Printf("[session] login token rejected, falling back to guest")
	}
	return s.initGuest()
}

// activate stores token + decodes its identity. ok=false when unusable.
func (s *Session) activate(token, kind string) (ok bool) {
	claims, err := decodeClaims(token)
	if err != nil {
		return false
	}
	uid, _ := claims["id"].(string)
	if uid == "" {
		return false
	}
	if kind == "login" && !checkLoginToken(token) {
		return false
	}
	name := "Guest"
	if email, _ := claims["email"].(string); email != "" {
		if at := strings.IndexByte(email, '@'); at > 0 {
			name = email[:at]
		}
	}
	s.mu.Lock()
	s.token, s.userID, s.userName, s.ready = token, uid, name, true
	s.mu.Unlock()
	log.Printf("[session] %s ready as %s (%s)", kind, name, util.ShortToken(uid))
	return true
}

// initGuest mints an anonymous token: warm-up POST then the GET that
// returns {"token": "..."}.
func (s *Session) initGuest() error {
	hdr := map[string]string{
		"Origin":       BaseURL,
		"Referer":      BaseURL + "/",
		"Content-Type": "application/json",
		"User-Agent":   ChromeUA,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_ = postJSON(ctx, BaseURL+"/api/v1/auths/guest", "{}", hdr)

	token := ""
	if t, err := getToken(ctx, hdr); err == nil {
		token = t
	}
	if token == "" {
		return fmt.Errorf("session: guest token request failed")
	}
	if !s.activate(token, "guest") {
		return fmt.Errorf("session: guest token unusable")
	}
	return nil
}

// Snapshot returns a consistent copy of the live identity.
func (s *Session) Snapshot() (token, userID, userName, fe string, ready bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.token, s.userID, s.userName, s.fe, s.ready
}

// Sign builds the X-Signature value for one chat request carrying prompt.
func (s *Session) Sign(prompt string) (sig, requestID string) {
	now := time.Now().UnixMilli()
	ts := fmt.Sprintf("%d", now)
	requestID = util.UUIDv4()
	bucket := fmt.Sprintf("%d", now/300000)

	mac := hmac.New(sha256.New, []byte(signSalt))
	mac.Write([]byte(bucket))
	roundKey := hex.EncodeToString(mac.Sum(nil))

	info := "requestId," + requestID + ",timestamp," + ts + ",user_id," + s.currentUserID()
	promptB64 := util.B64Std([]byte(strings.TrimSpace(prompt)))
	mac2 := hmac.New(sha256.New, []byte(roundKey))
	mac2.Write([]byte(info + "|" + promptB64 + "|" + ts))
	return hex.EncodeToString(mac2.Sum(nil)), requestID
}

func (s *Session) currentUserID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.userID
}

// scrapeFE refreshes the frontend version from the homepage HTML.
func (s *Session) scrapeFE() {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	hdr := map[string]string{
		"User-Agent":                ChromeUA,
		"Accept":                    "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,image/apng,*/*;q=0.8",
		"Accept-Language":           "vi-VN,vi;q=0.9,en-US;q=0.8,en;q=0.7",
		"Sec-Ch-Ua":                 `"Chromium";v="146", "Not A(Brand";v="24", "Google Chrome";v="146"`,
		"Sec-Ch-Ua-Mobile":          "?0",
		"Sec-Ch-Ua-Platform":        `"Windows"`,
		"Sec-Fetch-Dest":            "document",
		"Sec-Fetch-Mode":            "navigate",
		"Sec-Fetch-Site":            "none",
		"Sec-Fetch-User":            "?1",
		"Upgrade-Insecure-Requests": "1",
	}
	_, body, err := doRequest(ctx, "GET", BaseURL, nil, hdr)
	if err != nil {
		return
	}
	if m := fePattern.Find(body); m != nil {
		s.mu.Lock()
		s.fe = string(m)
		s.mu.Unlock()
		log.Printf("[session] frontend version: %s (cookies seeded)", m)
	}
}

// decodeClaims base64url-decodes a JWT payload without verifying it.
func decodeClaims(token string) (map[string]interface{}, error) {
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return nil, fmt.Errorf("not a jwt")
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, err
	}
	var claims map[string]interface{}
	if err := json.Unmarshal(raw, &claims); err != nil {
		return nil, err
	}
	return claims, nil
}

// checkLoginToken validates a login JWT against the auth endpoint.
func checkLoginToken(token string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	hdr := map[string]string{
		"Authorization": "Bearer " + token,
		"User-Agent":    ChromeUA,
	}
	status, _, err := doRequest(ctx, "GET", BaseURL+"/api/v1/auths/", nil, hdr)
	if err != nil {
		return false
	}
	return status == http.StatusOK
}

// getToken performs the anonymous token GET.
func getToken(ctx context.Context, hdr map[string]string) (string, error) {
	status, body, err := doRequest(ctx, "GET", BaseURL+"/api/v1/auths/", nil, hdr)
	if err != nil {
		return "", err
	}
	if status != http.StatusOK {
		return "", fmt.Errorf("status %d", status)
	}
	var out struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return "", err
	}
	return out.Token, nil
}

// postJSON is a small JSON POST helper returning the raw body.
func postJSON(ctx context.Context, url, body string, hdr map[string]string) string {
	_, data, _ := doRequest(ctx, "POST", url, strings.NewReader(body), hdr)
	return string(data)
}
