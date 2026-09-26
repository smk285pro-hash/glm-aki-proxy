// Package api exposes the OpenAI-compatible HTTP surface:
// GET /v1/models, POST /v1/chat/completions, POST /v1/messages
// (Anthropic shim for Claude Code), GET /status, GET / (web UI).
package api

import (
	"context"
	"crypto/subtle"
	_ "embed"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"glm-aki-proxy/internal/config"
	"glm-aki-proxy/internal/pool"
	"glm-aki-proxy/internal/session"
	"glm-aki-proxy/internal/upstream"
	"glm-aki-proxy/internal/util"
)

// Server wires config, identity, token pool and conversation memory.
type Server struct {
	cfg       config.Config
	sess      *session.Pool
	tokens    *pool.Pool
	mux       *http.ServeMux
	harvestMu sync.Mutex

	mu    sync.Mutex
	chats map[string]*conversation
}

type conversation struct {
	mu       sync.Mutex
	chatID   string
	messages []json.RawMessage
	used     time.Time
}

// begin locks the conversation for one request turn and returns a copy
// of its history. The lock is held across the upstream call so two
// concurrent turns on the same session cannot interleave history
// (lost-update race). Same-session requests therefore serialize;
// different sessions never block each other.
func (c *conversation) begin() []json.RawMessage {
	c.mu.Lock()
	c.used = time.Now()
	return append([]json.RawMessage(nil), c.messages...)
}

// commit appends turn records and releases the turn lock.
func (c *conversation) commit(records ...json.RawMessage) {
	c.messages = append(c.messages, records...)
	c.mu.Unlock()
}

// abort releases the turn lock without recording anything.
func (c *conversation) abort() { c.mu.Unlock() }

//go:embed web/index.html
var webIndex []byte

//go:embed web/install.ps1
var installScript string

// New builds the server (call Init first).
func New(cfg config.Config, sess *session.Pool, tokens *pool.Pool) *Server {
	s := &Server{cfg: cfg, sess: sess, tokens: tokens, chats: map[string]*conversation{}}
	m := http.NewServeMux()
	m.HandleFunc("/", s.handleIndex)
	m.HandleFunc("/v1/models", s.withAuth(s.handleModels))
	m.HandleFunc("/models", s.withAuth(s.handleModels))
	m.HandleFunc("/v1/chat/completions", s.withAuth(s.handleChat))
	m.HandleFunc("/v1/messages", s.withAuth(s.handleMessages))
	m.HandleFunc("/v1/messages/count_tokens", s.withAuth(s.handleCountTokens))
	m.HandleFunc("/status", s.handleStatus)

	// Aliases with /api prefix for compatibility
	m.HandleFunc("/api/v1/models", s.withAuth(s.handleModels))
	m.HandleFunc("/api/models", s.withAuth(s.handleModels))
	m.HandleFunc("/api/v1/chat/completions", s.withAuth(s.handleChat))
	m.HandleFunc("/api/v1/messages", s.withAuth(s.handleMessages))
	m.HandleFunc("/api/v1/messages/count_tokens", s.withAuth(s.handleCountTokens))
	m.HandleFunc("/api/status", s.handleStatus)

	// Healthcheck endpoints for gateways (e.g. OpenCode HEAD /api/hello)
	m.HandleFunc("/api/hello", s.handleHealth)
	m.HandleFunc("/hello", s.handleHealth)
	m.HandleFunc("/health", s.handleHealth)
	m.HandleFunc("/api/health", s.handleHealth)

	// 1-line Claude CLI setup script
	m.HandleFunc("/install.ps1", s.handleInstallPS1)
	m.HandleFunc("/api/install.ps1", s.handleInstallPS1)

	// OpenAI Responses API (for Codex CLI)
	m.HandleFunc("/v1/responses", s.withAuth(s.handleResponses))
	m.HandleFunc("/responses", s.withAuth(s.handleResponses))
	m.HandleFunc("/api/v1/responses", s.withAuth(s.handleResponses))
	m.HandleFunc("/api/responses", s.withAuth(s.handleResponses))

	// Admin endpoints
	m.HandleFunc("/admin/health", s.handleAdminHealth)
	m.HandleFunc("/admin/stats", s.handleAdminStats)
	m.HandleFunc("/admin/models", s.withAuth(s.handleAdminModels))
	m.HandleFunc("/admin/session/clear", s.withAuth(s.handleSessionClear))
	m.HandleFunc("/api/admin/health", s.handleAdminHealth)
	m.HandleFunc("/api/admin/stats", s.handleAdminStats)
	m.HandleFunc("/api/admin/models", s.withAuth(s.handleAdminModels))
	m.HandleFunc("/api/admin/session/clear", s.withAuth(s.handleSessionClear))

	// In-app token harvest
	m.HandleFunc("/api/harvest", s.withAuth(s.handleHarvest))
	m.HandleFunc("/harvest", s.withAuth(s.handleHarvest))

	s.mux = m
	go s.reap()
	return s
}

func (s *Server) Handler() http.Handler { return &accessLog{mux: s.mux} }

// accessLog logs one line per request (method, path, duration).
type accessLog struct{ mux http.Handler }

func (a *accessLog) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	// Clean double slashes e.g. //v1/messages -> /v1/messages
	for strings.Contains(r.URL.Path, "//") {
		r.URL.Path = strings.ReplaceAll(r.URL.Path, "//", "/")
	}
	// Normalize duplicated subpaths caused by client baseURL misconfigurations
	path := r.URL.Path
	if strings.HasSuffix(path, "/v1/messages/v1/messages") || strings.HasSuffix(path, "/messages/messages") {
		r.URL.Path = "/v1/messages"
	} else if strings.HasSuffix(path, "/v1/chat/completions/v1/chat/completions") {
		r.URL.Path = "/v1/chat/completions"
	}
	a.mux.ServeHTTP(w, r)
	log.Printf("http %s %s %s", r.Method, path, time.Since(start).Round(time.Millisecond))
}

// reap drops idle conversations after 30 minutes.
func (s *Server) reap() {
	for range time.Tick(5 * time.Minute) {
		now := time.Now()
		s.mu.Lock()
		for id, c := range s.chats {
			if now.Sub(c.used) > 30*time.Minute {
				delete(s.chats, id)
			}
		}
		s.mu.Unlock()
	}
}

func (s *Server) verbose() bool { return s.cfg.Debug() }

// ── auth ──

func (s *Server) withAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		got := strings.TrimSpace(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
		if got == "" {
			got = strings.TrimSpace(r.Header.Get("X-API-Key"))
		}
		if !apiKeyOK(got, s.cfg.AuthToken) {
			writeJSON(w, http.StatusUnauthorized, map[string]interface{}{
				"error": map[string]interface{}{
					"message": "invalid api key",
					"type":    "authentication_error",
				},
			})
			return
		}
		next(w, r)
	}
}

// ── / (embedded web chat UI) ──

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		writeJSON(w, http.StatusMethodNotAllowed, errObj("method not allowed"))
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(webIndex)
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		_, _ = w.Write([]byte("ok"))
	}
}



// ── /v1/chat/completions ──

type chatRequest struct {
	Model           string            `json:"model"`
	Messages        []json.RawMessage `json:"messages"`
	Stream          bool              `json:"stream"`
	Tools           []json.RawMessage `json:"tools"`
	WebSearch       *bool             `json:"webSearch"`
	Search          *bool             `json:"search"`
	DeepThink       *bool             `json:"deepThink"`
	ReasoningEffort string            `json:"reasoning_effort"`
}

// chatOpts folds the request's extra fields into upstream options.
func chatOpts(req chatRequest) upstream.ChatOpts {
	thinking := req.DeepThink
	if thinking == nil && !req.Stream {
		off := false
		thinking = &off
	}
	return upstream.ChatOpts{
		WebSearch:       upstream.FirstTrue(req.WebSearch, req.Search),
		Thinking:        thinking,
		ReasoningEffort: req.ReasoningEffort,
	}
}

func (s *Server) handleChat(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, errObj("method not allowed"))
		return
	}
	var req chatRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<20)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, errObj("invalid json body"))
		return
	}
	if len(req.Messages) == 0 {
		writeJSON(w, http.StatusBadRequest, errObj("messages must not be empty"))
		return
	}
	reqModel := req.Model
	if strings.TrimSpace(reqModel) == "" {
		reqModel = upstream.DefaultModel
	}
	model := mapModel(reqModel)

	// Per-session memory is opt-in via X-Session-Id (the web UI always
	// sends one). Without it the endpoint is stateless: most OpenAI
	// clients resend full history themselves, and prepending stored
	// history would duplicate it every turn (exponential input growth).
	// It also avoids cross-talk between clients sharing "default".
	var conv *conversation
	full := req.Messages
	if sid := r.Header.Get("X-Session-Id"); sid != "" || r.Header.Get("X-Fresh-Session") == "true" {
		conv = s.conversation(sid, r.Header.Get("X-Fresh-Session") == "true")
		full = append(conv.begin(), req.Messages...)
	}

	// Tool path: no native calls upstream, so inject the text contract.
	defs := normalizeTools(req.Tools)
	full = injectPersonaToMessages(full)
	if len(defs) > 0 {
		full = convertForTools(full, buildContract(defs))
	}

	id := "chatcmpl-" + util.UUIDv4()[:8]
	opts := chatOpts(req)
	if req.Stream {
		s.serveStream(w, r, id, reqModel, model, full, conv, req.Messages, defs, opts)
		return
	}
	text, reason, err := upstream.Chat(r.Context(), s.sess, s.tokens, model, full, nil, nil, s.verbose(), opts)
	if err != nil {
		if conv != nil {
			conv.abort()
		}
		fail(w, err)
		return
	}
	finish, msgObj := openAIMessage(text, reason, defs)
	if conv != nil {
		conv.commit(append(append([]json.RawMessage(nil), req.Messages...), assistantRecord(msgObj))...)
	}
	promptTokens := util.EstimateTokens(upstream.FlattenPrompt(full))
	completionTokens := util.EstimateTokens(text)
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"id": id, "object": "chat.completion", "created": time.Now().Unix(), "model": reqModel,
		"choices": []interface{}{map[string]interface{}{
			"index": 0, "message": msgObj, "finish_reason": finish,
		}},
		"usage": map[string]interface{}{
			"prompt_tokens":     promptTokens,
			"completion_tokens": completionTokens,
			"total_tokens":      promptTokens + completionTokens,
		},
	})
}

// openAIMessage parses tool calls out of model text and builds the
// OpenAI message object plus finish reason.
func openAIMessage(text, reason string, defs []ToolDef) (string, map[string]interface{}) {
	msg := map[string]interface{}{"role": "assistant", "content": text}
	if reason != "" {
		msg["reasoning_content"] = reason
	}
	if len(defs) == 0 {
		return "stop", msg
	}
	calls := parseToolCalls(text, defs)
	if len(calls) == 0 {
		return "stop", msg
	}
	residual := stripToolBlocksForDefs(text, defs)
	msg["content"] = residual
	var tc []interface{}
	for _, c := range calls {
		tc = append(tc, map[string]interface{}{
			"id": c.ID, "type": "function",
			"function": map[string]interface{}{"name": c.Name, "arguments": c.Arguments},
		})
	}
	msg["tool_calls"] = tc
	return "tool_calls", msg
}

// assistantRecord serializes the answered assistant message for
// conversation memory (tool_calls preserved for the next turn).
func assistantRecord(msg map[string]interface{}) json.RawMessage {
	raw, _ := json.Marshal(msg)
	return raw
}

func (s *Server) serveStream(w http.ResponseWriter, r *http.Request, id, reqModel, model string,
	full []json.RawMessage, conv *conversation, fresh []json.RawMessage, defs []ToolDef, opts upstream.ChatOpts) {

	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache, no-transform")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	flusher, ok := w.(http.Flusher)
	if !ok {
		if conv != nil {
			conv.abort()
		}
		fail(w, fmt.Errorf("streaming unsupported"))
		return
	}
	flusher.Flush()
	emit := func(delta map[string]interface{}) {
		chunk := map[string]interface{}{
			"id": id, "object": "chat.completion.chunk", "created": time.Now().Unix(), "model": reqModel,
			"choices": []interface{}{map[string]interface{}{
				"index": 0, "delta": delta, "finish_reason": nil,
			}},
		}
		raw, _ := json.Marshal(chunk)
		fmt.Fprintf(w, "data: %s\n\n", raw)
		flusher.Flush()
	}
	emitTool := func(d toolDelta) {
		fn := map[string]interface{}{}
		if d.HasNameOnly {
			fn["name"] = d.Name
			fn["arguments"] = ""
		} else if d.ArgsFrag != "" {
			fn["arguments"] = d.ArgsFrag
		} else {
			return
		}
		tc := map[string]interface{}{"index": d.Index, "function": fn}
		if d.HasNameOnly {
			tc["id"] = d.ID
			tc["type"] = "function"
		}
		emit(map[string]interface{}{"tool_calls": []interface{}{tc}})
	}
	var filter *toolFilter
	if len(defs) > 0 {
		filter = newToolFilter(defs)
	}
	var acc strings.Builder // raw upstream text (pre-filter)
	var residue strings.Builder
	var accWhy strings.Builder
	var streamedCalls []toolCall
	noteCall := func(d toolDelta) {
		if d.HasNameOnly {
			streamedCalls = append(streamedCalls, toolCall{ID: d.ID, Name: d.Name})
		} else if d.ArgsFrag != "" {
			for i := range streamedCalls {
				if streamedCalls[i].ID == d.ID {
					streamedCalls[i].Arguments += d.ArgsFrag
				}
			}
		}
	}
	text, reason, err := upstream.Chat(r.Context(), s.sess, s.tokens, model, full,
		func(d string) {
			acc.WriteString(d)
			if filter == nil {
				residue.WriteString(d)
				emit(map[string]interface{}{"content": d})
				return
			}
			plain, deltas := filter.feed(d)
			if plain != "" {
				residue.WriteString(plain)
				emit(map[string]interface{}{"content": plain})
			}
			for _, td := range deltas {
				noteCall(td)
				if !td.CallDone {
					emitTool(td)
				}
			}
		},
		func(d string) { accWhy.WriteString(d); emit(map[string]interface{}{"reasoning_content": d}) },
		s.verbose(), opts)
	if err != nil {
		emit(map[string]interface{}{"content": "\n[upstream error: " + err.Error() + "]"})
	}
	if filter != nil {
		if tail := filter.flushText(); tail != "" {
			residue.WriteString(tail)
			emit(map[string]interface{}{"content": tail})
		}
	}
	if text == "" {
		text = acc.String()
	}
	if reason == "" {
		reason = accWhy.String()
	}
	finish := "stop"
	msgObj := map[string]interface{}{"role": "assistant", "content": residue.String()}
	if reason != "" {
		msgObj["reasoning_content"] = reason
	}
	if filter != nil {
		calls := streamedCalls
		if len(calls) == 0 {
			// Safety net: markers the live filter missed (odd shapes).
			calls = parseToolCalls(text, defs)
			if len(calls) > 0 {
				// A legacy call was held out of the live stream. Send only
				// answer text that was not already emitted.
				residual := stripToolBlocksForDefs(text, defs)
				if strings.HasPrefix(residual, residue.String()) {
					if tail := residual[residue.Len():]; tail != "" {
						residue.WriteString(tail)
						emit(map[string]interface{}{"content": tail})
					}
				}
			}
			for i := range calls {
				calls[i].ID = fmt.Sprintf("call_net_%d", i)
				emitTool(toolDelta{Index: i, ID: calls[i].ID, Name: calls[i].Name, HasNameOnly: true})
				if calls[i].Arguments != "" && calls[i].Arguments != "{}" {
					emitTool(toolDelta{Index: i, ID: calls[i].ID, ArgsFrag: calls[i].Arguments})
				}
			}
		}
		if len(calls) > 0 {
			finish = "tool_calls"
			msgObj["content"] = stripToolBlocksForDefs(text, defs)
			var tc []interface{}
			for _, c := range calls {
				tc = append(tc, map[string]interface{}{
					"id": c.ID, "type": "function",
					"function": map[string]interface{}{"name": c.Name, "arguments": c.Arguments},
				})
			}
			msgObj["tool_calls"] = tc
		}
	}
	if conv != nil {
		conv.commit(append(append([]json.RawMessage(nil), fresh...), assistantRecord(msgObj))...)
	}
	done := map[string]interface{}{
		"id": id, "object": "chat.completion.chunk", "created": time.Now().Unix(), "model": reqModel,
		"choices": []interface{}{map[string]interface{}{
			"index": 0, "delta": map[string]interface{}{}, "finish_reason": finish,
		}},
	}
	raw, _ := json.Marshal(done)
	fmt.Fprintf(w, "data: %s\n\n", raw)
	fmt.Fprintf(w, "data: [DONE]\n\n")
	flusher.Flush()
}

// ── /status ──

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	_, uid, name, fe, ready := s.sess.Snapshot()
	if len(uid) > 8 {
		uid = uid[:8] + "..."
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"service": "glm-aki-proxy", "connected": ready,
		"user": name, "user_id": uid, "frontend": fe,
		"accounts_total":   s.sess.TotalCount(),
		"accounts_healthy": s.sess.HealthyCount(),
		"pool_tokens":      s.tokens.Count(), "port": s.cfg.Port,
	})
}

// ── conversations ──

func (s *Server) conversation(sessionID string, fresh bool) *conversation {
	if sessionID == "" {
		sessionID = "default"
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	c := s.chats[sessionID]
	if fresh || c == nil {
		c = &conversation{chatID: util.UUIDv4()}
		s.chats[sessionID] = c
	}
	c.used = time.Now()
	return c
}

// ── helpers ──

// apiKeyOK compares keys in constant time. Empty candidates never match
// (fail closed when AUTH_TOKEN is unset).
func apiKeyOK(got, want string) bool {
	if got == "" || want == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}

func errObj(msg string) map[string]interface{} {
	return map[string]interface{}{"error": map[string]interface{}{"message": msg}}
}

func fail(w http.ResponseWriter, err error) {
	msg := err.Error()
	code := http.StatusBadGateway
	if strings.Contains(msg, "captcha") || strings.Contains(msg, "pool") {
		code = http.StatusServiceUnavailable
	}
	if strings.Contains(msg, "401") || strings.Contains(msg, "expired") {
		code = http.StatusUnauthorized
	}
	writeJSON(w, code, map[string]interface{}{
		"error": map[string]interface{}{"message": msg, "type": "upstream_error"},
	})
}

func writeJSON(w http.ResponseWriter, code int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// ── /install.ps1 ──

func (s *Server) handleInstallPS1(w http.ResponseWriter, r *http.Request) {
	key := r.Header.Get("x-api-key")
	if key == "" {
		auth := r.Header.Get("Authorization")
		if strings.HasPrefix(strings.ToLower(auth), "bearer ") {
			key = strings.TrimSpace(auth[7:])
		}
	}
	if key == "" {
		key = r.URL.Query().Get("key")
	}
	if key == "" {
		key = "YOUR_API_KEY"
	}

	proto := "https"
	if r.TLS == nil && r.Header.Get("X-Forwarded-Proto") != "https" && !strings.Contains(r.Host, "vercel.app") {
		if r.Header.Get("X-Forwarded-Proto") != "" {
			proto = r.Header.Get("X-Forwarded-Proto")
		} else {
			proto = "http"
		}
	}
	origin := fmt.Sprintf("%s://%s", proto, r.Host)

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	content := strings.ReplaceAll(installScript, "__KEY__", key)
	content = strings.ReplaceAll(content, "__ORIGIN__", origin)

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(content))
}

// ── /api/harvest ──

func (s *Server) handleHarvest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, errObj("method not allowed"))
		return
	}
	if !s.harvestMu.TryLock() {
		writeJSON(w, http.StatusConflict, map[string]interface{}{
			"success": false,
			"error":   "thu hoạch token đang chạy, vui lòng chờ...",
		})
		return
	}
	defer s.harvestMu.Unlock()

	count := 50
	if q := r.URL.Query().Get("count"); q != "" {
		if n, err := strconv.Atoi(q); err == nil && n > 0 && n <= 2000 {
			count = n
		}
	}

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
	defer cancel()

	// Locate collector binary or fallback to go run
	var cmd *exec.Cmd
	collectBin := ""
	candidates := []string{"aki-collect.exe", "aki-collect", "./aki-collect.exe", "./aki-collect"}
	if exePath, err := os.Executable(); err == nil {
		dir := filepath.Dir(exePath)
		candidates = append([]string{
			filepath.Join(dir, "aki-collect.exe"),
			filepath.Join(dir, "aki-collect"),
		}, candidates...)
	}

	for _, c := range candidates {
		if fi, err := os.Stat(c); err == nil && !fi.IsDir() {
			collectBin = c
			break
		}
	}

	if collectBin != "" {
		cmd = exec.CommandContext(ctx, collectBin, "--count", strconv.Itoa(count))
	} else {
		cmd = exec.CommandContext(ctx, "go", "run", "./cmd/collect", "--count", strconv.Itoa(count))
	}

	out, err := cmd.CombinedOutput()
	if err != nil {
		log.Printf("[harvest] error: %v, out: %s", err, string(out))
		writeJSON(w, http.StatusInternalServerError, map[string]interface{}{
			"success": false,
			"error":   fmt.Sprintf("thu hoạch token thất bại: %v", err),
			"output":  string(out),
		})
		return
	}

	if err := s.tokens.Reload(); err != nil {
		log.Printf("[harvest] reload pool error: %v", err)
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"success":     true,
		"pool_tokens": s.tokens.Count(),
		"message":     fmt.Sprintf("Nạp thành công! Hiện có %d token sẵn sàng.", s.tokens.Count()),
	})
}

func init() { log.SetFlags(log.LstdFlags) }
