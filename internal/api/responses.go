package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"glm-aki-proxy/internal/upstream"
	"glm-aki-proxy/internal/util"
)

// ── Request types ──

type responsesRequest struct {
	Model              string            `json:"model"`
	Input              json.RawMessage   `json:"input"`
	Instructions       string            `json:"instructions,omitempty"`
	Stream             bool              `json:"stream"`
	Tools              []json.RawMessage `json:"tools,omitempty"`
	PreviousResponseID string            `json:"previous_response_id,omitempty"`
}

type responsesInputItem struct {
	Type    string          `json:"type"` // message | function_call | function_call_output | reasoning
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content,omitempty"`
	CallID  string          `json:"call_id,omitempty"`
	Name    string          `json:"name,omitempty"`
	Args    string          `json:"arguments,omitempty"`
	Output  string          `json:"output,omitempty"`
	ID      string          `json:"id,omitempty"`
}

type responsesTool struct {
	Type        string                 `json:"type"`
	Name        string                 `json:"name"`
	Description string                 `json:"description,omitempty"`
	Parameters  map[string]interface{} `json:"parameters,omitempty"`
}

// ── Output types ──

type responsesOutputText struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type responsesOutputMessage struct {
	ID      string                `json:"id"`
	Type    string                `json:"type"` // message
	Status  string                `json:"status"`
	Role    string                `json:"role"`
	Content []responsesOutputText `json:"content"`
}

type responsesOutputFunctionCall struct {
	ID        string `json:"id"`
	CallID    string `json:"call_id"`
	Type      string `json:"type"` // function_call
	Status    string `json:"status"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type responsesUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
	TotalTokens  int `json:"total_tokens"`
}

func responsesContentText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &parts) != nil {
		return ""
	}
	var sb strings.Builder
	for _, p := range parts {
		if p.Type == "input_text" || p.Type == "output_text" || p.Type == "text" {
			sb.WriteString(p.Text)
		}
	}
	return sb.String()
}

func convertResponsesInput(raw json.RawMessage) ([]json.RawMessage, error) {
	if len(raw) == 0 {
		return nil, fmt.Errorf("input is required")
	}
	var strInput string
	if json.Unmarshal(raw, &strInput) == nil {
		if strings.TrimSpace(strInput) == "" {
			return nil, fmt.Errorf("input is required")
		}
		b, _ := json.Marshal(map[string]string{"role": "user", "content": strInput})
		return []json.RawMessage{b}, nil
	}

	var items []responsesInputItem
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil, fmt.Errorf("invalid input items: %w", err)
	}

	var msgs []json.RawMessage
	for _, it := range items {
		switch it.Type {
		case "", "message":
			role := it.Role
			if role == "" {
				role = "user"
			}
			if role == "developer" {
				role = "system"
			}
			text := responsesContentText(it.Content)
			if strings.TrimSpace(text) == "" {
				continue
			}
			b, _ := json.Marshal(map[string]string{"role": role, "content": text})
			msgs = append(msgs, b)
		case "function_call":
			if it.Name == "" {
				continue
			}
			callID := it.CallID
			if callID == "" {
				callID = it.ID
			}
			tc := []interface{}{map[string]interface{}{
				"id":   callID,
				"type": "function",
				"function": map[string]interface{}{
					"name":      it.Name,
					"arguments": it.Args,
				},
			}}
			b, _ := json.Marshal(map[string]interface{}{
				"role":       "assistant",
				"content":    "",
				"tool_calls": tc,
			})
			msgs = append(msgs, b)
		case "function_call_output":
			if it.CallID == "" {
				continue
			}
			b, _ := json.Marshal(map[string]string{
				"role":         "tool",
				"tool_call_id": it.CallID,
				"content":      it.Output,
			})
			msgs = append(msgs, b)
		}
	}
	if len(msgs) == 0 {
		return nil, fmt.Errorf("input produced no usable messages")
	}
	return msgs, nil
}

func convertResponsesTools(rawTools []json.RawMessage) []ToolDef {
	var defs []ToolDef
	for _, raw := range rawTools {
		var t responsesTool
		if json.Unmarshal(raw, &t) != nil || t.Type != "function" || t.Name == "" {
			continue
		}
		defs = append(defs, ToolDef{
			Name:        t.Name,
			Description: t.Description,
			Parameters:  t.Parameters,
		})
	}
	return defs
}

// ── Handler ──

func (s *Server) handleResponses(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, errObj("method not allowed — POST /v1/responses"))
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 50*1024*1024)

	var req responsesRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, errObj("invalid JSON: "+err.Error()))
		return
	}
	if req.PreviousResponseID != "" {
		writeJSON(w, http.StatusBadRequest, errObj("previous_response_id is not supported — stateless bridge"))
		return
	}

	msgs, err := convertResponsesInput(req.Input)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errObj(err.Error()))
		return
	}
	if req.Instructions != "" {
		b, _ := json.Marshal(map[string]string{"role": "system", "content": req.Instructions})
		msgs = append([]json.RawMessage{b}, msgs...)
	}

	model := mapModel(req.Model)
	defs := convertResponsesTools(req.Tools)

	full := msgs
	if len(defs) > 0 {
		full = convertForTools(full, buildContract(defs))
	}

	opts := upstream.ChatOpts{}
	requestID := "resp_" + util.UUIDv4()[:8]

	if !req.Stream {
		text, _, err := upstream.Chat(r.Context(), s.sess, s.tokens, model, full, nil, nil, s.verbose(), opts)
		if err != nil {
			fail(w, err)
			return
		}

		var calls []toolCall
		if len(defs) > 0 {
			calls = parseToolCalls(text, defs)
		}
		residual := sanitizeOutputText(text)
		if len(calls) > 0 {
			residual = sanitizeOutputText(stripToolBlocksForDefs(text, defs))
		}

		var outputItems []interface{}
		if strings.TrimSpace(residual) != "" || len(calls) == 0 {
			outputItems = append(outputItems, responsesOutputMessage{
				ID:     "msg_" + util.UUIDv4()[:8],
				Type:   "message",
				Status: "completed",
				Role:   "assistant",
				Content: []responsesOutputText{{
					Type: "output_text",
					Text: residual,
				}},
			})
		}
		for i, c := range calls {
			callID := c.ID
			if callID == "" {
				callID = fmt.Sprintf("call_%s_%d", requestID, i)
			}
			outputItems = append(outputItems, responsesOutputFunctionCall{
				ID:        fmt.Sprintf("item_%s_%d", requestID, i),
				CallID:    callID,
				Type:      "function_call",
				Status:    "completed",
				Name:      c.Name,
				Arguments: c.Arguments,
			})
		}

		inTokens := util.EstimateTokens(upstream.FlattenPrompt(full))
		outTokens := util.EstimateTokens(text)
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"id":          requestID,
			"object":      "response",
			"created_at":  time.Now().Unix(),
			"status":      "completed",
			"model":       req.Model,
			"output":      outputItems,
			"usage": responsesUsage{
				InputTokens:  inTokens,
				OutputTokens: outTokens,
				TotalTokens:  inTokens + outTokens,
			},
		})
		return
	}

	// ── Streaming Responses SSE ──
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeJSON(w, http.StatusInternalServerError, errObj("streaming unsupported"))
		return
	}
	flusher.Flush()

	emitSSE := func(event string, data interface{}) {
		raw, _ := json.Marshal(data)
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, raw)
		flusher.Flush()
	}

	emitSSE("response.created", map[string]interface{}{
		"response": map[string]interface{}{
			"id":         requestID,
			"object":     "response",
			"created_at": time.Now().Unix(),
			"status":     "in_progress",
			"model":      req.Model,
		},
	})

	var acc strings.Builder
	hasTools := len(defs) > 0
	text, _, err := upstream.Chat(r.Context(), s.sess, s.tokens, model, full,
		func(d string) {
			acc.WriteString(d)
			if !hasTools {
				emitSSE("output_text.delta", map[string]interface{}{
					"delta": d,
				})
			}
		}, nil, s.verbose(), opts)

	if err != nil {
		emitSSE("response.failed", map[string]interface{}{
			"error": err.Error(),
		})
		return
	}

	if text == "" {
		text = acc.String()
	}

	var calls []toolCall
	if hasTools {
		calls = parseToolCalls(text, defs)
	}
	residual := sanitizeOutputText(text)
	if len(calls) > 0 {
		residual = sanitizeOutputText(stripToolBlocksForDefs(text, defs))
	}

	if hasTools && strings.TrimSpace(residual) != "" {
		emitSSE("output_text.delta", map[string]interface{}{
			"delta": residual,
		})
	}
	for i, c := range calls {
		callID := c.ID
		if callID == "" {
			callID = fmt.Sprintf("call_%s_%d", requestID, i)
		}
		emitSSE("output_item.added", map[string]interface{}{
			"item": responsesOutputFunctionCall{
				ID:        fmt.Sprintf("item_%s_%d", requestID, i),
				CallID:    callID,
				Type:      "function_call",
				Status:    "completed",
				Name:      c.Name,
				Arguments: c.Arguments,
			},
		})
	}

	inTokens := util.EstimateTokens(upstream.FlattenPrompt(full))
	outTokens := util.EstimateTokens(text)

	emitSSE("response.completed", map[string]interface{}{
		"response": map[string]interface{}{
			"id":         requestID,
			"object":     "response",
			"created_at": time.Now().Unix(),
			"status":     "completed",
			"model":      req.Model,
			"usage": responsesUsage{
				InputTokens:  inTokens,
				OutputTokens: outTokens,
				TotalTokens:  inTokens + outTokens,
			},
		},
	})
	fmt.Fprintf(w, "data: [DONE]\n\n")
	flusher.Flush()
}
