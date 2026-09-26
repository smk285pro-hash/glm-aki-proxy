package api

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
	"unicode/utf8"

	"glm-aki-proxy/internal/upstream"
)

// ══════════════════════════════════════════════════════════════════
// CHALLENGER 2: ADVERSARIAL STRESS TEST SUITE
// ══════════════════════════════════════════════════════════════════

// Test 1A: Streaming Preamble Suppression - Supported Preambles (no internal dots)
func TestAdversarial_StreamingPreambleSuppression_HappyPath(t *testing.T) {
	defs := []ToolDef{
		{
			Name:        "Read",
			Description: "Read file contents",
			Parameters: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"file_path": map[string]interface{}{"type": "string"},
				},
				"required": []interface{}{"file_path"},
			},
		},
	}

	preambles := []string{
		"Tôi sẽ dùng công cụ Read để đọc file.",
		"Tôi sẽ sử dụng công cụ Read để kiểm tra.",
		"Theo quy tắc công cụ, tôi sẽ gọi Read.",
		"Theo protocol, tôi sẽ thực hiện thao tác sau:",
		"I will use tool Read to check the file.",
		"Let me read the file contents:",
		"Calling tool now...",
		"Executing tool...",
		"Dưới đây là thao tác gọi công cụ:",
	}

	for _, preamble := range preambles {
		t.Run("Preamble_"+preamble[:min(len(preamble), 20)], func(t *testing.T) {
			rawStreamOutput := fmt.Sprintf("%s\n<<<TOOL_CALL>>>\n{\"name\":\"Read\",\"arguments\":{\"file_path\":\"config.go\"}}\n<<<END_TOOL_CALL>>>", preamble)

			w := httptest.NewRecorder()
			opts := upstream.ChatOpts{}

			writeAnthropicStream(w, "msg_stream_preamble", "claude-3-7-sonnet-20250219", nil, defs, false, opts,
				func(onText, onReason func(string)) (string, string, error) {
					onText(rawStreamOutput)
					return rawStreamOutput, "", nil
				})

			events := decodeAnthropicEvents(t, w.Body.String())

			var textDeltas []string
			var toolCalls []string
			stopReason := ""

			for _, e := range events {
				if e.Type == "content_block_start" && e.ContentBlock.Type == "tool_use" {
					toolCalls = append(toolCalls, e.ContentBlock.Name)
				}
				if e.Type == "content_block_delta" && e.Delta.Type == "text_delta" {
					textDeltas = append(textDeltas, e.Delta.Text)
				}
				if e.Type == "message_delta" && e.Delta.StopReason != "" {
					stopReason = e.Delta.StopReason
				}
			}

			// Invariant 1: Preamble must be completely suppressed from text deltas
			if len(textDeltas) > 0 {
				t.Errorf("expected 0 text deltas, got %d: %q", len(textDeltas), strings.Join(textDeltas, ""))
			}

			// Invariant 2: Exactly 1 tool_use block produced with name 'Read'
			if len(toolCalls) != 1 || toolCalls[0] != "Read" {
				t.Errorf("expected 1 tool call 'Read', got %v", toolCalls)
			}

			// Invariant 3: stop_reason MUST be 'tool_use'
			if stopReason != "tool_use" {
				t.Errorf("expected stop_reason 'tool_use', got %q", stopReason)
			}
		})
	}
}

// Test 1B: Empirical Bug Reproduction: Preambles with filenames containing periods (e.g. config.go, main.py) leak into text deltas
func TestAdversarial_StreamingPreambleSuppression_DotInFilenameBug(t *testing.T) {
	defs := []ToolDef{
		{
			Name:        "Read",
			Description: "Read file contents",
			Parameters:  map[string]interface{}{"type": "object"},
		},
	}

	buggyPreambles := []struct {
		preamble string
		reason   string
	}{
		{
			preamble: "Tôi sẽ dùng công cụ Read để đọc file config.go.",
			reason:   "dot in config.go breaks [^.\\n]*",
		},
		{
			preamble: "Tôi sẽ dùng tool Read để đọc file main.go",
			reason:   "dot in main.go and no trailing punctuation",
		},
		{
			preamble: "I will use tool Read to check main.go.",
			reason:   "dot in main.go breaks English preamble",
		},
		{
			preamble: "Let me check package.json using tool Read.",
			reason:   "dot in package.json breaks Let me preamble",
		},
		{
			preamble: "Tôi sẽ kiểm tra file README.md.",
			reason:   "dot in README.md breaks Vietnamese preamble",
		},
	}

	for _, tc := range buggyPreambles {
		t.Run("BugRepro_"+tc.preamble[:min(len(tc.preamble), 25)], func(t *testing.T) {
			rawStreamOutput := fmt.Sprintf("%s\n<<<TOOL_CALL>>>\n{\"name\":\"Read\",\"arguments\":{\"file_path\":\"config.go\"}}\n<<<END_TOOL_CALL>>>", tc.preamble)

			w := httptest.NewRecorder()
			opts := upstream.ChatOpts{}

			writeAnthropicStream(w, "msg_stream_bug", "claude-3-7-sonnet-20250219", nil, defs, false, opts,
				func(onText, onReason func(string)) (string, string, error) {
					onText(rawStreamOutput)
					return rawStreamOutput, "", nil
				})

			events := decodeAnthropicEvents(t, w.Body.String())

			var textDeltas []string
			for _, e := range events {
				if e.Type == "content_block_delta" && e.Delta.Type == "text_delta" {
					textDeltas = append(textDeltas, e.Delta.Text)
				}
			}

			// Verify that the preamble is detected as fluff and suppressed (0 text deltas)
			fluffDetected := isPreambleFluff(tc.preamble)
			t.Logf("Preamble %q -> isPreambleFluff = %v, leaked textDeltas = %v (Reason: %s)",
				tc.preamble, fluffDetected, textDeltas, tc.reason)

			if !fluffDetected {
				t.Errorf("FAIL: isPreambleFluff(%q) = false, want true (%s)", tc.preamble, tc.reason)
			}
			if len(textDeltas) > 0 {
				t.Errorf("expected 0 text deltas, got %d: %q", len(textDeltas), strings.Join(textDeltas, ""))
			}
		})
	}
}

// Test 2A: Non-Streaming Preamble Suppression Happy Path
func TestAdversarial_NonStreamingPreambleSuppression_HappyPath(t *testing.T) {
	defs := []ToolDef{
		{
			Name:        "Read",
			Description: "Read file contents",
			Parameters:  map[string]interface{}{"type": "object"},
		},
	}

	preambles := []string{
		"Tôi sẽ dùng công cụ Read để đọc file.",
		"Tôi sẽ sử dụng công cụ Read để xem nội dung.",
		"Theo quy tắc, tôi sẽ gọi công cụ sau:",
		"I will use tool Read to inspect the repository.",
		"Let me call the tool now:",
	}

	for _, preamble := range preambles {
		t.Run("NonStreaming_"+preamble[:min(len(preamble), 20)], func(t *testing.T) {
			text := fmt.Sprintf("%s\n<<<TOOL_CALL>>>\n{\"name\":\"Read\",\"arguments\":{\"file_path\":\"main.go\"}}\n<<<END_TOOL_CALL>>>", preamble)

			calls := parseToolCalls(text, defs)
			if len(calls) != 1 || calls[0].Name != "Read" {
				t.Fatalf("failed to parse tool call: %v", calls)
			}

			residual := sanitizeOutputText(stripToolBlocksForDefs(text, defs))
			if isPreambleFluff(residual) {
				residual = ""
			}

			if residual != "" {
				t.Errorf("preamble was not suppressed in non-streaming residual: %q", residual)
			}
		})
	}
}

// Test 2B: Non-Streaming Preamble Suppression Bug Reproduction (Dot in filename)
func TestAdversarial_NonStreamingPreambleSuppression_DotInFilenameBug(t *testing.T) {
	defs := []ToolDef{
		{
			Name:        "Read",
			Description: "Read file contents",
			Parameters:  map[string]interface{}{"type": "object"},
		},
	}

	buggyPreambles := []string{
		"Tôi sẽ dùng công cụ Read để đọc file config.go.",
		"Tôi sẽ dùng tool Read để đọc file main.go",
		"I will use tool Read to inspect main.go.",
	}

	for _, preamble := range buggyPreambles {
		t.Run("NonStreamingBug_"+preamble[:min(len(preamble), 25)], func(t *testing.T) {
			text := fmt.Sprintf("%s\n<<<TOOL_CALL>>>\n{\"name\":\"Read\",\"arguments\":{\"file_path\":\"main.go\"}}\n<<<END_TOOL_CALL>>>", preamble)

			calls := parseToolCalls(text, defs)
			if len(calls) != 1 || calls[0].Name != "Read" {
				t.Fatalf("failed to parse tool call: %v", calls)
			}

			residual := sanitizeOutputText(stripToolBlocksForDefs(text, defs))
			if isPreambleFluff(residual) {
				residual = ""
			}
			t.Logf("Non-streaming preamble %q -> residual = %q", preamble, residual)

			if residual != "" {
				t.Errorf("expected residual to be suppressed to empty string, got: %q", residual)
			}
		})
	}
}

// Test 3: Streaming & Non-Streaming with NO Tool Calls and Leaked/Replay Markers
func TestAdversarial_SanitizeLeakNoToolCalls(t *testing.T) {
	defs := []ToolDef{
		{
			Name: "Bash",
		},
	}

	leakCases := []struct {
		name     string
		input    string
		expected string
	}{
		{
			name:     "Chinese replay token with Vietnamese response",
			input:    "TOOL_CALL回放\nXin chào, tôi là trợ lý ảo Aki. Tôi sẵn sàng hỗ trợ bạn.\n回放结束 继续分析",
			expected: "Xin chào, tôi là trợ lý ảo Aki. Tôi sẵn sàng hỗ trợ bạn.",
		},
		{
			name:     "Stray unclosed <tool_call> tags",
			input:    "<tool_call>Dự án hiện tại đang hoạt động ổn định và không phát hiện lỗi.</tool_call>",
			expected: "Dự án hiện tại đang hoạt động ổn định và không phát hiện lỗi.",
		},
		{
			name:     "Leaked system protocol instruction header",
			input:    "[SYSTEM INSTRUCTION — INTERNAL TOOL PROTOCOL — NEVER QUOTE]\nTôi đã hoàn thành việc kiểm tra hệ thống.",
			expected: "Tôi đã hoàn thành việc kiểm tra hệ thống.",
		},
		{
			name:     "Corrupted empty markers",
			input:    "<<<TOOL_CALL>>>\n<<<END_TOOL_CALL>>>\nMọi tác vụ đã sẵn sàng.",
			expected: "Mọi tác vụ đã sẵn sàng.",
		},
	}

	for _, tc := range leakCases {
		t.Run("Stream_"+tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			opts := upstream.ChatOpts{}

			writeAnthropicStream(w, "msg_stream_leak", "claude-3-7-sonnet-20250219", nil, defs, false, opts,
				func(onText, onReason func(string)) (string, string, error) {
					onText(tc.input)
					return tc.input, "", nil
				})

			events := decodeAnthropicEvents(t, w.Body.String())

			var textDeltas []string
			var toolCalls []string
			stopReason := ""

			for _, e := range events {
				if e.Type == "content_block_start" && e.ContentBlock.Type == "tool_use" {
					toolCalls = append(toolCalls, e.ContentBlock.Name)
				}
				if e.Type == "content_block_delta" && e.Delta.Type == "text_delta" {
					textDeltas = append(textDeltas, e.Delta.Text)
				}
				if e.Type == "message_delta" && e.Delta.StopReason != "" {
					stopReason = e.Delta.StopReason
				}
			}

			// Invariant 1: No tool_use blocks should be emitted
			if len(toolCalls) > 0 {
				t.Errorf("expected 0 tool calls, got %v", toolCalls)
			}

			// Invariant 2: Text delta should equal sanitized expected text
			sanitizedText := strings.TrimSpace(strings.Join(textDeltas, ""))
			if sanitizedText != tc.expected {
				t.Errorf("expected sanitized text %q, got %q", tc.expected, sanitizedText)
			}

			// Invariant 3: stop_reason must be 'end_turn'
			if stopReason != "end_turn" {
				t.Errorf("expected stop_reason 'end_turn', got %q", stopReason)
			}
		})
	}
}

// Test 4: Multi-Turn Contract Reinforcement Over 5+ Turns
func TestAdversarial_MultiTurnContractReinforcementOver5Turns(t *testing.T) {
	contractFraming := "[SYSTEM INSTRUCTION — INTERNAL TOOL PROTOCOL]\n<<<TOOL_CALL>>>..."

	// Simulate 6 turns (Initial User Prompt + 5 tool iterations + final answer)
	// Claude Code sends full cumulative history on every turn.
	type stepHistory struct {
		turnName string
		messages []json.RawMessage
	}

	makeRaw := func(v interface{}) json.RawMessage {
		b, _ := json.Marshal(v)
		return b
	}

	var history []json.RawMessage

	// Turn 1: User asks initial question
	history = append(history, makeRaw(map[string]interface{}{
		"role":    "user",
		"content": "Hãy phân tích và tối ưu hóa hệ thống.",
	}))

	converted1 := convertForTools(history, contractFraming)
	if len(converted1) != 1 {
		t.Fatalf("turn 1: expected 1 message, got %d", len(converted1))
	}
	var m1 map[string]interface{}
	json.Unmarshal(converted1[0], &m1)
	if !strings.Contains(m1["content"].(string), contractFraming) {
		t.Fatalf("turn 1: user message missing contract framing")
	}

	// Loop turns 2 to 6 (5 consecutive tool interactions)
	toolNames := []string{"Glob", "Read", "Bash", "Edit", "Bash"}
	toolArgs := []string{
		`{"pattern":"*.go"}`,
		`{"file_path":"main.go"}`,
		`{"command":"go build"}`,
		`{"file_path":"main.go","text":"// optimized"}`,
		`{"command":"go test ./..."}`,
	}
	toolResults := []string{
		`["main.go", "config.go"]`,
		`package main\nfunc main() {}`,
		`Build succeeded`,
		`File edited successfully`,
		`PASS: ok 0.12s`,
	}

	for turn := 0; turn < 5; turn++ {
		callID := fmt.Sprintf("call_%d", turn+1)
		// Assistant emits tool call
		history = append(history, makeRaw(map[string]interface{}{
			"role": "assistant",
			"tool_calls": []interface{}{
				map[string]interface{}{
					"id":   callID,
					"type": "function",
					"function": map[string]interface{}{
						"name":      toolNames[turn],
						"arguments": toolArgs[turn],
					},
				},
			},
		}))

		// User/Environment responds with tool result
		history = append(history, makeRaw(map[string]interface{}{
			"role":         "tool",
			"tool_call_id": callID,
			"content":      toolResults[turn],
		}))

		// Execute convertForTools on the cumulative history
		converted := convertForTools(history, contractFraming)

		// Assertions for this turn:
		// 1. Initial user prompt (index 0) still carries the original contract framing
		var firstMsg map[string]interface{}
		json.Unmarshal(converted[0], &firstMsg)
		if !strings.Contains(firstMsg["content"].(string), contractFraming) {
			t.Errorf("turn %d: initial user prompt lost contract framing: %v", turn+2, firstMsg["content"])
		}

		// 2. The latest message (last tool result) MUST have the instruction reminder
		lastIdx := len(converted) - 1
		var lastMsg map[string]interface{}
		json.Unmarshal(converted[lastIdx], &lastMsg)
		lastContent := lastMsg["content"].(string)

		expectedReminder := "[Instruction: Continue the task in the user's language. If another action is needed, emit <<<TOOL_CALL>>> directly with valid JSON."
		if !strings.Contains(lastContent, expectedReminder) {
			t.Errorf("turn %d: latest tool result missing reinforcement reminder! Content: %s", turn+2, lastContent)
		}

		// 3. Prior tool results MUST NOT have duplicate reminders (prevent context bloat)
		for j := 1; j < lastIdx; j++ {
			var midMsg map[string]interface{}
			json.Unmarshal(converted[j], &midMsg)
			midContent, _ := midMsg["content"].(string)
			if strings.Contains(midContent, "[Tool result for") && strings.Contains(midContent, expectedReminder) {
				t.Errorf("turn %d: prior tool result at index %d has redundant reminder: %s", turn+2, j, midContent)
			}
		}

		// 4. Past assistant tool calls are cleanly encapsulated in <<<TOOL_CALL>>> blocks
		for j := 1; j < lastIdx; j++ {
			var midMsg map[string]interface{}
			json.Unmarshal(converted[j], &midMsg)
			if midMsg["role"] == "assistant" {
				midContent, _ := midMsg["content"].(string)
				if !strings.Contains(midContent, toolBlockStart) || !strings.Contains(midContent, toolBlockEnd) {
					t.Errorf("turn %d: past assistant message at index %d not encapsulated with toolBlock markers: %s", turn+2, j, midContent)
				}
			}
		}
	}

	// Turn 7: Parallel tool invocation test (Assistant calls 2 tools in parallel, User returns 2 tool_results)
	history = append(history, makeRaw(map[string]interface{}{
		"role": "assistant",
		"tool_calls": []interface{}{
			map[string]interface{}{
				"id":       "call_parallel_1",
				"type":     "function",
				"function": map[string]interface{}{"name": "Glob", "arguments": `{"pattern":"*.md"}`},
			},
			map[string]interface{}{
				"id":       "call_parallel_2",
				"type":     "function",
				"function": map[string]interface{}{"name": "Read", "arguments": `{"file_path":"README.md"}`},
			},
		},
	}))
	history = append(history, makeRaw(map[string]interface{}{
		"role":         "tool",
		"tool_call_id": "call_parallel_1",
		"content":      `["README.md"]`,
	}))
	history = append(history, makeRaw(map[string]interface{}{
		"role":         "tool",
		"tool_call_id": "call_parallel_2",
		"content":      `# Project Documentation`,
	}))

	convertedParallel := convertForTools(history, contractFraming)
	lastIdx := len(convertedParallel) - 1
	penultimateIdx := lastIdx - 1

	var lastParallelMsg, penultParallelMsg map[string]interface{}
	json.Unmarshal(convertedParallel[lastIdx], &lastParallelMsg)
	json.Unmarshal(convertedParallel[penultimateIdx], &penultParallelMsg)

	expectedReminder := "[Instruction: Continue the task in the user's language."
	if !strings.Contains(lastParallelMsg["content"].(string), expectedReminder) {
		t.Errorf("turn 7 parallel: last tool result missing reinforcement reminder: %s", lastParallelMsg["content"])
	}
	if strings.Contains(penultParallelMsg["content"].(string), expectedReminder) {
		t.Errorf("turn 7 parallel: penultimate tool result should NOT have duplicate reminder: %s", penultParallelMsg["content"])
	}
}

// Test 5: UTF-8 Description Rune Safety with Emoji, Vietnamese, and CJK
func TestAdversarial_UTF8DescriptionRuneSafety(t *testing.T) {
	testCases := []struct {
		name          string
		desc          string
		expectTrunc   bool
		minByteLength int
		runeLength    int
	}{
		{
			name:          "Vietnamese >80 bytes but <80 runes (should NOT truncate)",
			desc:          "Công cụ đọc và hiển thị nội dung tệp tin mã nguồn của dự án một cách an toàn.",
			expectTrunc:   false,
			minByteLength: 85,
			runeLength:    76,
		},
		{
			name:          "Vietnamese >80 runes (should truncate cleanly without corrupting runes)",
			desc:          "Công cụ này được thiết kế để tự động quét toàn bộ mã nguồn của dự án, phát hiện lỗi bảo mật và kiểm tra các quy chuẩn lập trình một cách chi tiết và chính xác.",
			expectTrunc:   true,
			minByteLength: 180,
			runeLength:    162,
		},
		{
			name:          "Emoji heavy >80 bytes but <80 runes (4 bytes per rune, should NOT truncate)",
			desc:          "🔍 Đọc tệp tin 🚀 Khởi chạy server ⚡ Tối ưu hoá bộ nhớ 💻 Phân tích mã nguồn 🔧",
			expectTrunc:   false,
			minByteLength: 95,
			runeLength:    74,
		},
		{
			name:          "Emoji + CJK >80 runes (should truncate cleanly at rune boundary)",
			desc:          "🛠️ 源代码安全检查工具：全面检测代码漏洞，自动修复缺陷并优化系统性能。🚀 💻 🔍 ⚡ 📦 🔧 🎯 🔒 🛡️ 📊 📈 📉 📜 📝 📋 📌 📍 📎 📏 📐 📑 📒 📓 📕 📖 📗 📘 📙 📚 🏷️ 🔑 🗝️ 🔨 🪓",
			expectTrunc:   true,
			minByteLength: 200,
			runeLength:    110,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			if len(tc.desc) < tc.minByteLength {
				t.Fatalf("test setup error: byte length %d < expected min %d", len(tc.desc), tc.minByteLength)
			}
			runes := []rune(tc.desc)
			runeCount := len(runes)
			if tc.expectTrunc && runeCount <= 80 {
				t.Fatalf("test setup error: expected truncation but rune count is %d <= 80", runeCount)
			}
			if !tc.expectTrunc && runeCount > 80 {
				t.Fatalf("test setup error: expected no truncation but rune count is %d > 80", runeCount)
			}

			defs := []ToolDef{
				{
					Name:        "SafeTool",
					Description: tc.desc,
					Parameters:  map[string]interface{}{"type": "object"},
				},
			}

			contract := buildContract(defs)

			// Invariant 1: Resulting contract must be 100% valid UTF-8
			if !utf8.ValidString(contract) {
				t.Fatalf("buildContract produced invalid UTF-8 string!")
			}

			// Invariant 2: No unicode replacement characters (\ufffd)
			if strings.Contains(contract, "\ufffd") {
				t.Fatalf("contract contains corrupted UTF-8 replacement character (\\ufffd)!")
			}

			// Invariant 3: Truncation behavior matches rune boundary
			if tc.expectTrunc {
				expectedPrefix := string(runes[:80]) + "..."
				if !strings.Contains(contract, expectedPrefix) {
					t.Errorf("contract missing expected rune-truncated prefix: %q", expectedPrefix)
				}
			} else {
				if strings.Contains(contract, tc.desc+"...") {
					t.Errorf("description with <= 80 runes was unexpectedly truncated!")
				}
				if !strings.Contains(contract, tc.desc) {
					t.Errorf("full description missing from contract: %q", tc.desc)
				}
			}
		})
	}
}
