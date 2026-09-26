package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"glm-aki-proxy/internal/config"
	"glm-aki-proxy/internal/pool"
	"glm-aki-proxy/internal/session"
)

// TestAnthropicOverloadedPoolReturnsHTTP529 tests whether the Anthropic Messages
// endpoint returns HTTP 529 with Retry-After: 15 when the pool is in cooldown / overloaded.
func TestAnthropicOverloadedPoolReturnsHTTP529(t *testing.T) {
	origBaseURL := session.BaseURL
	defer func() { session.BaseURL = origBaseURL }()

	// Mock server for session.Init
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
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	session.BaseURL = server.URL

	tok := "eyJhbGciOiJub25lIn0.eyJpZCI6InVzZXJfMSIsImVtYWlsIjoidXNlcjFAdGVzdC5jb20ifQ.sig"
	sessPool := session.NewPool()
	if err := sessPool.Init([]string{tok}); err != nil {
		t.Fatalf("sessPool.Init failed: %v", err)
	}

	if sessPool.TotalCount() != 1 || sessPool.HealthyCount() != 1 {
		t.Fatalf("expected 1 healthy session, got total=%d healthy=%d",
			sessPool.TotalCount(), sessPool.HealthyCount())
	}

	// Now mark the session as fatal (disabled 24h) to simulate pool cooldown/overload
	s := sessPool.Pick()
	if s == nil {
		t.Fatalf("Pick returned nil session")
	}
	s.MarkFatal()

	if sessPool.HealthyCount() != 0 {
		t.Fatalf("expected healthy count 0 after MarkFatal, got %d", sessPool.HealthyCount())
	}

	cfg := config.Config{AuthToken: "test_token"}
	tokPool, _ := pool.Open(cfg)
	srv := New(cfg, sessPool, tokPool)

	reqBody := `{"model":"claude-3-7-sonnet","messages":[{"role":"user","content":"hello"}]}`
	req := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(reqBody))
	req.Header.Set("Authorization", "Bearer test_token")
	req.Header.Set("Content-Type", "application/json")

	rec := httptest.NewRecorder()
	srv.mux.ServeHTTP(rec, req)

	t.Logf("Response Status Code: %d", rec.Code)
	t.Logf("Response Headers: %+v", rec.Header())
	t.Logf("Response Body: %s", rec.Body.String())

	// Requirement R4: HTTP 529 Overloaded with Retry-After: 15
	if rec.Code != 529 {
		t.Errorf("FAIL: Expected HTTP status 529 for overloaded pool, got %d", rec.Code)
	}

	retryAfter := rec.Header().Get("Retry-After")
	if retryAfter != "15" {
		t.Errorf("FAIL: Expected Retry-After header '15', got %q", retryAfter)
	}

	var errResp struct {
		Type  string `json:"type"`
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &errResp); err != nil {
		t.Fatalf("failed to decode Anthropic error response: %v", err)
	}

	if errResp.Type != "error" || errResp.Error.Type != "overloaded_error" {
		t.Errorf("FAIL: Expected Anthropic error type 'overloaded_error', got type=%q error.type=%q",
			errResp.Type, errResp.Error.Type)
	}
}
