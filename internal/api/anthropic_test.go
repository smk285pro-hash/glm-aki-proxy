package api

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"glm-aki-proxy/internal/upstream"
)

type anthroEvent struct {
	Type         string `json:"type"`
	Index        int    `json:"index"`
	ContentBlock struct {
		Type string `json:"type"`
		Name string `json:"name"`
	} `json:"content_block"`
	Delta struct {
		Type        string `json:"type"`
		Thinking    string `json:"thinking"`
		Text        string `json:"text"`
		PartialJSON string `json:"partial_json"`
		StopReason  string `json:"stop_reason"`
	} `json:"delta"`
}

func decodeAnthropicEvents(t *testing.T, body string) []anthroEvent {
	t.Helper()
	var events []anthroEvent
	for _, chunk := range strings.Split(body, "\n\n") {
		if chunk == "" {
			continue
		}
		parts := strings.SplitN(chunk, "\ndata: ", 2)
		if len(parts) != 2 {
			t.Fatalf("bad SSE chunk: %q", chunk)
		}
		var e anthroEvent
		if err := json.Unmarshal([]byte(parts[1]), &e); err != nil {
			t.Fatalf("bad SSE data: %v", err)
		}
		events = append(events, e)
	}
	return events
}

func TestAnthropicStreamThinkingAndMalformedTools(t *testing.T) {
	defs := []ToolDef{{Name: "Glob"}, {Name: "Read"}}
	answer := `Mình xem nhanh.Glob(pattern: *</arg_value>Read(file_path: C:\work\PLAN.md</arg_value><arg_key>limit: 60</arg_value>`
	w := httptest.NewRecorder()
	writeAnthropicStream(w, "msg_test", "claude-3-7-sonnet-20250219", nil, defs, false,
		upstream.ChatOpts{}, func(onText, onReason func(string)) (string, string, error) {
			// Z.AI can generate an answer prefix before the reasoning
			// block; Anthropic events still need thinking first.
			onText("Mình xem nhanh.")
			onReason("Đang xét cấu trúc.")
			onText(answer[len("Mình xem nhanh."):])
			return answer, "Đang xét cấu trúc.", nil
		})
	events := decodeAnthropicEvents(t, w.Body.String())
	open := -1
	thinking, text, calls, args := "", "", []string{}, []string{}
	for _, e := range events {
		switch e.Type {
		case "content_block_start":
			if open >= 0 {
				t.Fatalf("overlapping content blocks: %d then %d", open, e.Index)
			}
			open = e.Index
			if e.ContentBlock.Type == "tool_use" {
				calls = append(calls, e.ContentBlock.Name)
				args = append(args, "")
			}
		case "content_block_delta":
			if e.Index != open {
				t.Fatalf("delta for closed block %d (open %d)", e.Index, open)
			}
			switch e.Delta.Type {
			case "thinking_delta":
				thinking += e.Delta.Thinking
			case "text_delta":
				text += e.Delta.Text
			case "input_json_delta":
				args[len(args)-1] += e.Delta.PartialJSON
			}
		case "content_block_stop":
			if e.Index != open {
				t.Fatalf("stop for closed block %d (open %d)", e.Index, open)
			}
			open = -1
		}
	}
	if open != -1 {
		t.Fatalf("unclosed block %d", open)
	}
	if thinking != "Đang xét cấu trúc." || text != "Mình xem nhanh." {
		t.Fatalf("unexpected thinking/text: %q / %q", thinking, text)
	}
	if len(calls) != 2 || calls[0] != "Glob" || calls[1] != "Read" {
		t.Fatalf("unexpected calls: %v", calls)
	}
	var readArgs map[string]interface{}
	if json.Unmarshal([]byte(args[1]), &readArgs) != nil || readArgs["limit"] != float64(60) {
		t.Fatalf("wrong Read args: %q", args[1])
	}
	if events[len(events)-2].Delta.StopReason != "tool_use" || events[len(events)-1].Type != "message_stop" {
		t.Fatalf("wrong stream ending: %+v", events[len(events)-2:])
	}
}

func TestAnthropicStreamDisabledThinking(t *testing.T) {
	off := false
	w := httptest.NewRecorder()
	writeAnthropicStream(w, "msg_test", "glm-5.3", nil, nil, false,
		upstream.ChatOpts{Thinking: &off}, func(onText, onReason func(string)) (string, string, error) {
			onReason("hidden")
			onText("answer")
			return "answer", "hidden", nil
		})
	for _, e := range decodeAnthropicEvents(t, w.Body.String()) {
		if e.ContentBlock.Type == "thinking" || e.Delta.Type == "thinking_delta" {
			t.Fatalf("disabled request emitted thinking: %+v", e)
		}
		if e.Type == "content_block_start" && e.ContentBlock.Type == "text" && e.Index != 0 {
			t.Fatalf("first block without thinking must have index 0, got %d", e.Index)
		}
	}
}
