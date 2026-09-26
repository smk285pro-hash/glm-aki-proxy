package upstream

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"glm-aki-proxy/internal/session"
)

type dummyTaker struct{}

func (d dummyTaker) Take() (string, bool) {
	return "dummy", true
}

// TestUserBlockedMultiLevelWrapping tests that multi-level wrapped errors
// are cleanly matched by errors.Is(err, ErrUserBlocked).
func TestUserBlockedMultiLevelWrapping(t *testing.T) {
	err0 := ErrUserBlocked
	err1 := fmt.Errorf("%w: account rejected by platform (403)", err0)
	err2 := fmt.Errorf("roundTrip transport failure: %w", err1)
	err3 := fmt.Errorf("upstream chat failure: %w", err2)
	err4 := fmt.Errorf("api handler dispatch: %w", err3)

	if !errors.Is(err4, ErrUserBlocked) {
		t.Fatalf("errors.Is failed on 4-level wrapped ErrUserBlocked: %v", err4)
	}

	// Double check != fails for wrapped errors (demonstrating reference repo's bug)
	if err4 == ErrUserBlocked {
		t.Fatalf("direct == equality should be false for wrapped error")
	}
}

// TestUserBlockedRotationAndFailoverEmpirical verifies that:
// 1. roundTrip detects 403 USER_BLOCKED and returns ErrUserBlocked wrapped.
// 2. Multi-level wrapped error is caught by errors.Is.
// 3. pool.ReportFatalError disables the token for 24h.
// 4. pool.HasAlternative detects alternative token.
// 5. Next roundTrip call succeeds with the alternative token.
func TestUserBlockedRotationAndFailoverEmpirical(t *testing.T) {
	origBaseURL := session.BaseURL
	defer func() { session.BaseURL = origBaseURL }()

	tok1 := "eyJhbGciOiJub25lIn0.eyJpZCI6InVzZXJfMSIsImVtYWlsIjoidXNlcjFAdGVzdC5jb20ifQ.sig"
	tok2 := "eyJhbGciOiJub25lIn0.eyJpZCI6InVzZXJfMiIsImVtYWlsIjoidXNlcjJAdGVzdC5jb20ifQ.sig"

	reqCount := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reqCount++
		auth := r.Header.Get("authorization")

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
			if auth == "Bearer "+tok1 {
				w.WriteHeader(http.StatusForbidden)
				w.Write([]byte(`{"code": 403, "message": "USER_BLOCKED: account disabled by platform"}`))
				return
			}
			if auth == "Bearer "+tok2 {
				w.Header().Set("Content-Type", "text/event-stream")
				w.WriteHeader(http.StatusOK)
				w.Write([]byte("data: {\"data\":{\"delta_content\":\"hello from healthy token 2\"}}\n\ndata: [DONE]\n\n"))
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

	if pool.HealthyCount() != 2 {
		t.Fatalf("expected 2 healthy accounts, got %d", pool.HealthyCount())
	}

	ctx := context.Background()
	msgs := []json.RawMessage{json.RawMessage(`{"role":"user","content":"hi"}`)}

	// 1. Pick first session (token 1)
	sess1 := pool.Pick()
	if sess1 == nil {
		t.Fatalf("expected session 1, got nil")
	}

	// 2. Perform roundTrip with token 1 -> should return 403 USER_BLOCKED
	_, _, _, err1 := roundTrip(ctx, sess1, "glm-4.7", msgs, "hi", "mock_param", nil, nil, false, ChatOpts{}, nil)
	if err1 == nil {
		t.Fatalf("expected error from blocked token 1, got nil")
	}

	// Multi-level wrap to verify robustness
	wrappedErr := fmt.Errorf("transport layer: %w", fmt.Errorf("roundTrip: %w", err1))
	if !errors.Is(wrappedErr, ErrUserBlocked) {
		t.Fatalf("errors.Is(wrappedErr, ErrUserBlocked) failed: %v", wrappedErr)
	}

	// 3. Execute rotation logic identical to chat.go:279-286
	if errors.Is(wrappedErr, ErrUserBlocked) {
		pool.ReportFatalError(sess1, wrappedErr)
		if !pool.HasAlternative(sess1) {
			t.Fatalf("pool should have alternative for sess1")
		}
	} else {
		t.Fatalf("expected errors.Is to catch ErrUserBlocked")
	}

	// Verify sess1 is disabled
	if sess1.InCooldown() != true {
		t.Fatalf("sess1 should be in cooldown (disabled 24h)")
	}
	if pool.HealthyCount() != 1 {
		t.Fatalf("expected 1 healthy session remaining, got %d", pool.HealthyCount())
	}

	// 4. Pick next session -> must return sess2
	sess2 := pool.Pick()
	if sess2 == nil || sess2 == sess1 {
		t.Fatalf("expected healthy alternative session 2, got %v", sess2)
	}

	// 5. Perform roundTrip with token 2 -> must succeed
	text, _, _, err2 := roundTrip(ctx, sess2, "glm-4.7", msgs, "hi", "mock_param", nil, nil, false, ChatOpts{}, nil)
	if err2 != nil {
		t.Fatalf("roundTrip on alternative token 2 failed: %v", err2)
	}
	if text != "hello from healthy token 2" {
		t.Fatalf("expected 'hello from healthy token 2', got %q", text)
	}

	// 6. Block token 2 as well
	pool.ReportFatalError(sess2, wrappedErr)
	if pool.HasAlternative(sess2) {
		t.Fatalf("pool should have NO alternative when all tokens blocked")
	}
	if pool.HealthyCount() != 0 {
		t.Fatalf("expected 0 healthy accounts, got %d", pool.HealthyCount())
	}

	// 7. Verify Chat() immediately returns ErrPoolOverloaded without burning captcha
	taker := dummyTaker{}
	_, _, errOverload := Chat(ctx, pool, taker, "glm-4.7", msgs, nil, nil, false, ChatOpts{})
	if !errors.Is(errOverload, ErrPoolOverloaded) {
		t.Fatalf("expected ErrPoolOverloaded when all accounts blocked, got: %v", errOverload)
	}

	var overloaded *OverloadedError
	if !errors.As(errOverload, &overloaded) {
		t.Fatalf("expected *OverloadedError, got %T", errOverload)
	}
	if overloaded.StatusCode() != 529 {
		t.Fatalf("expected HTTP 529, got %d", overloaded.StatusCode())
	}
	if overloaded.RetryAfterSeconds() != 15 {
		t.Fatalf("expected Retry-After: 15, got %d", overloaded.RetryAfterSeconds())
	}
}
