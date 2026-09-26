package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"glm-aki-proxy/internal/config"
	"glm-aki-proxy/internal/pool"
	"glm-aki-proxy/internal/session"
	"glm-aki-proxy/internal/upstream"
)

func newTestServer() *Server {
	cfg := config.Config{AuthToken: "test_secret"}
	sessPool := session.NewPool()
	tokPool, _ := pool.Open(cfg)
	return New(cfg, sessPool, tokPool)
}

func TestAdminHealthEndpoint(t *testing.T) {
	srv := newTestServer()
	req := httptest.NewRequest("GET", "/admin/health", nil)
	rec := httptest.NewRecorder()
	srv.mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	var res map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatalf("invalid json: %v", err)
	}
	if res["status"] != "healthy" && res["status"] != "degraded" {
		t.Errorf("unexpected status: %v", res["status"])
	}
	if _, ok := res["uptime_seconds"]; !ok {
		t.Errorf("missing uptime_seconds")
	}
}

func TestAdminStatsEndpoint(t *testing.T) {
	srv := newTestServer()
	req := httptest.NewRequest("GET", "/admin/stats", nil)
	rec := httptest.NewRecorder()
	srv.mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	var res map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatalf("invalid json: %v", err)
	}
	if _, ok := res["pacing_min_ms"]; !ok {
		t.Errorf("missing pacing_min_ms")
	}
}

func TestAdminSessionClearEndpoint(t *testing.T) {
	srv := newTestServer()
	req := httptest.NewRequest("POST", "/admin/session/clear", nil)
	req.Header.Set("Authorization", "Bearer test_secret")
	rec := httptest.NewRecorder()
	srv.mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var res map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatalf("invalid json: %v", err)
	}
	if res["status"] != "cleared" {
		t.Errorf("expected cleared status, got %v", res["status"])
	}
}

func TestModelsCatalogEndpoint(t *testing.T) {
	srv := newTestServer()
	req := httptest.NewRequest("GET", "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer test_secret")
	rec := httptest.NewRecorder()
	srv.mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	var res struct {
		Object string      `json:"object"`
		Data   []ModelInfo `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatalf("invalid json: %v", err)
	}
	if res.Object != "list" || len(res.Data) == 0 {
		t.Errorf("expected non-empty models list, got len=%d", len(res.Data))
	}
}

func TestResponsesEndpointValidation(t *testing.T) {
	srv := newTestServer()

	// Missing input
	req := httptest.NewRequest("POST", "/v1/responses", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer test_secret")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for empty input, got %d", rec.Code)
	}

	// Method not allowed
	reqGet := httptest.NewRequest("GET", "/v1/responses", nil)
	reqGet.Header.Set("Authorization", "Bearer test_secret")
	recGet := httptest.NewRecorder()
	srv.mux.ServeHTTP(recGet, reqGet)

	if recGet.Code != http.StatusMethodNotAllowed {
		t.Errorf("expected 405 for GET /v1/responses, got %d", recGet.Code)
	}
}

func TestResponsesInputConversion(t *testing.T) {
	// String input
	raw := json.RawMessage(`"What is the weather?"`)
	msgs, err := convertResponsesInput(raw)
	if err != nil || len(msgs) != 1 {
		t.Fatalf("expected 1 message from string input, err: %v", err)
	}

	// Typed items input
	rawItems := json.RawMessage(`[
		{"type": "message", "role": "user", "content": "Hello"},
		{"type": "function_call", "name": "get_weather", "call_id": "call_1", "arguments": "{\"city\":\"Hanoi\"}"},
		{"type": "function_call_output", "call_id": "call_1", "output": "{\"temp\": 28}"}
	]`)
	msgs, err = convertResponsesInput(rawItems)
	if err != nil || len(msgs) != 3 {
		t.Fatalf("expected 3 messages from items input, got len=%d err: %v", len(msgs), err)
	}
}

func TestVisionAutoRoute(t *testing.T) {
	// 1. Text only message -> hasImages is false
	textMsg := json.RawMessage(`{"role":"user","content":"explain code"}`)
	if hasImages([]json.RawMessage{textMsg}) {
		t.Errorf("expected hasImages false for text message")
	}

	// 2. Multimodal image message -> hasImages is true
	imgMsg := json.RawMessage(`{"role":"user","content":[{"type":"text","text":"what is this?"},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNk+M9QDwADhgGAWjR9awAAAABJRU5ErkJggg=="}}]}`)
	if !hasImages([]json.RawMessage{imgMsg}) {
		t.Errorf("expected hasImages true for image message")
	}
}

func TestSSEKeepAliveEmitsComment(t *testing.T) {
	w := httptest.NewRecorder()
	done := make(chan struct{})
	// Mock a chat that waits 5.2s so the 5s ticker fires at least once
	writeAnthropicStream(w, "msg_keepalive", "claude-3-7-sonnet", nil, nil, false,
		upstream.ChatOpts{}, func(onText, onReason func(string)) (string, string, error) {
			time.Sleep(5200 * time.Millisecond)
			close(done)
			return "done", "", nil
		})
	<-done
	body := w.Body.String()
	if !strings.Contains(body, ": keep-alive\n\n") {
		t.Errorf("expected SSE stream to contain ': keep-alive\\n\\n' ping, got body: %s", body)
	}
}

func TestResponsesToolConversion(t *testing.T) {
	raw := []json.RawMessage{
		json.RawMessage(`{"type":"function","name":"get_weather","description":"fetch weather","parameters":{"type":"object"}}`),
		json.RawMessage(`{"type":"invalid_type","name":"test"}`),
	}
	defs := convertResponsesTools(raw)
	if len(defs) != 1 || defs[0].Name != "get_weather" {
		t.Fatalf("expected 1 valid tool def, got %d", len(defs))
	}
}
