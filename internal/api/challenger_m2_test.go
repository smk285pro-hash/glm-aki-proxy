package api

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"

	"glm-aki-proxy/internal/upstream"
	"glm-aki-proxy/internal/util"
)

// ══════════════════════════════════════════════════════════════════
// CHALLENGER M2: ADVERSARIAL STRESS TEST SUITE
// ══════════════════════════════════════════════════════════════════

// 1. Tier 1 Adversarial: Markdown Fences inside Code Snippet
// Ensures tool calls containing nested markdown ```json ... ``` blocks are not truncated or misparsed.
func TestChallengerM2_Tier1_Syntax_NestedMarkdownFences(t *testing.T) {
	defs := []ToolDef{
		{
			Name:        "WriteDoc",
			Description: "Write markdown documentation",
			Parameters: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"path":    map[string]interface{}{"type": "string"},
					"content": map[string]interface{}{"type": "string"},
				},
				"required": []interface{}{"path", "content"},
			},
		},
	}

	docContent := "# API Guide\n\nExample payload:\n```json\n{\n  \"model\": \"glm-5.3\",\n  \"stream\": true\n}\n```\n\nUse with caution."
	callJSON, _ := json.Marshal(map[string]interface{}{
		"name": "WriteDoc",
		"arguments": map[string]interface{}{
			"path":    "docs/guide.md",
			"content": docContent,
		},
	})

	rawOutput := fmt.Sprintf("```json\n%s\n```", string(callJSON))
	calls := parseToolCalls(rawOutput, defs)
	if len(calls) != 1 {
		t.Fatalf("expected 1 call parsed, got %d", len(calls))
	}

	var args map[string]interface{}
	if err := json.Unmarshal([]byte(calls[0].Arguments), &args); err != nil {
		t.Fatalf("unmarshal arguments failed: %v", err)
	}

	if args["content"] != docContent {
		t.Fatalf("content mismatch:\nwant:\n%s\ngot:\n%s", docContent, args["content"])
	}
}

// 2. Tier 1 Adversarial: Preservation of Zero Values and Empty Containers
// Ensure boolean false, integer 0, float 0.0, and empty array [] are not dropped.
func TestChallengerM2_Tier1_Syntax_ZeroValuesPreserved(t *testing.T) {
	defs := []ToolDef{
		{
			Name:        "SetOptions",
			Description: "Configure options",
			Parameters:  map[string]interface{}{"type": "object"},
		},
	}

	rawArgs := `{"enabled": false, "retries": 0, "score": 0.0, "tags": [], "extra": null}`
	rawOutput := fmt.Sprintf("<<<TOOL_CALL>>>\n{\"name\":\"SetOptions\",\"arguments\":%s}\n<<<END_TOOL_CALL>>>", rawArgs)

	calls := parseToolCalls(rawOutput, defs)
	if len(calls) != 1 {
		t.Fatalf("expected 1 call, got %d", len(calls))
	}

	var args map[string]interface{}
	if err := json.Unmarshal([]byte(calls[0].Arguments), &args); err != nil {
		t.Fatalf("unmarshal arguments failed: %v", err)
	}

	if args["enabled"] != false {
		t.Errorf("boolean false corrupted: %+v", args["enabled"])
	}
	if args["retries"] != float64(0) {
		t.Errorf("integer 0 corrupted: %+v", args["retries"])
	}
	tags, ok := args["tags"].([]interface{})
	if !ok || len(tags) != 0 {
		t.Errorf("empty array corrupted: %+v", args["tags"])
	}
}

// 3. Tier 1 Adversarial: Windows UNC Path Handling
func TestChallengerM2_Tier1_Syntax_WindowsUNCPath(t *testing.T) {
	uncPath := `\\nas-server\projects\glm-aki-proxy\config.json`
	rawJSON := fmt.Sprintf(`{"file_path": "%s", "read_only": true}`, uncPath)
	repaired := repairArgs(rawJSON)

	if !json.Valid([]byte(repaired)) {
		t.Fatalf("repaired JSON invalid: %s", repaired)
	}

	var m map[string]interface{}
	if err := json.Unmarshal([]byte(repaired), &m); err != nil {
		t.Fatalf("unmarshal failed: %v", err)
	}

	p, _ := m["file_path"].(string)
	if !strings.Contains(p, `nas-server`) {
		t.Fatalf("UNC path corrupted: %s", p)
	}
}

// 4. Tier 2 Adversarial: Genuine Reasoning Text Preceding Tool Call
// When model provides valid non-fluff analytical reasoning before <<<TOOL_CALL>>>,
// the reasoning should be emitted as text_delta while tool_use is emitted cleanly.
func TestChallengerM2_Tier2_ReasoningPrecedingToolCall(t *testing.T) {
	defs := []ToolDef{
		{
			Name:        "Read",
			Description: "Read file",
			Parameters:  map[string]interface{}{"type": "object"},
		},
	}

	analyticalReasoning := "Sau khi phân tích log, hàm parseToolCalls tại dòng 276 có vẻ trả về nil thay vì mảng calls."
	rawOutput := fmt.Sprintf("%s\n<<<TOOL_CALL>>>\n{\"name\":\"Read\",\"arguments\":{\"file_path\":\"tools.go\"}}\n<<<END_TOOL_CALL>>>", analyticalReasoning)

	w := httptest.NewRecorder()
	opts := upstream.ChatOpts{}

	writeAnthropicStream(w, "msg_reasoning_test", "claude-3-7-sonnet-20250219", nil, defs, false, opts,
		func(onText, onReason func(string)) (string, string, error) {
			onText(rawOutput)
			return rawOutput, "", nil
		})

	events := decodeAnthropicEvents(t, w.Body.String())

	var emittedText strings.Builder
	var emittedTools []string
	var stopReason string

	for _, e := range events {
		if e.Type == "content_block_delta" && e.Delta.Type == "text_delta" {
			emittedText.WriteString(e.Delta.Text)
		}
		if e.Type == "content_block_start" && e.ContentBlock.Type == "tool_use" {
			emittedTools = append(emittedTools, e.ContentBlock.Name)
		}
		if e.Type == "message_delta" && e.Delta.StopReason != "" {
			stopReason = e.Delta.StopReason
		}
	}

	// 1. Genuine analytical reasoning must NOT be dropped as fluff
	if !strings.Contains(emittedText.String(), "Sau khi phân tích log") {
		t.Fatalf("analytical reasoning was incorrectly dropped as fluff: %q", emittedText.String())
	}

	// 2. Tool call must still be detected and emitted
	if len(emittedTools) != 1 || emittedTools[0] != "Read" {
		t.Fatalf("expected 1 Read tool call, got: %v", emittedTools)
	}

	// 3. Stop reason must be tool_use
	if stopReason != "tool_use" {
		t.Fatalf("expected stop_reason 'tool_use', got %q", stopReason)
	}
}

// 5. Tier 2 Adversarial: Parallel Tool Calls in a Single Turn
// Assistant emits 2 tool calls in parallel (e.g. Read main.go AND Read config.go).
func TestChallengerM2_Tier2_ParallelToolCallsInOneTurn(t *testing.T) {
	defs := []ToolDef{
		{Name: "Read", Description: "Read file", Parameters: map[string]interface{}{"type": "object"}},
	}

	rawParallel := `<<<TOOL_CALL>>>
{"name":"Read","arguments":{"file_path":"main.go"}}
<<<END_TOOL_CALL>>>
<<<TOOL_CALL>>>
{"name":"Read","arguments":{"file_path":"config.go"}}
<<<END_TOOL_CALL>>>`

	w := httptest.NewRecorder()
	opts := upstream.ChatOpts{}

	writeAnthropicStream(w, "msg_parallel_test", "claude-3-7-sonnet-20250219", nil, defs, false, opts,
		func(onText, onReason func(string)) (string, string, error) {
			onText(rawParallel)
			return rawParallel, "", nil
		})

	events := decodeAnthropicEvents(t, w.Body.String())

	var toolCalls []string
	var stopReason string

	for _, e := range events {
		if e.Type == "content_block_start" && e.ContentBlock.Type == "tool_use" {
			toolCalls = append(toolCalls, e.ContentBlock.Name)
		}
		if e.Type == "message_delta" && e.Delta.StopReason != "" {
			stopReason = e.Delta.StopReason
		}
	}

	if len(toolCalls) != 2 {
		t.Fatalf("expected 2 parallel tool calls, got %d (%v)", len(toolCalls), toolCalls)
	}
	if stopReason != "tool_use" {
		t.Fatalf("expected stop_reason 'tool_use', got %q", stopReason)
	}
}

// 6. Tier 2 Adversarial: Deep 10-Turn Loop Token Growth Invariance
// Simulates an extended 10-turn conversation and verifies strict O(N) linear token scaling.
func TestChallengerM2_Tier2_Deep10TurnsTokenScaling(t *testing.T) {
	defs := []ToolDef{
		{Name: "Bash", Description: "Run command", Parameters: map[string]interface{}{"type": "object"}},
	}

	var accumulated []json.RawMessage
	accumulated = append(accumulated, makeRawJSON(map[string]interface{}{
		"role":    "user",
		"content": "Perform deep diagnostic loop across 10 steps.",
	}))

	var tokenCounts []int

	for step := 1; step <= 10; step++ {
		toolID := fmt.Sprintf("step_call_%d", step)
		// Assistant turn
		accumulated = append(accumulated, makeRawJSON(map[string]interface{}{
			"role": "assistant",
			"content": []map[string]interface{}{
				{"type": "tool_use", "id": toolID, "name": "Bash", "input": map[string]interface{}{"command": "uptime"}},
			},
		}))

		// User tool_result turn
		accumulated = append(accumulated, makeRawJSON(map[string]interface{}{
			"role": "user",
			"content": []map[string]interface{}{
				{"type": "tool_result", "tool_use_id": toolID, "content": fmt.Sprintf("load average: 0.%02d", step)},
			},
		}))

		req := anthropicRequest{
			Model:    "claude-3-7-sonnet-20250219",
			Messages: accumulated,
		}
		openAIMsgs, err := toOpenAI(req)
		if err != nil {
			t.Fatalf("step %d toOpenAI failed: %v", step, err)
		}

		converted := convertForTools(openAIMsgs, buildContract(defs))
		prompt := upstream.FlattenPrompt(converted)
		tokens := util.EstimateTokens(prompt)
		tokenCounts = append(tokenCounts, tokens)

		if step > 1 {
			growth := tokens - tokenCounts[step-2]
			if growth > 500 {
				t.Fatalf("step %d abnormal quadratic token spike: +%d tokens (current: %d)", step, growth, tokens)
			}
		}
	}

	t.Logf("10-turn token scaling trajectory: %v", tokenCounts)
	totalGrowth := tokenCounts[9] - tokenCounts[0]
	avgPerTurn := totalGrowth / 9
	t.Logf("Average growth per turn: %d tokens (linear baseline)", avgPerTurn)
}

// 7. Tier 3 Adversarial: MCP Tool with Hyphenated Server and Complex Action
func TestChallengerM2_Tier3_MCP_HyphenatedNames(t *testing.T) {
	defs := []ToolDef{
		{
			Name:        "mcp__github-enterprise__create_pull_request",
			Description: "Create PR on Github Enterprise",
			Parameters: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"title": map[string]interface{}{"type": "string"},
					"head":  map[string]interface{}{"type": "string"},
					"base":  map[string]interface{}{"type": "string"},
				},
				"required": []interface{}{"title", "head", "base"},
			},
		},
	}

	contract := buildContract(defs)
	if !strings.Contains(contract, "mcp__github-enterprise__create_pull_request") {
		t.Fatalf("contract did not preserve hyphenated MCP tool name: %s", contract)
	}

	rawCall := `<<<TOOL_CALL>>>
{"name":"mcp__github-enterprise__create_pull_request","arguments":{"title":"Release v2.0","head":"feature/v2","base":"main"}}
<<<END_TOOL_CALL>>>`

	calls := parseToolCalls(rawCall, defs)
	if len(calls) != 1 || calls[0].Name != "mcp__github-enterprise__create_pull_request" {
		t.Fatalf("parseToolCalls failed for hyphenated MCP name: %+v", calls)
	}
}

// 8. Tier 3 Adversarial: MCP Tool Result with JSON Structured Error
func TestChallengerM2_Tier3_MCP_JSONStructuredError(t *testing.T) {
	toolID := "mcp_err_call_777"
	jsonErrorPayload := `{"error_code": 429, "message": "rate limit exceeded", "retry_after_ms": 3000}`

	anthroReq := anthropicRequest{
		Model: "claude-3-7-sonnet-20250219",
		Messages: []json.RawMessage{
			makeRawJSON(map[string]interface{}{"role": "user", "content": "Execute API call"}),
			makeRawJSON(map[string]interface{}{
				"role": "assistant",
				"content": []map[string]interface{}{
					{"type": "tool_use", "id": toolID, "name": "mcp__api__fetch", "input": map[string]interface{}{}},
				},
			}),
			makeRawJSON(map[string]interface{}{
				"role": "user",
				"content": []map[string]interface{}{
					{"type": "tool_result", "tool_use_id": toolID, "content": jsonErrorPayload, "is_error": true},
				},
			}),
		},
	}

	openAIMsgs, err := toOpenAI(anthroReq)
	if err != nil {
		t.Fatalf("toOpenAI failed: %v", err)
	}

	converted := convertForTools(openAIMsgs, "CONTRACT")
	lastMsg := converted[len(converted)-1]

	var m map[string]interface{}
	if err := json.Unmarshal(lastMsg, &m); err != nil {
		t.Fatalf("unmarshal failed: %v", err)
	}

	content, _ := m["content"].(string)

	expectedHeader := fmt.Sprintf("[Tool ERROR for %s]:", toolID)
	if !strings.Contains(content, expectedHeader) {
		t.Fatalf("missing [Tool ERROR for ...] header in: %s", content)
	}
	if !strings.Contains(content, jsonErrorPayload) {
		t.Fatalf("JSON error payload corrupted: %s", content)
	}
}
