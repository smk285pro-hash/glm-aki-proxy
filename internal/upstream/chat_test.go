// Tests for the upstream SSE parser primitives and request options.
package upstream

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestUTF16UnitsToBytes(t *testing.T) {
	if got := utf16UnitsToBytes("hello", 2); got != 2 {
		t.Fatalf("ascii: want 2, got %d", got)
	}
	if got := utf16UnitsToBytes("hello", 0); got != 0 {
		t.Fatalf("zero: want 0, got %d", got)
	}
	// "😀" is one rune, 4 bytes, 2 UTF-16 units.
	if got := utf16UnitsToBytes("😀", 1); got != 0 {
		t.Fatalf("mid-surrogate must clamp to rune start, got %d", got)
	}
	if got := utf16UnitsToBytes("😀", 2); got != 4 {
		t.Fatalf("full emoji: want 4, got %d", got)
	}
	if got := utf16UnitsToBytes("aé", 2); got != 3 {
		t.Fatalf("latin-1 tail: want 3, got %d", got)
	}
}

func TestEmitDelta(t *testing.T) {
	view := ""
	if d := emitDelta(&view, "hello"); d != "hello" || view != "hello" {
		t.Fatalf("fresh: got %q view %q", d, view)
	}
	if d := emitDelta(&view, "helloworld"); d != "world" || view != "helloworld" {
		t.Fatalf("append: got %q view %q", d, view)
	}
	if d := emitDelta(&view, "hello"); d != "" {
		t.Fatalf("rewrite shrink must yield nothing, got %q", d)
	}
	v2 := "hé"
	if d := emitDelta(&v2, "héllo"); d != "llo" {
		t.Fatalf("unicode tail: got %q", d)
	}
}

func TestSplitDetails(t *testing.T) {
	raw := "ANSWER<details><summary>think</summary>\n> line one\n</details>TAIL"
	p := &parser{}
	p.full.WriteString(raw)
	ans, why := p.split()
	if ans != "ANSWERTAIL" {
		t.Fatalf("answer: got %q", ans)
	}
	if !strings.Contains(why, "think") || !strings.Contains(why, "line one") {
		t.Fatalf("reasoning: got %q", why)
	}
	if strings.Contains(why, "> ") || strings.Contains(why, "<summary") {
		t.Fatalf("reasoning must be cleaned: %q", why)
	}
}

func TestSplitUnclosed(t *testing.T) {
	p := &parser{}
	p.full.WriteString("A<details>partial reasoning")
	ans, why := p.split()
	if ans != "A" || why != "partial reasoning" {
		t.Fatalf("unclosed: got %q / %q", ans, why)
	}
	p2 := &parser{}
	p2.full.WriteString("plain answer")
	if a, r := p2.split(); a != "plain answer" || r != "" {
		t.Fatalf("plain: got %q / %q", a, r)
	}
}

func feedAll(t *testing.T, p *parser, lines []string) (texts []string, whys []string, done bool) {
	t.Helper()
	tv := func(s string) { texts = append(texts, s) }
	rv := func(s string) { whys = append(whys, s) }
	for _, l := range lines {
		d, err := p.feed(l, tv, rv, false)
		if err != nil {
			t.Fatalf("feed: %v", err)
		}
		if d {
			done = true
		}
	}
	return texts, whys, done
}

func TestFeedDeltaContent(t *testing.T) {
	p := &parser{}
	long := strings.Repeat("x", 100)
	texts, _, done := feedAll(t, p, []string{
		`data: {"data":{"delta_content":"` + long + `"}}`,
		`data: [DONE]`,
	})
	if !done {
		t.Fatalf("DONE must finish")
	}
	joined := strings.Join(texts, "")
	// Live flush holds back a 24-rune tail; the remainder streams.
	if len(joined) != len(long)-24 {
		t.Fatalf("live holdback: want %d streamed, got %d", len(long)-24, len(joined))
	}
	p.flushFinal(func(s string) { texts = append(texts, s) }, nil)
	if got := strings.Join(texts, ""); got != long {
		t.Fatalf("final must complete the text, got %d chars", len(got))
	}
}

func TestFeedEditRewrite(t *testing.T) {
	p := &parser{}
	texts, _, _ := feedAll(t, p, []string{
		`data: {"data":{"delta_content":"Hello WORLD"}}`,
		`data: {"data":{"edit_content":"world","edit_index":6}}`,
		`data: [DONE]`,
	})
	_ = texts
	if got := p.text(); got != "Hello world" {
		t.Fatalf("edit rewrite: got %q", got)
	}
}

func TestFeedDetailsReasoning(t *testing.T) {
	p := &parser{}
	_, whys, _ := feedAll(t, p, []string{
		`data: {"data":{"delta_content":"ANSWER<details><summary>t</summary>\n> deep thought here, definitely more than twenty four runes long\n</details>"}}`,
		`data: [DONE]`,
	})
	p.flushFinal(nil, func(s string) { whys = append(whys, s) })
	got := strings.Join(whys, "")
	if !strings.Contains(got, "deep thought") {
		t.Fatalf("reasoning must extract, got %q", got)
	}
	if p.text() != "ANSWER" {
		t.Fatalf("answer must exclude details, got %q", p.text())
	}
}

func TestFeedInlineError(t *testing.T) {
	p := &parser{}
	_, err := p.feed(`data: {"data":{"error":{"message":"boom"}}}`, nil, nil, false)
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("inline error must surface, got %v", err)
	}
}

func TestApplyOpts(t *testing.T) {
	mk := func() map[string]interface{} {
		return map[string]interface{}{
			"image_generation": false, "web_search": false,
			"auto_web_search": false, "preview_mode": true, "enable_thinking": true,
		}
	}
	on, off := true, false

	f := mk()
	applyOpts(f, "glm-4.7", ChatOpts{WebSearch: &on})
	if f["auto_web_search"] != true || f["web_search"] != true {
		t.Fatalf("websearch on: %+v", f)
	}
	f = mk()
	applyOpts(f, "glm-4.7", ChatOpts{WebSearch: &off})
	if _, ok := f["auto_web_search"]; ok {
		t.Fatalf("websearch off must delete keys: %+v", f)
	}
	f = mk()
	applyOpts(f, "glm-4.7", ChatOpts{Thinking: &off})
	if f["enable_thinking"] != false {
		t.Fatalf("thinking override: %+v", f)
	}
	f = mk()
	applyOpts(f, "glm-5.3", ChatOpts{ReasoningEffort: "high"})
	if f["reasoning_effort"] != "high" || f["enable_thinking"] != true {
		t.Fatalf("effort on supported model: %+v", f)
	}
	f = mk()
	applyOpts(f, "glm-4.7", ChatOpts{ReasoningEffort: "high"})
	if _, ok := f["reasoning_effort"]; ok {
		t.Fatalf("effort on unsupported model must be ignored: %+v", f)
	}
	f = mk()
	applyOpts(f, "glm-5.3", ChatOpts{ReasoningEffort: "ultra"})
	if _, ok := f["reasoning_effort"]; ok {
		t.Fatalf("invalid effort must be ignored: %+v", f)
	}
	if FirstTrue(nil, &on, &off) != &on {
		t.Fatalf("FirstTrue must return first non-nil")
	}
	if FirstTrue(nil, nil) != nil {
		t.Fatalf("FirstTrue all-nil must be nil")
	}
}

func TestTLSClientCachingAndProfiles(t *testing.T) {
	// 1. Direct clients must be cached
	c1, err := GetBrowserClient("", 120)
	if err != nil {
		t.Fatalf("direct client init: %v", err)
	}
	c2, err := GetBrowserClient("", 120)
	if err != nil {
		t.Fatalf("direct client 2 init: %v", err)
	}
	if c1 != c2 {
		t.Fatalf("direct client must be cached, got different instances")
	}

	// 2. Proxied clients must NOT be cached to avoid socket starvation across mass rotations
	p1, err := GetBrowserClient("http://127.0.0.1:8080", 120)
	if err != nil {
		t.Fatalf("proxied client 1 init: %v", err)
	}
	p2, err := GetBrowserClient("http://127.0.0.1:8080", 120)
	if err != nil {
		t.Fatalf("proxied client 2 init: %v", err)
	}
	if p1 == p2 {
		t.Fatalf("proxied clients must be instantiated fresh per attempt, but were cached")
	}
}

func TestProxyPoolRotationAndDeadIsolation(t *testing.T) {
	p := NewProxyPool()
	err := p.Load("", "http://10.0.0.1:8080,http://10.0.0.2:8080,http://10.0.0.3:8080")
	if err != nil {
		t.Fatalf("load proxies: %v", err)
	}
	if p.TotalCount() != 3 {
		t.Fatalf("want 3 proxies, got %d", p.TotalCount())
	}
	if !p.Enabled() {
		t.Fatalf("proxy pool should be enabled")
	}

	u1 := p.CurrentURL()
	if u1 == nil || u1.Host != "10.0.0.1:8080" {
		t.Fatalf("expected 10.0.0.1:8080 first, got %v", u1)
	}

	// Mark 10.0.0.1 dead
	p.MarkDead(u1)
	if !p.IsDead(u1) {
		t.Fatalf("10.0.0.1 should be marked dead")
	}

	// CurrentURL must advance past dead proxy
	u2 := p.CurrentURL()
	if u2 == nil || u2.Host != "10.0.0.2:8080" {
		t.Fatalf("expected 10.0.0.2:8080 after u1 marked dead, got %v", u2)
	}

	// Rotate advances
	u3 := p.Rotate()
	if u3 == nil || u3.Host != "10.0.0.3:8080" {
		t.Fatalf("expected 10.0.0.3:8080 after rotate, got %v", u3)
	}

	// Disable pool
	p.SetEnabled(false)
	if p.Enabled() {
		t.Fatalf("pool should be disabled")
	}
	if p.CurrentURL() != nil {
		t.Fatalf("disabled pool must return nil CurrentURL")
	}
}

func TestThrottleGateAndCancellation(t *testing.T) {
	ResetThrottle()

	// 1. Initial call should succeed immediately
	ctx := context.Background()
	if err := Throttle(ctx); err != nil {
		t.Fatalf("throttle first call: %v", err)
	}

	// 2. Cancelled context should abort immediately without hanging
	cancelCtx, cancel := context.WithCancel(context.Background())
	cancel()

	err := Throttle(cancelCtx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled error, got %v", err)
	}
}

func TestUserBlockedSentinelAndWrapping(t *testing.T) {
	// Directly verify the bug fix from reference repository:
	// In reference repo: err == errZAIUnauthorized evaluated to false when wrapped with %w.
	wrappedErr := fmt.Errorf("%w: account rejected by platform (403)", ErrUserBlocked)

	// Direct == fails on wrapped error:
	if wrappedErr == ErrUserBlocked {
		t.Fatalf("wrappedErr == ErrUserBlocked should be false (demonstrating why == is flawed)")
	}

	// Genuine errors.Is works:
	if !errors.Is(wrappedErr, ErrUserBlocked) {
		t.Fatalf("errors.Is(wrappedErr, ErrUserBlocked) must return true")
	}

	// Test isTransient classification
	if isTransient(wrappedErr) {
		t.Fatalf("USER_BLOCKED must not be classified as transient")
	}
	if !isTransient(ErrWAFBlock) {
		t.Fatalf("WAFBlock must be classified as transient")
	}
	if !isTransient(ErrPoolOverloaded) {
		t.Fatalf("ErrPoolOverloaded must be classified as transient")
	}
}

func TestErrPoolOverloadedAnthropic529(t *testing.T) {
	if !errors.Is(ErrPoolOverloaded, ErrPoolOverloaded) {
		t.Fatalf("errors.Is must match ErrPoolOverloaded")
	}
	if ErrPoolOverloaded.StatusCode() != 529 {
		t.Fatalf("want status 529, got %d", ErrPoolOverloaded.StatusCode())
	}
	if ErrPoolOverloaded.RetryAfterSeconds() != 15 {
		t.Fatalf("want retry-after 15, got %d", ErrPoolOverloaded.RetryAfterSeconds())
	}

	rec := httptest.NewRecorder()
	WriteAnthropicOverloaded(rec)

	if rec.Code != 529 {
		t.Fatalf("want HTTP 529, got %d", rec.Code)
	}
	if rec.Header().Get("Retry-After") != "15" {
		t.Fatalf("want Retry-After: 15, got %q", rec.Header().Get("Retry-After"))
	}
	if !strings.Contains(rec.Header().Get("Content-Type"), "application/json") {
		t.Fatalf("want application/json Content-Type, got %q", rec.Header().Get("Content-Type"))
	}

	var resp struct {
		Type  string `json:"type"`
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to parse Anthropic JSON response: %v", err)
	}
	if resp.Type != "error" || resp.Error.Type != "overloaded_error" {
		t.Fatalf("unexpected Anthropic error payload: %+v", resp)
	}
}

