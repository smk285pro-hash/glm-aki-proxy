// Anthropic Messages API compatibility shim.
//
// Claude Code CLI speaks POST /v1/messages (Anthropic format) and sends
// its key in the x-api-key header. This file translates that dialect to
// the OpenAI-shaped messages upstream.Chat expects, then re-wraps the
// answer (or the SSE event stream) back into Anthropic shape.
//
// Tool calling rides the text-protocol adapter in tools.go: tool
// definitions become a marker contract in the prompt, marker blocks in
// the reply become tool_use blocks. The CLI itself runs the agentic
// loop; this server is stateless per request (plus session memory).
package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"glm-aki-proxy/internal/upstream"
	"glm-aki-proxy/internal/util"
)

var (
	systemReminderRe = regexp.MustCompile(`(?s)<system-reminder>.*?</system-reminder>`)
)

// stripAnthropicNoise removes Desktop/CLI injected blocks GLM doesn't need (cuts ~30k chars).
func stripAnthropicNoise(s string) string {
	s = systemReminderRe.ReplaceAllString(s, "")
	if i := strings.Index(s, "x-anthropic-billing-header:"); i >= 0 {
		if j := strings.Index(s[i:], "\n"); j >= 0 {
			s = s[:i] + s[i+j+1:]
		} else {
			s = s[:i]
		}
	}
	return strings.TrimSpace(s)
}

// maxSystem caps huge system prompts before upstream Z.AI.
const maxSystem = 8000

func capSystem(content string) string {
	content = stripAnthropicNoise(content)
	if len(content) <= maxSystem {
		return content
	}
	head := maxSystem * 3 / 4
	tail := maxSystem - head
	return content[:head] + "\n...[truncated]...\n" + content[len(content)-tail:]
}

// ── request ──

type anthropicRequest struct {
	Model           string             `json:"model"`
	MaxTokens       int                `json:"max_tokens"`
	Messages        []json.RawMessage  `json:"messages"`
	System          json.RawMessage    `json:"system"`
	Stream          bool               `json:"stream"`
	Tools           []json.RawMessage  `json:"tools"`
	WebSearch       *bool              `json:"webSearch"`
	DeepThink       *bool              `json:"deepThink"`
	Thinking        *anthropicThinking `json:"thinking"`
	ReasoningEffort string             `json:"reasoning_effort"`
}

type anthropicThinking struct {
	Type         string `json:"type"`
	BudgetTokens int    `json:"budget_tokens"`
}

// anthropicBlock is one element of an Anthropic content array.
type anthropicBlock struct {
	Type     string          `json:"type"`
	ID       string          `json:"id"`
	Text     string          `json:"text"`
	Thinking string          `json:"thinking"`
	Name     string          `json:"name"`
	Input    json.RawMessage `json:"input"`
	Source   json.RawMessage `json:"source"`
	// tool_result nests content the same way message content does.
	Content   json.RawMessage `json:"content"`
	ToolUseID string          `json:"tool_use_id"`
	IsError   bool            `json:"is_error,omitempty"`
}

// blockText flattens one content block to plain text.
func blockText(b anthropicBlock) string {
	switch b.Type {
	case "", "text":
		return stripAnthropicNoise(b.Text)
	case "thinking":
		// Thinking blocks echoed back by the client: keep the text,
		// drop the (unverifiable) signature.
		return b.Thinking
	case "tool_result":
		if s, ok := unquote(b.Content); ok {
			return stripAnthropicNoise(s)
		}
		if inner, ok := unmarshalBlocks(b.Content); ok {
			var sb strings.Builder
			for _, ib := range inner {
				sb.WriteString(blockText(ib))
				sb.WriteString("\n")
			}
			return strings.TrimSpace(sb.String())
		}
		return string(b.Content)
	case "tool_use":
		// Unreachable in practice (tools are rejected upfront), but
		// never silently drop user-visible content.
		return "[tool call: " + b.Name + " " + string(b.Input) + "]"
	case "image":
		return "[image omitted]"
	default:
		if b.Text != "" {
			return b.Text
		}
		return "[" + b.Type + " omitted]"
	}
}

// messageText flattens an Anthropic message content (string or blocks).
func messageText(raw json.RawMessage) (string, error) {
	if s, ok := unquote(raw); ok {
		return s, nil
	}
	blocks, ok := unmarshalBlocks(raw)
	if !ok {
		return "", fmt.Errorf("bad content")
	}
	var sb strings.Builder
	for _, b := range blocks {
		sb.WriteString(blockText(b))
		sb.WriteString("\n")
	}
	return strings.TrimSpace(sb.String()), nil
}

func unquote(raw json.RawMessage) (string, bool) {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s, true
	}
	return "", false
}

func unmarshalBlocks(raw json.RawMessage) ([]anthropicBlock, bool) {
	var blocks []anthropicBlock
	if json.Unmarshal(raw, &blocks) != nil {
		return nil, false
	}
	return blocks, true
}

// Auxiliary CLI prompts (input suggestions, away-recaps) must be
// answered TEXT-ONLY: a tool_use there is rejected by the CLI and
// poisons the main history. Matched on the last user message.
var auxMarkers = []string{
	"[SUGGESTION MODE:",
	"The user stepped away and is coming back. Recap in under 40 words",
}

func isAuxRequest(msgs []json.RawMessage) bool {
	for i := len(msgs) - 1; i >= 0; i-- {
		var m struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		}
		if json.Unmarshal(msgs[i], &m) != nil {
			continue
		}
		if m.Role != "user" {
			continue
		}
		text, err := messageText(m.Content)
		if err != nil {
			return false
		}
		for _, mk := range auxMarkers {
			if strings.Contains(text, mk) {
				return true
			}
		}
		return false
	}
	return false
}

// toOpenAI converts the Anthropic request into OpenAI-shaped messages
// for upstream.Chat + server-side conversation memory. tool_use blocks
// become tool_calls (Claude ids preserved for result matching),
// tool_result blocks become tool-role messages.
func toOpenAI(req anthropicRequest) ([]json.RawMessage, error) {
	out := []json.RawMessage{}
	if len(req.System) > 0 && string(req.System) != "null" {
		sys, err := messageText(req.System)
		if err != nil {
			return nil, fmt.Errorf("bad system block")
		}
		raw, _ := json.Marshal(map[string]string{"role": "system", "content": capSystem(injectPersonaToSystem(sys))})
		out = append(out, raw)
	}
	for _, m := range req.Messages {
		var msg struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		}
		if json.Unmarshal(m, &msg) != nil {
			return nil, fmt.Errorf("bad message")
		}
		if msg.Role != "user" && msg.Role != "assistant" && msg.Role != "system" {
			return nil, fmt.Errorf("bad role %q", msg.Role)
		}
		if s, ok := unquote(msg.Content); ok {
			s = stripAnthropicNoise(s)
			if s == "" {
				continue
			}
			raw, _ := json.Marshal(map[string]string{"role": msg.Role, "content": s})
			out = append(out, raw)
			continue
		}
		blocks, ok := unmarshalBlocks(msg.Content)
		if !ok {
			return nil, fmt.Errorf("bad message content")
		}
		var texts []string
		var calls []map[string]interface{}
		var imageParts []map[string]interface{}
		flushAssistant := func() {
			if len(texts) == 0 && len(calls) == 0 {
				return
			}
			m := map[string]interface{}{"role": "assistant", "content": strings.Join(texts, "\n")}
			if len(calls) > 0 {
				m["tool_calls"] = calls
			}
			b, _ := json.Marshal(m)
			out = append(out, b)
			texts, calls = nil, nil
		}
		for _, b := range blocks {
			switch b.Type {
			case "", "text":
				if clean := stripAnthropicNoise(b.Text); clean != "" {
					texts = append(texts, clean)
				}
			case "thinking":
				// Old thinking is dropped from upstream history: GLM
				// regenerates reasoning every turn and its thinking is
				// extremely verbose — echoing it back would bloat each
				// request by thousands of tokens and trigger constant
				// context compaction. (The CLI keeps its own copy.)
			case "tool_use":
				input := b.Input
				if len(input) == 0 {
					input = json.RawMessage("{}")
				}
				id := b.ID
				if id == "" {
					id = "call_" + shortID()
				}
				calls = append(calls, map[string]interface{}{
					"id":   id,
					"type": "function",
					"function": map[string]interface{}{
						"name":      b.Name,
						"arguments": string(input),
					},
				})
			case "tool_result":
				flushAssistant()
				text, err := messageText(b.Content)
				if err != nil {
					text = string(b.Content)
				}
				id := b.ToolUseID
				if id == "" {
					id = "unknown"
				}
				toolMsg := map[string]interface{}{
					"role": "tool", "tool_call_id": id, "content": text,
				}
				if b.IsError {
					toolMsg["is_error"] = true
				}
				tb, _ := json.Marshal(toolMsg)
				out = append(out, tb)
			case "image":
				if part := anthropicImagePart(b.Source); part != nil {
					imageParts = append(imageParts, part)
				}
			default:
				if b.Text != "" {
					texts = append(texts, b.Text)
				}
			}
		}
		if msg.Role == "assistant" {
			flushAssistant()
			continue
		}
		if len(imageParts) > 0 {
			// Multimodal user message: typed parts array (text + images).
			content := make([]map[string]interface{}, 0, len(texts)+len(imageParts))
			for _, tp := range texts {
				content = append(content, map[string]interface{}{"type": "text", "text": tp})
			}
			content = append(content, imageParts...)
			b, _ := json.Marshal(map[string]interface{}{"role": msg.Role, "content": content})
			out = append(out, b)
			continue
		}
		if len(texts) > 0 {
			ub, _ := json.Marshal(map[string]string{"role": msg.Role, "content": strings.Join(texts, "\n")})
			out = append(out, ub)
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("messages must not be empty")
	}
	return out, nil
}

// anthropicImagePart converts an Anthropic image block source into an
// OpenAI image_url part (base64 becomes a data: URL, url passes through).
func anthropicImagePart(src json.RawMessage) map[string]interface{} {
	var s struct {
		Type      string `json:"type"`
		MediaType string `json:"media_type"`
		Data      string `json:"data"`
		URL       string `json:"url"`
	}
	if json.Unmarshal(src, &s) != nil {
		return nil
	}
	mk := func(u string) map[string]interface{} {
		return map[string]interface{}{"type": "image_url", "image_url": map[string]interface{}{"url": u}}
	}
	switch s.Type {
	case "base64":
		if s.Data == "" {
			return nil
		}
		mime := s.MediaType
		if mime == "" {
			mime = "image/png"
		}
		return mk("data:" + mime + ";base64," + s.Data)
	case "url":
		if s.URL == "" {
			return nil
		}
		return mk(s.URL)
	}
	return nil
}

func hasImages(messages []json.RawMessage) bool {
	for _, m := range messages {
		var msg struct {
			Content json.RawMessage `json:"content"`
		}
		if json.Unmarshal(m, &msg) != nil {
			continue
		}
		blocks, ok := unmarshalBlocks(msg.Content)
		if !ok {
			continue
		}
		for _, b := range blocks {
			if b.Type == "image" {
				return true
			}
		}
	}
	return false
}

// mapModel maps a Claude-style model id to a GLM one. GLM-looking ids
// pass through untouched; anything else falls back to the default.
func mapModel(m string) string {
	t := strings.Trim(strings.TrimSpace(m), "[]\"' ")
	l := strings.ToLower(t)

	// Direct GL / GLM mapping (claude-opus-gl-5.3, claude-glm-4.7, etc.)
	if strings.Contains(l, "gl") || strings.Contains(l, "x-preview") {
		if strings.Contains(l, "5.3") {
			return "glm-5.3"
		}
		if strings.Contains(l, "5.2") {
			return "glm-5.2"
		}
		if strings.Contains(l, "5v") {
			return "GLM-5v-Turbo"
		}
		if strings.Contains(l, "turbo") {
			return "GLM-5-Turbo"
		}
		if strings.Contains(l, "4.7") {
			return "glm-4.7"
		}
		trimmed := strings.TrimPrefix(l, "claude-")
		return upstream.NormalizeModel(trimmed)
	}

	// Standard Claude aliases
	if strings.Contains(l, "opus-4.8") || strings.Contains(l, "opus-4-8") || strings.Contains(l, "opus-4,8") || strings.Contains(l, "opus-4.6") || strings.Contains(l, "opus-4-6") || strings.Contains(l, "opus") || strings.Contains(l, "3-7") {
		return "glm-5.3"
	}
	if strings.Contains(l, "sonnet") {
		return "glm-5.2"
	}
	if strings.Contains(l, "haiku") {
		return "glm-4.7"
	}

	return upstream.DefaultModel
}

// ── handler ──

func (s *Server) handleMessages(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		anthropicError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
		return
	}
	if s.sess != nil && s.sess.ReadyCount() == 0 {
		upstream.WriteAnthropicOverloaded(w)
		return
	}
	var req anthropicRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 50<<20)).Decode(&req); err != nil {
		anthropicError(w, http.StatusBadRequest, "invalid_request_error", "invalid json body")
		return
	}
	defs := normalizeTools(req.Tools)
	msgs, err := toOpenAI(req)
	if err != nil {
		anthropicError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	// Auxiliary prompts get plain-text answers; a tool_use there is
	// rejected by the CLI and poisons the main history.
	aux := len(defs) > 0 && isAuxRequest(req.Messages)
	model := mapModel(req.Model)
	if hasImages(req.Messages) {
		model = "GLM-5v-Turbo"
	}

	// Stateless by design: Anthropic clients (Claude Code) resend their
	// FULL history every turn. Prepending server memory here would
	// duplicate it (H + (H+new) + ...) and explode input token usage
	// exponentially — the cause of constant context compaction.
	// X-Session-Id is therefore ignored on this endpoint.
	full := msgs
	if len(req.System) == 0 || string(req.System) == "null" {
		full = injectPersonaToMessages(full)
	}
	if len(defs) > 0 && !aux {
		full = convertForTools(full, buildContract(defs))
	}

	id := "msg_" + util.UUIDv4()[:8]
	var thinkingToggle *bool
	if req.DeepThink != nil {
		thinkingToggle = req.DeepThink
	} else if req.Thinking != nil {
		enabled := req.Thinking.Type != "disabled"
		thinkingToggle = &enabled
	} else if !req.Stream {
		// Non-streaming requests default to no thinking to prevent client timeouts
		off := false
		thinkingToggle = &off
	}

	opts := upstream.ChatOpts{
		WebSearch:       req.WebSearch,
		Thinking:        thinkingToggle,
		ReasoningEffort: req.ReasoningEffort,
	}
	if req.Stream {
		s.serveAnthropicStream(w, r, id, req.Model, model, full, defs, aux, opts)
		return
	}
	text, reason, err := upstream.Chat(r.Context(), s.sess, s.tokens, model, full, nil, nil, s.verbose(), opts)
	if err != nil {
		anthropicFail(w, err)
		return
	}
	var calls []toolCall
	if len(defs) > 0 && !aux {
		calls = parseToolCalls(text, defs)
	}
	residual := sanitizeOutputText(text)
	if len(calls) > 0 {
		residual = sanitizeOutputText(stripToolBlocksForDefs(text, defs))
		if isPreambleFluff(residual) {
			residual = ""
		}
	}
	content := []interface{}{}
	if strings.TrimSpace(reason) != "" {
		content = append(content, map[string]interface{}{
			"type": "thinking", "thinking": reason,
			// No verifiable signature exists for Z.AI reasoning;
			// clients must treat this block as unsigned.
			"signature": "unsigned-glm-aki-proxy",
		})
	}
	if strings.TrimSpace(residual) != "" || (len(content) == 0 && len(calls) == 0) {
		content = append(content, map[string]interface{}{"type": "text", "text": residual})
	}
	stop := "end_turn"
	for _, c := range calls {
		content = append(content, map[string]interface{}{
			"type": "tool_use", "id": c.ID, "name": c.Name, "input": c.argsMap(),
		})
		stop = "tool_use"
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"id": id, "type": "message", "role": "assistant", "model": req.Model,
		"content":     content,
		"stop_reason": stop,
		"usage": map[string]interface{}{
			"input_tokens":  util.EstimateTokens(upstream.FlattenPrompt(full)),
			"output_tokens": util.EstimateTokens(text) + util.EstimateTokens(reason),
		},
	})
}

// assistantToolRecord serializes an assistant turn (with tool calls)
// into OpenAI shape for conversation memory.
func assistantToolRecord(residual string, calls []toolCall) json.RawMessage {
	m := map[string]interface{}{"role": "assistant", "content": residual}
	if len(calls) > 0 {
		var tc []interface{}
		for _, c := range calls {
			tc = append(tc, map[string]interface{}{
				"id": c.ID, "type": "function",
				"function": map[string]interface{}{"name": c.Name, "arguments": c.Arguments},
			})
		}
		m["tool_calls"] = tc
	}
	raw, _ := json.Marshal(m)
	return raw
}

// ── streaming (Anthropic SSE event dialect) ──

// emitToolComplete publishes a validated tool call as one sequential
// Anthropic content block.
func emitToolComplete(emit func(string, interface{}), nextIdx *int, c toolCall) {
	idx := *nextIdx
	*nextIdx++
	emit("content_block_start", map[string]interface{}{
		"type": "content_block_start", "index": idx,
		"content_block": map[string]interface{}{
			"type": "tool_use", "id": c.ID, "name": c.Name, "input": map[string]interface{}{},
		},
	})
	if c.Arguments != "" {
		emit("content_block_delta", map[string]interface{}{
			"type": "content_block_delta", "index": idx,
			"delta": map[string]interface{}{"type": "input_json_delta", "partial_json": c.Arguments},
		})
	}
	emit("content_block_stop", map[string]interface{}{"type": "content_block_stop", "index": idx})
}

func (s *Server) serveAnthropicStream(w http.ResponseWriter, r *http.Request,
	msgID, reqModel, model string, full []json.RawMessage,
	defs []ToolDef, aux bool, opts upstream.ChatOpts) {
	chat := func(onText, onReason func(string)) (string, string, error) {
		return upstream.Chat(r.Context(), s.sess, s.tokens, model, full,
			onText, onReason, s.verbose(), opts)
	}
	writeAnthropicStream(w, msgID, reqModel, full, defs, aux, opts, chat)
}

func writeAnthropicStream(w http.ResponseWriter, msgID, reqModel string,
	full []json.RawMessage, defs []ToolDef, aux bool, opts upstream.ChatOpts,
	chat func(func(string), func(string)) (string, string, error)) {

	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache, no-transform")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	flusher, ok := w.(http.Flusher)
	if !ok {
		anthropicError(w, http.StatusInternalServerError, "api_error", "streaming unsupported")
		return
	}
	flusher.Flush()
	var emitMu sync.Mutex
	emit := func(event string, v interface{}) {
		emitMu.Lock()
		defer emitMu.Unlock()
		raw, _ := json.Marshal(v)
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, raw)
		flusher.Flush()
	}
	emitKeepAlive := func() {
		emitMu.Lock()
		defer emitMu.Unlock()
		defer func() { recover() }()
		fmt.Fprintf(w, ": keep-alive\n\n")
		flusher.Flush()
	}
	inTokens := util.EstimateTokens(upstream.FlattenPrompt(full))
	emit("message_start", map[string]interface{}{
		"type": "message_start",
		"message": map[string]interface{}{
			"id": msgID, "type": "message", "role": "assistant", "model": reqModel,
			"content": []interface{}{}, "stop_reason": nil,
			"usage": map[string]interface{}{"input_tokens": inTokens, "output_tokens": 0},
		},
	})
	// Open the thinking block before the upstream request. Z.AI can stay
	// silent for seconds, and clients can show the active thinking state.
	var acc, accWhy strings.Builder
	thinkIdx, textIdx := 0, -1
	nextIdx := 0
	thinkOpen, textOpen := false, false
	// Explicit thinking:{type:"disabled"} must not receive a thinking
	// block. With no override, streaming uses the upstream default.
	thinkingEnabled := opts.Thinking == nil || *opts.Thinking
	if thinkingEnabled {
		emit("content_block_start", map[string]interface{}{
			"type": "content_block_start", "index": nextIdx,
			"content_block": map[string]interface{}{"type": "thinking", "thinking": ""},
		})
		thinkOpen = true
		nextIdx++
	}
	toolEnabled := len(defs) > 0 && !aux
	closeThink := func() {
		if !thinkOpen {
			return
		}
		emit("content_block_delta", map[string]interface{}{
			"type":  "content_block_delta",
			"index": thinkIdx,
			"delta": map[string]interface{}{
				"type":      "signature_delta",
				"signature": "unsigned-glm-aki-proxy-" + shortID(),
			},
		})
		emit("content_block_stop", map[string]interface{}{
			"type":  "content_block_stop",
			"index": thinkIdx,
		})
		thinkOpen = false
	}
	openText := func() {
		if textOpen {
			return
		}
		if thinkOpen {
			closeThink()
		}
		if textIdx < 0 {
			textIdx = nextIdx
			nextIdx++
		}
		emit("content_block_start", map[string]interface{}{
			"type": "content_block_start", "index": textIdx,
			"content_block": map[string]interface{}{"type": "text", "text": ""},
		})
		textOpen = true
	}
	// Keep answer text private until the upstream turn is complete. Z.AI
	// can append <details> reasoning after an answer prefix; publishing
	// that prefix would close the thinking block before reasoning arrives.
	prepareTool := func() {
		// Anthropic content blocks are strictly sequential. A tool_use block
		// cannot start while the thinking or text block is still open.
		if thinkOpen {
			closeThink()
		}
		if textOpen {
			emit("content_block_stop", map[string]interface{}{"type": "content_block_stop", "index": textIdx})
			textOpen = false
		}
	}
	keepAliveStop := make(chan struct{})
	var keepAliveWG sync.WaitGroup
	keepAliveWG.Add(1)
	go func() {
		defer keepAliveWG.Done()
		defer func() { recover() }()
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				emitKeepAlive()
			case <-keepAliveStop:
				return
			}
		}
	}()

	text, reason, err := chat(
		func(d string) { acc.WriteString(d) },
		func(d string) {
			accWhy.WriteString(d)
			if !thinkingEnabled {
				return
			}
			emit("content_block_delta", map[string]interface{}{
				"type": "content_block_delta", "index": thinkIdx,
				"delta": map[string]interface{}{"type": "thinking_delta", "thinking": d},
			})
		})
	close(keepAliveStop)
	keepAliveWG.Wait()
	streamErr := err
	if streamErr != nil {
		prepareTool()
		openText()
		emit("content_block_delta", map[string]interface{}{
			"type": "content_block_delta", "index": textIdx,
			"delta": map[string]interface{}{"type": "text_delta", "text": "\n[upstream error: " + streamErr.Error() + "]"},
		})
	}
	if text == "" {
		text = acc.String()
	}
	if reason == "" {
		reason = accWhy.String()
	}
	var calls []toolCall
	if toolEnabled && streamErr == nil {
		// The full response is authoritative. Parsing here also covers the
		// legacy XML/function syntax that was intentionally held from the
		// live stream.
		calls = parseToolCalls(text, defs)
	}
	residual := sanitizeOutputText(text)
	if len(calls) > 0 {
		residual = sanitizeOutputText(stripToolBlocksForDefs(text, defs))
		if isPreambleFluff(residual) {
			residual = ""
		}
	}
	if strings.TrimSpace(residual) != "" && streamErr == nil {
		openText()
		emit("content_block_delta", map[string]interface{}{
			"type": "content_block_delta", "index": textIdx,
			"delta": map[string]interface{}{"type": "text_delta", "text": residual},
		})
	}
	if len(calls) > 0 {
		prepareTool()
		for i := range calls {
			if calls[i].ID == "" {
				calls[i].ID = fmt.Sprintf("call_net_%d", i)
			}
			emitToolComplete(emit, &nextIdx, calls[i])
		}
	}
	// Stateless: nothing is stored (see handleMessages).
	if thinkOpen {
		closeThink()
	}
	if textOpen {
		emit("content_block_stop", map[string]interface{}{"type": "content_block_stop", "index": textIdx})
	}
	stop := "end_turn"
	if len(calls) > 0 {
		stop = "tool_use"
	}
	emit("message_delta", map[string]interface{}{
		"type":  "message_delta",
		"delta": map[string]interface{}{"stop_reason": stop},
		"usage": map[string]interface{}{"output_tokens": util.EstimateTokens(text) + util.EstimateTokens(reason)},
	})
	emit("message_stop", map[string]interface{}{"type": "message_stop"})
}

// ── count tokens (Anthropic shape) ──

func (s *Server) handleCountTokens(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		anthropicError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
		return
	}
	var req anthropicRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 50<<20)).Decode(&req); err != nil {
		anthropicError(w, http.StatusBadRequest, "invalid_request_error", "invalid json body")
		return
	}
	defs := normalizeTools(req.Tools)
	msgs, err := toOpenAI(req)
	if err != nil {
		anthropicError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	full := msgs
	if len(defs) > 0 && !isAuxRequest(req.Messages) {
		full = convertForTools(full, buildContract(defs))
	}
	tokens := util.EstimateTokens(upstream.FlattenPrompt(full))
	if tokens < 1 {
		tokens = 1
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"input_tokens": tokens,
	})
}

// ── errors (Anthropic shape) ──

func anthropicError(w http.ResponseWriter, code int, kind, msg string) {
	writeJSON(w, code, map[string]interface{}{
		"type":  "error",
		"error": map[string]interface{}{"type": kind, "message": msg},
	})
}

func anthropicFail(w http.ResponseWriter, err error) {
	if errors.Is(err, upstream.ErrPoolOverloaded) || strings.Contains(err.Error(), "overloaded") || strings.Contains(err.Error(), "in cooldown") {
		upstream.WriteAnthropicOverloaded(w)
		return
	}
	msg := err.Error()
	code := http.StatusBadGateway
	if strings.Contains(msg, "captcha") || strings.Contains(msg, "pool") {
		code = http.StatusServiceUnavailable
	}
	if strings.Contains(msg, "401") || strings.Contains(msg, "expired") {
		code = http.StatusUnauthorized
	}
	anthropicError(w, code, "api_error", msg)
}
