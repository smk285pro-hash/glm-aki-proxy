// Package upstream speaks the Z.AI chat protocol: request signing,
// captcha attachment, and the stateful SSE dialect (edit_index
// rewrites plus <details> reasoning blocks).
package upstream

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"time"
	"unicode/utf16"
	"unicode/utf8"

	fhttp "github.com/bogdanfinn/fhttp"

	"glm-aki-proxy/internal/captcha"
	"glm-aki-proxy/internal/session"
	"glm-aki-proxy/internal/util"
)

// DefaultModel is used when the client sends an empty model id.
const DefaultModel = "glm-4.7"

// FallbackModel is used when accounts are exhausted/in cooldown on the requested model.
const FallbackModel = "glm-5.2"

var solveCaptcha = captcha.Solve

// KnownModels advertises what this bridge will forward.
var KnownModels = []string{
	"claude-3-7-sonnet-20250219",
	"claude-3-7-sonnet",
	"claude-3-5-sonnet-20241022",
	"claude-3-5-sonnet",
	"claude-3-5-haiku-20241022",
	"claude-3-opus-20240229",
	"claude-3-opus",
	"claude-opus-4-8",
	"claude-opus-4.8",
	"claude-opus-4-8[1m]",
	"claude-opus-4-6",
	"claude-sonnet-4-6",
	"claude-opus-gl-5.3",
	"claude-sonnet-gl-5.2",
	"claude-haiku-gl-4.7",
	"claude-sonnet-gl-5-turbo",
	"claude-sonnet-gl-5v-turbo",
	"glm-5.3", "glm-5.2", "glm-4.7",
	"GLM-5-Turbo", "GLM-5v-Turbo", "x-preview-l",
}

// NormalizeModel trims the id and falls back to the default.
// Unknown ids are forwarded as-is; Z.AI decides whether they exist.
func NormalizeModel(m string) string {
	if strings.TrimSpace(m) == "" {
		return DefaultModel
	}
	return strings.TrimSpace(m)
}

// FlattenPrompt concatenates message texts for signature computation.
// The wire messages themselves are forwarded untouched.
func FlattenPrompt(msgs []json.RawMessage) string {
	var sb strings.Builder
	for _, raw := range msgs {
		var msg struct {
			Content json.RawMessage `json:"content"`
		}
		if json.Unmarshal(raw, &msg) != nil {
			continue
		}
		var s string
		if json.Unmarshal(msg.Content, &s) == nil {
			sb.WriteString(s)
			sb.WriteString("\n\n")
			continue
		}
		var parts []map[string]interface{}
		if json.Unmarshal(msg.Content, &parts) == nil {
			for _, p := range parts {
				if t, _ := p["type"].(string); t == "" || t == "text" {
					if txt, _ := p["text"].(string); txt != "" {
						sb.WriteString(txt)
						sb.WriteString("\n")
					}
				}
			}
			sb.WriteString("\n")
		}
	}
	return strings.TrimSpace(sb.String())
}

type chatBody struct {
	Model              string                   `json:"model"`
	ChatID             string                   `json:"chat_id"`
	Messages           []json.RawMessage        `json:"messages"`
	SignaturePrompt    string                   `json:"signature_prompt"`
	Stream             bool                     `json:"stream"`
	CaptchaVerifyParam string                   `json:"captcha_verify_param"`
	Features           map[string]interface{}   `json:"features"`
	Files              []map[string]interface{} `json:"files,omitempty"`
}

// Sentinel errors for upstream request classification.
var (
	ErrUserBlocked   = errors.New("upstream: account blocked (USER_BLOCKED)")
	ErrUnauthorized  = errors.New("upstream: 401 unauthorized — login token expired")
	ErrWAFBlock      = errors.New("upstream: WAF block")
	ErrStreamCut     = errors.New("upstream: stream cut before done")
	ErrStreamStalled = errors.New("upstream: stream stalled")
)

// OverloadedError models HTTP 529 in Anthropic Messages error JSON format.
type OverloadedError struct {
	RetryAfter int
	Message    string
}

func (e *OverloadedError) Error() string {
	if e.Message != "" {
		return e.Message
	}
	return "upstream: account pool overloaded / in cooldown"
}

func (e *OverloadedError) StatusCode() int {
	return 529
}

func (e *OverloadedError) RetryAfterSeconds() int {
	if e.RetryAfter > 0 {
		return e.RetryAfter
	}
	return 15
}

func (e *OverloadedError) AnthropicJSON() []byte {
	return []byte(`{"type":"error","error":{"type":"overloaded_error","message":"Overloaded: upstream accounts in cooldown or rate limited. Please retry in 15 seconds."}}`)
}

func (e *OverloadedError) Is(target error) bool {
	if _, ok := target.(*OverloadedError); ok {
		return true
	}
	return target == ErrPoolOverloaded
}

// ErrPoolOverloaded is returned when all tokens in the account pool are in cooldown or overloaded.
var ErrPoolOverloaded = &OverloadedError{
	RetryAfter: 15,
	Message:    "upstream: account pool overloaded / in cooldown",
}

// WriteAnthropicOverloaded writes HTTP 529 with Retry-After: 15 in Anthropic Messages JSON format.
func WriteAnthropicOverloaded(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Retry-After", "15")
	w.WriteHeader(529)
	w.Write(ErrPoolOverloaded.AnthropicJSON())
}

// isTransient reports whether an error is temporary and can be solved by retry or failover.
func isTransient(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, ErrUserBlocked) {
		return false
	}
	if errors.Is(err, ErrPoolOverloaded) || errors.Is(err, ErrWAFBlock) || errors.Is(err, ErrUnauthorized) || errors.Is(err, ErrStreamCut) || errors.Is(err, ErrStreamStalled) {
		return true
	}
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "capacity") ||
		strings.Contains(s, "busy") ||
		strings.Contains(s, "rate limit") ||
		strings.Contains(s, "too many requests") ||
		strings.Contains(s, "overloaded") ||
		strings.Contains(s, "try again") ||
		strings.Contains(s, "timeout") ||
		strings.Contains(s, "stream stalled") ||
		strings.Contains(s, "connection error") ||
		strings.Contains(s, "waf block") ||
		strings.Contains(s, "status 405") ||
		strings.Contains(s, "status 429") ||
		strings.Contains(s, "status 500") ||
		strings.Contains(s, "status 502") ||
		strings.Contains(s, "status 503") ||
		strings.Contains(s, "status 504") ||
		strings.Contains(s, "internal server error")
}

// Chat sends one completion request. onText/onReason receive live deltas
// (either may be nil); the returned strings are authoritative.
func Chat(ctx context.Context, pool *session.Pool, take captcha.TokenTaker,
	model string, msgs []json.RawMessage,
	onText, onReason func(string), verbose bool, opts ChatOpts) (string, string, error) {

	fallbackDone := false
	attemptsForModel := 0

	// If all accounts are in cooldown, check if we can auto-fallback to FallbackModel
	if pool.TotalCount() > 0 && pool.HealthyCount() == 0 {
		if pool.ReadyCount() > 0 && !strings.EqualFold(model, FallbackModel) {
			log.Printf("[upstream] all %d account(s) in cooldown on model %s, auto-falling back to %s", pool.TotalCount(), model, FallbackModel)
			model = FallbackModel
			pool.ResetCapacityCooldown()
			fallbackDone = true
		} else {
			log.Printf("[upstream] all %d accounts in cooldown, returning ErrPoolOverloaded (529)", pool.TotalCount())
			return "", "", ErrPoolOverloaded
		}
	}

	prompt := FlattenPrompt(msgs)
	var lastErr error
	maxAttempts := 4
	if tc := pool.TotalCount(); tc*2 > maxAttempts {
		maxAttempts = tc * 2
	}
	if maxAttempts > 6 {
		maxAttempts = 6
	}

	for attempt := 0; attempt < maxAttempts; attempt++ {
		if pool.TotalCount() > 0 && pool.HealthyCount() == 0 {
			if !fallbackDone && !strings.EqualFold(model, FallbackModel) && pool.ReadyCount() > 0 {
				fallbackDone = true
				log.Printf("[upstream] all accounts in cooldown for model %s, auto-falling back to %s", model, FallbackModel)
				model = FallbackModel
				pool.ResetCapacityCooldown()
				attemptsForModel = 0
				if attempt+pool.TotalCount() >= maxAttempts {
					maxAttempts = attempt + pool.TotalCount() + 1
					if maxAttempts > 10 {
						maxAttempts = 10
					}
				}
			} else {
				log.Printf("[upstream] all accounts exhausted/in cooldown during attempt %d/%d, returning ErrPoolOverloaded", attempt+1, maxAttempts)
				return "", "", ErrPoolOverloaded
			}
		}

		sess := pool.Pick()
		if sess == nil {
			return "", "", fmt.Errorf("no active upstream account available")
		}

		if hasVisionParts(msgs) && !isVisionModel(model) {
			log.Printf("[vision] model=%s is not the vision model; upstream decides what to do with the images", model)
		}
		vmsgs, files, err := prepareVision(ctx, sess, msgs)
		if err != nil {
			return "", "", err
		}
		opts.Files = files

		// Direct-First policy: attempt 0 connects direct; subsequent attempts fallback to proxy pool
		var routeProxy *url.URL
		if attempt > 0 && DefaultProxyPool.Enabled() {
			routeProxy = DefaultProxyPool.CurrentURL()
		}

		// Pre-check proxy route via GET /api/models before burning single-use captcha device token
		if routeProxy != nil {
			token, _, _, _, _ := sess.Snapshot()
			if err := DefaultProxyPool.PreCheck(ctx, routeProxy, token); err != nil {
				DefaultProxyPool.MarkDead(routeProxy)
				log.Printf("[upstream] proxy %s failed pre-check (%v), marked dead and rotated", routeProxy.Redacted(), err)
				DefaultProxyPool.Rotate()
				if attempt < maxAttempts-1 {
					continue
				}
				return "", "", fmt.Errorf("upstream: proxy pre-check failed: %w", err)
			}
		}

		param := solveCaptcha(take, verbose)
		if param == "" {
			lastErr = fmt.Errorf("captcha failed — refill the device-token pool")
			log.Printf("[upstream] captcha solve failed on attempt %d/%d", attempt+1, maxAttempts)
			if attempt < maxAttempts-1 {
				select {
				case <-ctx.Done():
					return "", "", ctx.Err()
				case <-time.After(time.Duration(attempt+1) * 800 * time.Millisecond):
				}
				continue
			}
			return "", "", lastErr
		}

		text, reason, retry, err := roundTrip(ctx, sess, model, vmsgs, prompt, param, onText, onReason, verbose, opts, routeProxy)
		if err == nil {
			return text, reason, nil
		}

		// Catch USER_BLOCKED cleanly via errors.Is (fixing the reference repo's == pointer bug)
		if errors.Is(err, ErrUserBlocked) {
			log.Printf("[upstream] account %s was BLOCKED by upstream, marking fatal and switching immediately", sess.Name())
			pool.ReportFatalError(sess, err)
			if pool.HasAlternative(sess) {
				continue
			}
			if !fallbackDone && !strings.EqualFold(model, FallbackModel) && pool.ReadyCount() > 0 {
				fallbackDone = true
				log.Printf("[upstream] account blocked, auto-falling back to %s for remaining accounts", FallbackModel)
				model = FallbackModel
				pool.ResetCapacityCooldown()
				attemptsForModel = 0
				continue
			}
			return "", "", ErrPoolOverloaded
		}

		if errors.Is(err, ErrWAFBlock) {
			if routeProxy != nil {
				DefaultProxyPool.MarkDead(routeProxy)
				log.Printf("[upstream] proxy %s hit WAF block, marked dead", routeProxy.Redacted())
				DefaultProxyPool.Rotate()
			} else if DefaultProxyPool.Enabled() {
				log.Printf("[upstream] direct route hit WAF block, falling back to proxy pool on retry")
			}
		}

		pool.ReportError(sess, err)
		lastErr = err
		attemptsForModel++

		// Check if we have completed a full round over accounts or all accounts entered cooldown
		completedRound := attemptsForModel >= pool.TotalCount() || (pool.TotalCount() > 0 && pool.HealthyCount() == 0)
		if completedRound && !fallbackDone && !strings.EqualFold(model, FallbackModel) && pool.ReadyCount() > 0 {
			fallbackDone = true
			log.Printf("[upstream] rotated all %d account(s) on model %s without success, auto-falling back to %s", pool.TotalCount(), model, FallbackModel)
			model = FallbackModel
			pool.ResetCapacityCooldown()
			attemptsForModel = 0
			if attempt+pool.TotalCount() >= maxAttempts {
				maxAttempts = attempt + pool.TotalCount() + 1
				if maxAttempts > 10 {
					maxAttempts = 10
				}
			}
			continue
		}

		if pool.TotalCount() > 0 && pool.HealthyCount() == 0 {
			return "", "", ErrPoolOverloaded
		}

		if !retry && !isTransient(err) {
			return "", "", err
		}

		if attempt < maxAttempts-1 {
			isWaf := errors.Is(err, ErrWAFBlock) || strings.Contains(strings.ToLower(err.Error()), "waf block") || strings.Contains(err.Error(), "405")
			if isWaf {
				backoff := time.Duration(attempt+1) * 2000 * time.Millisecond
				log.Printf("[upstream] account %s hit WAF block (%v), waiting %v before retry...", sess.Name(), err, backoff)
				pool.Refresh()
				select {
				case <-ctx.Done():
					return "", "", ctx.Err()
				case <-time.After(backoff):
				}
			} else if pool.HasAlternative(sess) {
				log.Printf("[upstream] account %s hit transient error (%v), switching to alternative account", sess.Name(), err)
			} else {
				select {
				case <-ctx.Done():
					return "", "", ctx.Err()
				case <-time.After(time.Duration(attempt+1) * 1200 * time.Millisecond):
				}
			}
		}
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("upstream: request failed")
	}
	return "", "", lastErr
}

// hasVisionParts reports whether any message carries image_url parts.
func hasVisionParts(msgs []json.RawMessage) bool {
	for _, raw := range msgs {
		var m map[string]interface{}
		if json.Unmarshal(raw, &m) != nil {
			continue
		}
		cr, _ := json.Marshal(m["content"])
		var parts []map[string]interface{}
		if json.Unmarshal(cr, &parts) != nil {
			continue
		}
		for _, p := range parts {
			if t, _ := p["type"].(string); t == "image_url" {
				return true
			}
		}
	}
	return false
}

// isVisionModel reports whether a model is expected to see images.
func isVisionModel(model string) bool {
	return strings.EqualFold(strings.TrimSpace(model), "GLM-5v-Turbo")
}

// roundTrip performs a single POST using Chrome 146 TLS client and pacing gate.
func roundTrip(ctx context.Context, sess *session.Session, model string,
	msgs []json.RawMessage, prompt, captchaParam string,
	onText, onReason func(string), verbose bool, opts ChatOpts, routeProxy *url.URL) (string, string, bool, error) {

	token, userID, _, fe, ready := sess.Snapshot()
	_ = userID
	if !ready || token == "" {
		return "", "", false, fmt.Errorf("upstream: session not ready")
	}
	sig, _ := sess.Sign(prompt)

	features := map[string]interface{}{
		"image_generation": false,
		"web_search":       false,
		"auto_web_search":  false,
		"preview_mode":     true,
		"enable_thinking":  true,
	}
	applyOpts(features, model, opts)

	body := chatBody{
		Model:              model,
		ChatID:             util.UUIDv4(),
		Messages:           msgs,
		SignaturePrompt:    prompt,
		Stream:             true,
		CaptchaVerifyParam: captchaParam,
		Features:           features,
		Files:              opts.Files,
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return "", "", false, err
	}

	proxyStr := ""
	if routeProxy != nil {
		proxyStr = routeProxy.String()
	}
	client, err := GetBrowserClient(proxyStr, 120)
	if err != nil {
		return "", "", false, fmt.Errorf("upstream tls client: %w", err)
	}

	req, err := fhttp.NewRequestWithContext(ctx, "POST",
		session.BaseURL+"/api/v2/chat/completions", bytes.NewReader(raw))
	if err != nil {
		return "", "", false, err
	}
	req.Header = make(fhttp.Header)
	req.Header.Set("authorization", "Bearer "+token)
	req.Header.Set("user-agent", ChromeUA)
	req.Header.Set("content-type", "application/json")
	req.Header.Set("accept", "text/event-stream, application/json, */*")
	req.Header.Set("accept-language", "vi-VN,vi;q=0.9,en-US;q=0.8,en;q=0.7")
	req.Header.Set("origin", session.BaseURL)
	req.Header.Set("referer", session.BaseURL+"/")
	req.Header.Set("sec-ch-ua", `"Chromium";v="146", "Not A(Brand";v="24", "Google Chrome";v="146"`)
	req.Header.Set("sec-ch-ua-mobile", "?0")
	req.Header.Set("sec-ch-ua-platform", `"Windows"`)
	req.Header.Set("sec-fetch-dest", "empty")
	req.Header.Set("sec-fetch-mode", "cors")
	req.Header.Set("sec-fetch-site", "same-origin")
	req.Header.Set("x-fe-version", fe)
	req.Header.Set("x-region", "overseas")
	req.Header.Set("x-signature", sig)

	// Pacing gate enforcing ANTHROPIC_ZAI_MIN_MS (default 1500ms)
	if err := Throttle(ctx); err != nil {
		return "", "", false, err
	}

	resp, err := client.Do(req)
	if err != nil {
		return "", "", true, fmt.Errorf("upstream: connection error: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusUnauthorized {
		return "", "", true, ErrUnauthorized
	}
	if resp.StatusCode == http.StatusForbidden {
		eb, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		es := strings.TrimSpace(string(eb))
		log.Printf("[upstream] 403 Forbidden from chat.z.ai: %.250s", es)
		if strings.Contains(strings.ToUpper(es), "USER_BLOCKED") || strings.Contains(strings.ToLower(es), "blocked") {
			return "", "", false, fmt.Errorf("%w: %s", ErrUserBlocked, es)
		}
		return "", "", true, fmt.Errorf("%w: status 403: %s", ErrWAFBlock, es)
	}
	if resp.StatusCode == http.StatusMethodNotAllowed {
		return "", "", true, fmt.Errorf("%w: status 405", ErrWAFBlock)
	}
	if resp.StatusCode >= 500 || resp.StatusCode == http.StatusTooManyRequests {
		return "", "", true, fmt.Errorf("upstream: status %d (transient)", resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK {
		eb, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		es := strings.TrimSpace(string(eb))
		log.Printf("[upstream] non-200 from chat.z.ai: status=%d headers=%v body=%.250s", resp.StatusCode, resp.Header, es)
		if strings.Contains(strings.ToLower(es), "<html") {
			return "", "", true, fmt.Errorf("%w: WAF block (status %d)", ErrWAFBlock, resp.StatusCode)
		}
		isTr := isTransient(fmt.Errorf("%s", es))
		return "", "", isTr, fmt.Errorf("upstream: status %d: %s", resp.StatusCode, es)
	}

	p := newParser()
	emitted := false
	wrapText, wrapReason := onText, onReason
	if wrapText != nil {
		outer := wrapText
		wrapText = func(s string) { emitted = true; outer(s) }
	}
	if wrapReason != nil {
		outer := wrapReason
		wrapReason = func(s string) { emitted = true; outer(s) }
	}

	var watchdogFired atomic.Bool
	wd := time.AfterFunc(120*time.Second, func() {
		watchdogFired.Store(true)
		log.Printf("[upstream] stream idle >120s, aborting")
		resp.Body.Close()
	})
	defer wd.Stop()

	reader := bufio.NewReaderSize(resp.Body, 256*1024)
	var carry string
	sawDone := false
	for {
		line, err := reader.ReadString('\n')
		if line != "" {
			wd.Reset(120 * time.Second)
			carry += line
			chunks := strings.Split(carry, "\n")
			carry = chunks[len(chunks)-1]
			for _, l := range chunks[:len(chunks)-1] {
				done, ferr := p.feed(l, wrapText, wrapReason, verbose)
				if ferr != nil {
					retryable := !emitted && isTransient(ferr)
					return "", "", retryable, ferr
				}
				if done {
					sawDone = true
				}
			}
			if sawDone {
				p.flushFinal(wrapText, wrapReason)
				return p.text(), p.reasoning(), false, nil
			}
		}
		if err != nil {
			if err == io.EOF && strings.TrimSpace(carry) != "" {
				done, ferr := p.feed(carry, wrapText, wrapReason, verbose)
				if ferr != nil {
					retryable := !emitted && isTransient(ferr)
					return "", "", retryable, ferr
				}
				if done {
					sawDone = true
				}
				if sawDone {
					p.flushFinal(wrapText, wrapReason)
					return p.text(), p.reasoning(), false, nil
				}
			}
			if watchdogFired.Load() {
				return "", "", true, ErrStreamStalled
			}
			break
		}
	}
	p.flushFinal(wrapText, wrapReason)
	if !emitted {
		return "", "", true, ErrStreamCut
	}
	return "", "", false, fmt.Errorf("upstream: stream cut after partial output")
}

// ── stateful SSE parser ──

// parser accumulates the authoritative full text. Z.AI rewrites already
// sent text in place: content = text[:edit_index] + edit_content, with
// edit_index counted in UTF-16 code units (JS string semantics).
type parser struct {
	full     strings.Builder
	seenText string
	seenWhy  string
}

func newParser() *parser { return &parser{} }

// utf16UnitsToBytes converts a UTF-16 offset into a byte offset,
// clamping inside surrogate pairs to the rune start.
func utf16UnitsToBytes(s string, units int) int {
	if units <= 0 {
		return 0
	}
	bi, u := 0, 0
	for bi < len(s) {
		if u == units {
			return bi
		}
		r, size := utf8.DecodeRuneInString(s[bi:])
		ru := utf16.RuneLen(r)
		if ru < 0 {
			ru = 1
		}
		if u+ru > units {
			return bi
		}
		u += ru
		bi += size
	}
	return len(s)
}

func (p *parser) applyEdit(ec string, idx int) {
	cur := p.full.String()
	if idx < 0 {
		cur += ec
	} else {
		cur = cur[:utf16UnitsToBytes(cur, idx)] + ec
	}
	p.full.Reset()
	p.full.WriteString(cur)
}

// split divides raw text into (answer, reasoning) on <details> blocks.
func (p *parser) split() (string, string) {
	raw := p.full.String()
	open := strings.Index(raw, "<details")
	if open < 0 {
		return raw, ""
	}
	end := strings.Index(raw[open:], ">")
	if end < 0 {
		return raw[:open], ""
	}
	after := raw[open+end+1:]
	if close := strings.Index(after, "</details>"); close >= 0 {
		return raw[:open] + after[close+len("</details>"):], cleanDetails(after[:close])
	}
	return raw[:open], cleanDetails(after)
}

func cleanDetails(s string) string {
	for _, tag := range []string{"<details", "<summary"} {
		if i := strings.Index(s, tag); i >= 0 {
			if e := strings.Index(s[i:], ">"); e >= 0 {
				s = s[:i] + s[i+e+1:]
			}
		}
	}
	s = strings.ReplaceAll(s, "</details>", "")
	s = strings.ReplaceAll(s, "</summary>", "")
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = strings.TrimPrefix(l, "> ")
	}
	return strings.Join(lines, "\n")
}

// text / reasoning return the authoritative trimmed values.
func (p *parser) text() string {
	t, _ := p.split()
	return strings.TrimSpace(t)
}

func (p *parser) reasoning() string {
	_, r := p.split()
	return strings.TrimSpace(r)
}

// emitDelta forwards only the unseen tail; shrunken snapshots (tail
// rewrites) yield nothing instead of duplicating output.
func emitDelta(view *string, target string) string {
	if len(target) < len(*view) {
		return ""
	}
	i := 0
	for i < len(*view) && i < len(target) {
		ra, sa := utf8.DecodeRuneInString((*view)[i:])
		rb, _ := utf8.DecodeRuneInString(target[i:])
		if ra != rb {
			break
		}
		i += sa
	}
	*view = target
	return target[i:]
}

// trimTail holds back n trailing runes while live so tail rewrites stay
// invisible; a dangling "<" fragment is held too.
func trimTail(s string, n int) string {
	i, c := len(s), 0
	for i > 0 && c < n {
		_, size := utf8.DecodeLastRuneInString(s[:i])
		i -= size
		c++
	}
	s = s[:i]
	if j := strings.LastIndexByte(s, '<'); j >= 0 {
		frag := s[j:]
		for _, open := range []string{"<details", "<summary"} {
			if len(frag) <= len(open) && strings.HasPrefix(open, frag) {
				return s[:j]
			}
		}
	}
	if strings.HasSuffix(s, ">") {
		lastNL := strings.LastIndex(s, "\n")
		tail := s[lastNL+1:]
		if strings.TrimSpace(tail) == ">" {
			return s[:lastNL+1]
		}
	}
	return s
}

func (p *parser) flush(tv, rv func(string), live bool) {
	t, r := p.split()
	reasoningDone := strings.Contains(p.full.String(), "</details>")
	if rv != nil {
		if live && !reasoningDone {
			r = trimTail(r, 24)
		}
		if d := emitDelta(&p.seenWhy, r); d != "" {
			rv(d)
		}
	}
	if tv != nil {
		if live {
			t = trimTail(t, 24)
		}
		if d := emitDelta(&p.seenText, t); d != "" {
			tv(d)
		}
	}
}

func (p *parser) flushFinal(tv, rv func(string)) { p.flush(tv, rv, false) }

// inlineErr extracts platform errors delivered inside HTTP 200 bodies.
func inlineErr(obj map[string]interface{}) string {
	for _, key := range []string{"data", ""} {
		var node map[string]interface{}
		if key == "" {
			node = obj
		} else {
			var ok bool
			node, ok = obj[key].(map[string]interface{})
			if !ok {
				continue
			}
		}
		if e, ok := node["error"].(map[string]interface{}); ok {
			if d, _ := e["detail"].(string); d != "" {
				return d
			}
			if m, _ := e["message"].(string); m != "" {
				return m
			}
		}
	}
	return ""
}

// feed consumes one SSE line. done=true on [DONE] or phase "done".
func (p *parser) feed(line string, tv, rv func(string), verbose bool) (done bool, err error) {
	t := strings.TrimSpace(line)
	if t == "" || !strings.HasPrefix(t, "data: ") {
		return false, nil
	}
	payload := t[len("data: "):]
	if payload == "[DONE]" {
		return true, nil
	}
	var obj map[string]interface{}
	if err := json.Unmarshal([]byte(payload), &obj); err != nil {
		if verbose {
			log.Printf("[upstream] skip unparsable chunk: %.120s", payload)
		}
		return false, nil
	}
	if detail := inlineErr(obj); detail != "" {
		if strings.Contains(strings.ToUpper(detail), "USER_BLOCKED") || strings.Contains(strings.ToLower(detail), "blocked") {
			return false, fmt.Errorf("%w: %s", ErrUserBlocked, detail)
		}
		return false, fmt.Errorf("z.ai: %s", detail)
	}
	if data, ok := obj["data"].(map[string]interface{}); ok {
		if ph, _ := data["phase"].(string); ph == "done" {
			return true, nil
		}
		if ec, _ := data["edit_content"].(string); ec != "" {
			idx := -1
			if f, ok := data["edit_index"].(float64); ok {
				idx = int(f)
			}
			p.applyEdit(ec, idx)
		} else if dc, _ := data["delta_content"].(string); dc != "" {
			p.full.WriteString(dc)
		} else if c, _ := data["content"].(string); c != "" {
			p.full.WriteString(c)
		}
	}
	p.flush(tv, rv, true)
	return false, nil
}
