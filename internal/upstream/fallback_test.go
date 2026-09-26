package upstream

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"glm-aki-proxy/internal/captcha"
	"glm-aki-proxy/internal/session"
)

// TestChatAutoFallbackToGLM52WhenAccountsExhausted verifies that when all accounts in the pool
// fail or enter cooldown with the requested model (e.g. glm-5.3), Chat automatically rotates
// and falls back to glm-5.2, resetting capacity cooldown and succeeding.
func TestChatAutoFallbackToGLM52WhenAccountsExhausted(t *testing.T) {
	origBaseURL := session.BaseURL
	defer func() { session.BaseURL = origBaseURL }()

	origSolve := solveCaptcha
	solveCaptcha = func(take captcha.TokenTaker, verbose bool) string {
		return "mock_param"
	}
	defer func() { solveCaptcha = origSolve }()

	tok1 := "eyJhbGciOiJub25lIn0.eyJpZCI6InVzZXJfMSIsImVtYWlsIjoidXNlcjFAdGVzdC5jb20ifQ.sig"
	tok2 := "eyJhbGciOiJub25lIn0.eyJpZCI6InVzZXJfMiIsImVtYWlsIjoidXNlcjJAdGVzdC5jb20ifQ.sig"

	modelsRequested := make([]string, 0)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`<html><script src="/static/fe_20260926.js"></script></html>`))
			return
		}
		if r.URL.Path == "/api/v1/auths/" {
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"code":200,"data":{"id":"ok"}}`))
			return
		}
		if r.URL.Path == "/api/v2/chat/completions" {
			bodyBytes, _ := io.ReadAll(r.Body)
			var bodyMap map[string]interface{}
			json.Unmarshal(bodyBytes, &bodyMap)
			m, _ := bodyMap["model"].(string)
			modelsRequested = append(modelsRequested, m)

			// If requested model is glm-5.3, return 429 Too Many Requests (capacity busy)
			if strings.EqualFold(m, "glm-5.3") {
				w.WriteHeader(http.StatusTooManyRequests)
				w.Write([]byte(`{"code":429,"message":"capacity busy / rate limit"}`))
				return
			}

			// If requested model is glm-5.2, return success!
			if strings.EqualFold(m, "glm-5.2") {
				w.Header().Set("Content-Type", "text/event-stream")
				w.WriteHeader(http.StatusOK)
				w.Write([]byte("data: {\"data\":{\"delta_content\":\"hello from fallback glm-5.2\"}}\n\ndata: [DONE]\n\n"))
				return
			}
		}

		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	session.BaseURL = server.URL

	pool := session.NewPool()
	if err := pool.Init([]string{tok1, tok2}); err != nil {
		t.Fatalf("pool.Init failed: %v", err)
	}

	ctx := context.Background()
	msgs := []json.RawMessage{json.RawMessage(`{"role":"user","content":"hi"}`)}

	taker := dummyTaker{}
	text, _, err := Chat(ctx, pool, taker, "glm-5.3", msgs, nil, nil, false, ChatOpts{})
	if err != nil {
		t.Fatalf("expected Chat to succeed via fallback to glm-5.2, got error: %v", err)
	}

	if text != "hello from fallback glm-5.2" {
		t.Fatalf("expected 'hello from fallback glm-5.2', got %q", text)
	}

	has53 := false
	has52 := false
	for _, m := range modelsRequested {
		if strings.EqualFold(m, "glm-5.3") {
			has53 = true
		}
		if strings.EqualFold(m, "glm-5.2") {
			has52 = true
		}
	}
	if !has53 || !has52 {
		t.Fatalf("expected attempts with both glm-5.3 and fallback glm-5.2, got: %v", modelsRequested)
	}
}

// TestChatAutoFallbackBothFailReturns529 verifies that when both requested model and glm-5.2
// fail, Chat terminates and returns ErrPoolOverloaded (529).
func TestChatAutoFallbackBothFailReturns529(t *testing.T) {
	origBaseURL := session.BaseURL
	defer func() { session.BaseURL = origBaseURL }()

	origSolve := solveCaptcha
	solveCaptcha = func(take captcha.TokenTaker, verbose bool) string {
		return "mock_param"
	}
	defer func() { solveCaptcha = origSolve }()

	tok := "eyJhbGciOiJub25lIn0.eyJpZCI6InVzZXJfMSIsImVtYWlsIjoidXNlcjFAdGVzdC5jb20ifQ.sig"

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`<html><script src="/static/fe_20260926.js"></script></html>`))
			return
		}
		if r.URL.Path == "/api/v1/auths/" {
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"code":200,"data":{"id":"ok"}}`))
			return
		}
		if r.URL.Path == "/api/v2/chat/completions" {
			// All models return 429
			w.WriteHeader(http.StatusTooManyRequests)
			w.Write([]byte(`{"code":429,"message":"capacity busy"}`))
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	session.BaseURL = server.URL

	pool := session.NewPool()
	if err := pool.Init([]string{tok}); err != nil {
		t.Fatalf("pool.Init failed: %v", err)
	}

	ctx := context.Background()
	msgs := []json.RawMessage{json.RawMessage(`{"role":"user","content":"hi"}`)}

	taker := dummyTaker{}
	_, _, err := Chat(ctx, pool, taker, "glm-5.3", msgs, nil, nil, false, ChatOpts{})
	if err == nil {
		t.Fatalf("expected error, got nil")
	}
	if !errors.Is(err, ErrPoolOverloaded) {
		t.Fatalf("expected ErrPoolOverloaded (529), got: %v", err)
	}
}
