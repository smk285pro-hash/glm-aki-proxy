// Package api contains the 3-Tier Benchmark Matrix test suite for Milestone 2.
// Covers Tier 1 (Syntax & Complex Args), Tier 2 (Multi-Step Agentic Loop >= 5 steps),
// Tier 3 (MCP Tool Integration), and Live Server verification.
package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"glm-aki-proxy/internal/upstream"
	"glm-aki-proxy/internal/util"
)

// ════════════════════════════════════════════════════════════════════════════════
// TIER 1: SYNTAX & COMPLEX ARGUMENTS BENCHMARK (T1.1 - T1.5)
// ════════════════════════════════════════════════════════════════════════════════

// T1.1 Escaped Characters & Newlines:
// Tool calls containing code with \n, \t, \", backticks, raw multiline code snippets.
// Verify argument unmarshaling and code preservation without corruption.
func TestBenchmark_Tier1_Syntax_T1_1_EscapedCharactersAndNewlines(t *testing.T) {
	defs := []ToolDef{
		{
			Name:        "Write",
			Description: "Write content to a file",
			Parameters: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"file_path": map[string]interface{}{"type": "string"},
					"content":   map[string]interface{}{"type": "string"},
				},
				"required": []interface{}{"file_path", "content"},
			},
		},
		{
			Name:        "Edit",
			Description: "Edit file content",
			Parameters: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"file_path":  map[string]interface{}{"type": "string"},
					"old_string": map[string]interface{}{"type": "string"},
					"new_string": map[string]interface{}{"type": "string"},
				},
				"required": []interface{}{"file_path", "old_string", "new_string"},
			},
		},
	}

	rawCodeSnippet := "package main\n\nimport (\n\t\"fmt\"\n\t\"os\"\n)\n\n// \"Hello World\" with backticks `test`\nfunc main() {\n\tmsg := \"Tab\tQuote\\\"Done\\\"\"\n\tfmt.Println(msg)\n}\n"

	argsObj := map[string]interface{}{
		"file_path": "main.go",
		"content":   rawCodeSnippet,
	}
	callObj := map[string]interface{}{
		"name":      "Write",
		"arguments": argsObj,
	}
	validCallBytes, err := json.Marshal(callObj)
	if err != nil {
		t.Fatalf("failed to marshal valid call JSON: %v", err)
	}

	// Also test raw unescaped multiline code snippet that requires repairArgs
	rawMultilineUnescaped := "{\n\"name\": \"Write\",\n\"arguments\": {\n\"file_path\": \"main.go\",\n\"content\": \"line 1\nline 2 with\ttab\nline 3\"\n}\n}"

	testDialects := []struct {
		name        string
		rawText     string
		wantSnippet string
	}{
		{
			name:        "MarkerBlock_WithNewlinesAndQuotes",
			rawText:     fmt.Sprintf("<<<TOOL_CALL>>>\n%s\n<<<END_TOOL_CALL>>>", string(validCallBytes)),
			wantSnippet: rawCodeSnippet,
		},
		{
			name:        "BareJSON_WithEscapedControlChars",
			rawText:     string(validCallBytes),
			wantSnippet: rawCodeSnippet,
		},
		{
			name:        "FencedJSON_WithMultilineCode",
			rawText:     fmt.Sprintf("```json\n%s\n```", string(validCallBytes)),
			wantSnippet: rawCodeSnippet,
		},
		{
			name:        "SingleTag_WithEscapedQuotes",
			rawText:     fmt.Sprintf("<tool_call>\n%s\n</tool_call>", string(validCallBytes)),
			wantSnippet: rawCodeSnippet,
		},
		{
			name:        "RawMultilineUnescapedCodeSnippet",
			rawText:     fmt.Sprintf("<<<TOOL_CALL>>>\n%s\n<<<END_TOOL_CALL>>>", rawMultilineUnescaped),
			wantSnippet: "line 1\nline 2 with\ttab\nline 3",
		},
	}

	for _, tc := range testDialects {
		t.Run(tc.name, func(t *testing.T) {
			calls := parseToolCalls(tc.rawText, defs)
			if len(calls) != 1 {
				t.Fatalf("expected 1 call parsed, got %d", len(calls))
			}
			if calls[0].Name != "Write" {
				t.Fatalf("expected tool name 'Write', got %q", calls[0].Name)
			}

			var args map[string]interface{}
			if err := json.Unmarshal([]byte(calls[0].Arguments), &args); err != nil {
				t.Fatalf("failed to unmarshal parsed arguments JSON: %v (raw: %s)", err, calls[0].Arguments)
			}

			content, ok := args["content"].(string)
			if !ok {
				t.Fatalf("arguments['content'] is not a string: %+v", args)
			}

			if content != tc.wantSnippet {
				t.Fatalf("code snippet corrupted!\nwant:\n%s\ngot:\n%s", tc.wantSnippet, content)
			}

			// Verify presence of exact escape runes
			if strings.Contains(tc.wantSnippet, "\t") && !strings.Contains(content, "\t") {
				t.Errorf("content missing literal tab character")
			}
			if strings.Contains(tc.wantSnippet, "\n") && !strings.Contains(content, "\n") {
				t.Errorf("content missing literal newline character")
			}
		})
	}
}

// T1.2 Shell Commands with Pipes & Regex:
// Bash tool calls with complex commands (e.g. rg, pipes, grep -v, regex with parentheses).
// Verify quote escaping and pipe stability.
func TestBenchmark_Tier1_Syntax_T1_2_ShellCommandsWithPipesAndRegex(t *testing.T) {
	defs := []ToolDef{
		{
			Name:        "Bash",
			Description: "Execute shell command",
			Parameters: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"command": map[string]interface{}{"type": "string"},
					"timeout": map[string]interface{}{"type": "integer"},
				},
				"required": []interface{}{"command"},
			},
		},
	}

	complexCommands := []struct {
		name string
		cmd  string
	}{
		{
			name: "RipgrepRegexWithPipesAndInversion",
			cmd:  `rg -in "func.*\(.*string\)" ./internal | grep -v "_test.go" | awk '{print $1}'`,
		},
		{
			name: "PipesWithStderrRedirectionAndTee",
			cmd:  `find . -name "*.go" -exec grep -Hn "TODO" {} + 2>&1 | tee /tmp/todo.log`,
		},
		{
			name: "InlinePythonWithDoubleAndSingleQuotes",
			cmd:  `python -c "import sys, json; print(json.dumps({'status': 'ok', 'count': 42}))"`,
		},
		{
			name: "SubshellWithBackticksAndVariables",
			cmd:  `echo "BUILD_TIME=$(date -u +'%Y-%m-%dT%H:%M:%SZ')" >> build.env`,
		},
		{
			name: "CurlWithComplexJSONHeaderAndData",
			cmd:  `curl -s -X POST http://127.0.0.1:5084/v1/messages -H "Content-Type: application/json" -d "{\"model\": \"glm-5.3\"}"`,
		},
	}

	for _, tc := range complexCommands {
		t.Run(tc.name, func(t *testing.T) {
			rawOutput := fmt.Sprintf("<<<TOOL_CALL>>>\n{\"name\":\"Bash\",\"arguments\":{\"command\":%q,\"timeout\":15000}}\n<<<END_TOOL_CALL>>>", tc.cmd)
			calls := parseToolCalls(rawOutput, defs)
			if len(calls) != 1 {
				t.Fatalf("expected 1 call, got %d", len(calls))
			}
			if calls[0].Name != "Bash" {
				t.Fatalf("expected tool 'Bash', got %q", calls[0].Name)
			}

			var args map[string]interface{}
			if err := json.Unmarshal([]byte(calls[0].Arguments), &args); err != nil {
				t.Fatalf("unmarshal arguments failed: %v", err)
			}

			cmdGot, _ := args["command"].(string)
			if cmdGot != tc.cmd {
				t.Fatalf("command mismatch:\nwant: %s\ngot:  %s", tc.cmd, cmdGot)
			}

			// Verify timeout preserved
			timeoutGot, _ := args["timeout"].(float64)
			if timeoutGot != 15000 {
				t.Fatalf("timeout mismatch: want 15000, got %v", timeoutGot)
			}
		})
	}
}

// T1.3 Deeply Nested JSON Objects:
// Tool calls with 3+ levels of nesting (e.g. MCP entity schema with metadata properties, tags array, nested config).
// Verify complete map hierarchy preservation.
func TestBenchmark_Tier1_Syntax_T1_3_DeeplyNestedJSONObjects(t *testing.T) {
	defs := []ToolDef{
		{
			Name:        "ConfigureService",
			Description: "Update nested service configurations",
			Parameters: map[string]interface{}{
				"type": "object",
			},
		},
	}

	nestedPayload := map[string]interface{}{
		"server": "gitnexus",
		"version": float64(2),
		"config": map[string]interface{}{
			"indexing": map[string]interface{}{
				"depth": float64(5),
				"filters": map[string]interface{}{
					"include": []interface{}{"*.go", "*.ts"},
					"exclude": []interface{}{"*_test.go", "vendor/*"},
				},
			},
			"metadata": map[string]interface{}{
				"tags": []interface{}{"repo", "symbols", "mcp"},
				"attributes": map[string]interface{}{
					"enabled": true,
					"limits": map[string]interface{}{
						"max_files":   float64(50000),
						"buffer_size": float64(1048576),
					},
				},
			},
		},
	}

	rawArgsBytes, err := json.Marshal(nestedPayload)
	if err != nil {
		t.Fatalf("failed to marshal nested payload: %v", err)
	}

	rawText := fmt.Sprintf("<<<TOOL_CALL>>>\n{\"name\":\"ConfigureService\",\"arguments\":%s}\n<<<END_TOOL_CALL>>>", string(rawArgsBytes))
	calls := parseToolCalls(rawText, defs)
	if len(calls) != 1 {
		t.Fatalf("expected 1 call, got %d", len(calls))
	}

	var parsedArgs map[string]interface{}
	if err := json.Unmarshal([]byte(calls[0].Arguments), &parsedArgs); err != nil {
		t.Fatalf("failed to unmarshal parsed nested arguments: %v", err)
	}

	// Verify Level 1
	if parsedArgs["server"] != "gitnexus" || parsedArgs["version"] != float64(2) {
		t.Fatalf("level 1 properties mismatch: %+v", parsedArgs)
	}

	// Verify Level 2: config
	configMap, ok := parsedArgs["config"].(map[string]interface{})
	if !ok {
		t.Fatalf("level 2 config is not map: %+v", parsedArgs["config"])
	}

	// Verify Level 3: config.indexing.filters
	indexingMap, ok := configMap["indexing"].(map[string]interface{})
	if !ok {
		t.Fatalf("level 3 indexing is not map: %+v", configMap["indexing"])
	}
	filtersMap, ok := indexingMap["filters"].(map[string]interface{})
	if !ok {
		t.Fatalf("level 4 filters is not map: %+v", indexingMap["filters"])
	}
	incList, ok := filtersMap["include"].([]interface{})
	if !ok || len(incList) != 2 || incList[0] != "*.go" || incList[1] != "*.ts" {
		t.Fatalf("include filters mismatch: %+v", filtersMap["include"])
	}

	// Verify Level 4: config.metadata.attributes.limits
	metaMap, ok := configMap["metadata"].(map[string]interface{})
	if !ok {
		t.Fatalf("level 3 metadata is not map: %+v", configMap["metadata"])
	}
	attrMap, ok := metaMap["attributes"].(map[string]interface{})
	if !ok {
		t.Fatalf("level 4 attributes is not map: %+v", metaMap["attributes"])
	}
	limitsMap, ok := attrMap["limits"].(map[string]interface{})
	if !ok {
		t.Fatalf("level 5 limits is not map: %+v", attrMap["limits"])
	}
	if limitsMap["max_files"] != float64(50000) || limitsMap["buffer_size"] != float64(1048576) {
		t.Fatalf("level 5 limits values mismatch: %+v", limitsMap)
	}
}

// T1.4 Unicode, Diacritics & Windows Paths:
// Tool calls with Vietnamese text ("Kiểm tra lỗi bộ đệm và đường dẫn") and Windows paths
// (C:\Users\smk28\Desktop\glm-aki-proxy\main.go, C:\tools\test.bat).
// Verify rune preservation and no control char corruption.
func TestBenchmark_Tier1_Syntax_T1_4_UnicodeDiacriticsAndWindowsPaths(t *testing.T) {
	defs := []ToolDef{
		{
			Name:        "AnalyzeFile",
			Description: "Analyze file with path and notes",
			Parameters: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"path":  map[string]interface{}{"type": "string"},
					"notes": map[string]interface{}{"type": "string"},
				},
				"required": []interface{}{"path", "notes"},
			},
		},
	}

	vietnameseText := "Kiểm tra lỗi bộ đệm và đường dẫn tệp tin hệ thống tiếng Việt có dấu: Ắ, Ằ, Ẳ, Ẵ, Ặ, Ế, Ề, Ể, Ễ, Ệ, Ố, Ồ, Ổ, Ỗ, Ộ."
	windowsPaths := []string{
		`C:\Users\smk28\Desktop\glm-aki-proxy\main.go`,
		`C:\tools\test_runner\run.bat`,
		`C:\new_folder\notes.txt`,
		`C:\release\v1.0\app.exe`,
		`C:\bin\build.exe`,
		`C:\files\data.csv`,
	}

	for _, wPath := range windowsPaths {
		t.Run("Path_"+wPath[:min(len(wPath), 25)], func(t *testing.T) {
			// Simulate raw model emitting unescaped single backslashes in Windows path
			rawJSON := fmt.Sprintf(`{"path": "%s", "notes": "%s"}`, wPath, vietnameseText)
			repaired := repairArgs(rawJSON)

			if !json.Valid([]byte(repaired)) {
				t.Fatalf("repaired JSON is invalid: %s (raw was: %s)", repaired, rawJSON)
			}

			var m map[string]string
			if err := json.Unmarshal([]byte(repaired), &m); err != nil {
				t.Fatalf("unmarshal repaired JSON failed: %v", err)
			}

			// 1. Verify Unicode & Diacritics
			if m["notes"] != vietnameseText {
				t.Fatalf("Vietnamese text corrupted!\nwant: %s\ngot:  %s", vietnameseText, m["notes"])
			}
			if !utf8.ValidString(m["notes"]) {
				t.Fatalf("notes string is not valid UTF-8: %q", m["notes"])
			}

			// 2. Verify Windows path integrity: must NOT convert folder names to control characters (\t, \n, \r, \b, \f)
			p := m["path"]
			if strings.ContainsRune(p, '\t') {
				t.Errorf("path contains literal TAB byte: %q", p)
			}
			if strings.ContainsRune(p, '\n') {
				t.Errorf("path contains literal NEWLINE byte: %q", p)
			}
			if strings.ContainsRune(p, '\r') {
				t.Errorf("path contains literal CARRIAGE RETURN byte: %q", p)
			}
			if strings.ContainsRune(p, '\b') {
				t.Errorf("path contains literal BACKSPACE byte: %q", p)
			}
			if strings.ContainsRune(p, '\f') {
				t.Errorf("path contains literal FORM FEED byte: %q", p)
			}

			// 3. Verify parseToolCalls end-to-end
			callText := fmt.Sprintf("<<<TOOL_CALL>>>\n{\"name\":\"AnalyzeFile\",\"arguments\":%s}\n<<<END_TOOL_CALL>>>", rawJSON)
			calls := parseToolCalls(callText, defs)
			if len(calls) != 1 {
				t.Fatalf("parseToolCalls failed for Windows path call: %q", callText)
			}
		})
	}
}

// T1.5 Malformed JSON Auto-Repair:
// Test truncation after colon ({"key":), missing trailing braces, double trailing commas,
// single-backslash Windows paths, and triple-stringified JSON.
func TestBenchmark_Tier1_Syntax_T1_5_MalformedJSONAutoRepair(t *testing.T) {
	testCases := []struct {
		name      string
		input     string
		checkKey  string
		wantExist bool
	}{
		{
			name:      "TruncationAfterColon",
			input:     `{"name":"Read","arguments":{"file_path":`,
			checkKey:  "file_path",
			wantExist: true,
		},
		{
			name:      "MissingTrailingBraces_TwoLevels",
			input:     `{"file_path":"a.txt","options":{"encoding":"utf8"`,
			checkKey:  "file_path",
			wantExist: true,
		},
		{
			name:      "DoubleTrailingCommas",
			input:     `{"path":"config.go",, "flags":["-v",,],}`,
			checkKey:  "path",
			wantExist: true,
		},
		{
			name:      "SingleBackslashWindowsPath_LowercaseUsers",
			input:     `{"file": "C:\users\developer\project\main.go"}`,
			checkKey:  "file",
			wantExist: true,
		},
		{
			name:      "TripleStringifiedJSON",
			input:     `"\"{\\\"name\\\": \\\"Read\\\", \\\"arguments\\\": {\\\"file\\\": \\\"test.go\\\"}}\""`,
			checkKey:  "file",
			wantExist: true,
		},
		{
			name:      "TruncationInsideArrayAfterColon",
			input:     `{"items": [{"id": 1, "data":`,
			checkKey:  "items",
			wantExist: true,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			repaired := repairArgs(tc.input)
			if !json.Valid([]byte(repaired)) {
				t.Fatalf("repairArgs output is invalid JSON:\ninput:    %s\nrepaired: %s", tc.input, repaired)
			}

			var val interface{}
			if err := json.Unmarshal([]byte(repaired), &val); err != nil {
				t.Fatalf("unmarshal failed on repaired JSON %q: %v", repaired, err)
			}

			m, ok := val.(map[string]interface{})
			if !ok {
				t.Fatalf("expected top-level JSON object, got %T: %+v", val, val)
			}

			if tc.wantExist {
				found := false
				if _, exists := m[tc.checkKey]; exists {
					found = true
				} else if argsMap, ok := m["arguments"].(map[string]interface{}); ok {
					if _, exists := argsMap[tc.checkKey]; exists {
						found = true
					}
				}
				if !found {
					t.Errorf("expected key %q in repaired map: %+v", tc.checkKey, m)
				}
			}
		})
	}
}

// ════════════════════════════════════════════════════════════════════════════════
// TIER 2: MULTI-STEP AGENTIC LOOP BENCHMARK (>= 5 STEPS / 7 TURNS)
// ════════════════════════════════════════════════════════════════════════════════

// TestBenchmark_Tier2_MultiStepAgenticLoop executes an automated 7-turn sequential workflow:
// Turn 1: Inspect Directory -> Glob (args: pattern: "*.go")
// Turn 2: Locate Code       -> Grep (args: pattern: "parseToolCalls", path: "internal/api")
// Turn 3: Read Source       -> Read (args: file_path: "internal/api/tools.go", offset: 1, limit: 50)
// Turn 4: Execute Test      -> Bash (args: command: "go test -v ./internal/api -run TestParse") [fails]
// Turn 5: Edit Code         -> Edit (args: file_path: "internal/api/tools.go", old: "foo", new: "bar")
// Turn 6: Re-test           -> Bash (args: command: "go test -v ./internal/api -run TestParse") [passes]
// Turn 7: Final Conclusion  -> Plain Vietnamese explanation (no tools)
//
// Assertions:
// 1. Turns 1–6 MUST emit stop_reason: "tool_use"; Turn 7 MUST emit stop_reason: "end_turn".
// 2. Protocol Hygiene: Exactly 0 occurrences of marker leaks across all turns.
// 3. Zero Self-Narration: Zero occurrences of model meta-commentary ("Theo quy tắc tool...", etc.).
// 4. Context Continuity: Linear token scaling O(N), no quadratic token duplication, valid tool IDs.
// 5. Vietnamese Language Anchoring: Turn 7 response in Vietnamese with zero Chinese drift.
func TestBenchmark_Tier2_MultiStepAgenticLoop(t *testing.T) {
	defs := []ToolDef{
		{Name: "Glob", Description: "Find files by pattern", Parameters: map[string]interface{}{"type": "object", "properties": map[string]interface{}{"pattern": map[string]interface{}{"type": "string"}}}},
		{Name: "Grep", Description: "Search regex in directory", Parameters: map[string]interface{}{"type": "object", "properties": map[string]interface{}{"pattern": map[string]interface{}{"type": "string"}, "path": map[string]interface{}{"type": "string"}}}},
		{Name: "Read", Description: "Read lines of a file", Parameters: map[string]interface{}{"type": "object", "properties": map[string]interface{}{"file_path": map[string]interface{}{"type": "string"}, "offset": map[string]interface{}{"type": "integer"}, "limit": map[string]interface{}{"type": "integer"}}}},
		{Name: "Bash", Description: "Run shell command", Parameters: map[string]interface{}{"type": "object", "properties": map[string]interface{}{"command": map[string]interface{}{"type": "string"}}}},
		{Name: "Edit", Description: "Replace exact string in file", Parameters: map[string]interface{}{"type": "object", "properties": map[string]interface{}{"file_path": map[string]interface{}{"type": "string"}, "old_string": map[string]interface{}{"type": "string"}, "new_string": map[string]interface{}{"type": "string"}}}},
	}

	type turnSpec struct {
		turnNum        int
		description    string
		simulatedRaw   string // Upstream model response
		expectedTool   string // Expected tool name, or "" for turn 7
		expectedStop   string // "tool_use" or "end_turn"
		toolResultBody string // CLI tool result returned in next turn
		toolIsError    bool
	}

	workflow := []turnSpec{
		{
			turnNum:        1,
			description:    "Inspect Directory -> Glob",
			simulatedRaw:   "Tôi sẽ kiểm tra cấu trúc thư mục.\n<<<TOOL_CALL>>>\n{\"name\":\"Glob\",\"arguments\":{\"pattern\":\"*.go\"}}\n<<<END_TOOL_CALL>>>",
			expectedTool:   "Glob",
			expectedStop:   "tool_use",
			toolResultBody: `["main.go", "tools.go", "anthropic.go", "api.go"]`,
		},
		{
			turnNum:        2,
			description:    "Locate Code -> Grep",
			simulatedRaw:   "<<<TOOL_CALL>>>\n{\"name\":\"Grep\",\"arguments\":{\"pattern\":\"parseToolCalls\",\"path\":\"internal/api\"}}\n<<<END_TOOL_CALL>>>",
			expectedTool:   "Grep",
			expectedStop:   "tool_use",
			toolResultBody: `internal/api/tools.go:276:func parseToolCalls(text string, defs []ToolDef) []toolCall`,
		},
		{
			turnNum:        3,
			description:    "Read Source -> Read",
			simulatedRaw:   "<<<TOOL_CALL>>>\n{\"name\":\"Read\",\"arguments\":{\"file_path\":\"internal/api/tools.go\",\"offset\":1,\"limit\":50}}\n<<<END_TOOL_CALL>>>",
			expectedTool:   "Read",
			expectedStop:   "tool_use",
			toolResultBody: `package api\n\nimport "strings"\n\nfunc parseToolCalls...`,
		},
		{
			turnNum:        4,
			description:    "Execute Test -> Bash (returns failure)",
			simulatedRaw:   "<<<TOOL_CALL>>>\n{\"name\":\"Bash\",\"arguments\":{\"command\":\"go test -v ./internal/api -run TestParse\"}}\n<<<END_TOOL_CALL>>>",
			expectedTool:   "Bash",
			expectedStop:   "tool_use",
			toolResultBody: `FAIL: TestParse (0.05s)\n    tools_test.go:45: expected 1 call, got 0\nFAIL`,
			toolIsError:    true,
		},
		{
			turnNum:        5,
			description:    "Edit Code -> Edit (fixes bug)",
			simulatedRaw:   "<<<TOOL_CALL>>>\n{\"name\":\"Edit\",\"arguments\":{\"file_path\":\"internal/api/tools.go\",\"old_string\":\"return nil\",\"new_string\":\"return calls\"}}\n<<<END_TOOL_CALL>>>",
			expectedTool:   "Edit",
			expectedStop:   "tool_use",
			toolResultBody: `Successfully replaced 1 occurrence of "return nil" with "return calls" in internal/api/tools.go`,
		},
		{
			turnNum:        6,
			description:    "Re-test -> Bash (returns pass)",
			simulatedRaw:   "<<<TOOL_CALL>>>\n{\"name\":\"Bash\",\"arguments\":{\"command\":\"go test -v ./internal/api -run TestParse\"}}\n<<<END_TOOL_CALL>>>",
			expectedTool:   "Bash",
			expectedStop:   "tool_use",
			toolResultBody: `=== RUN   TestParse\n--- PASS: TestParse (0.08s)\nPASS\nok  glm-aki-proxy/internal/api 0.12s`,
		},
		{
			turnNum:        7,
			description:    "Final Conclusion -> Plain Vietnamese explanation (no tools)",
			simulatedRaw:   "Tôi đã điều tra nguyên nhân gây lỗi trong hàm parseToolCalls và sửa giá trị trả về từ nil thành calls. Sau khi sửa, bài kiểm thử TestParse đã chạy lại và vượt qua 100% thành công.",
			expectedTool:   "",
			expectedStop:   "end_turn",
			toolResultBody: "",
		},
	}

	forbiddenMarkers := []string{
		"<<<TOOL_CALL>>>",
		"<<<END_TOOL_CALL>>>",
		"<tool_call>",
		"</tool_call>",
		"TOOL_CALL回放",
		"回放结束",
		"继续分析",
	}

	forbiddenNarrationPrefixes := []string{
		"Theo quy tắc tool",
		"Tôi sẽ dùng tool",
		"Tôi sẽ sử dụng công cụ",
		"Theo protocol",
		"I will use tool",
		"Calling tool now",
	}

	var accumulatedMessages []json.RawMessage
	// Initial user prompt
	initialUserMsg, _ := json.Marshal(map[string]interface{}{
		"role":    "user",
		"content": "Điều tra và sửa lỗi TestParse bị fail trong package internal/api bằng tiếng Việt.",
	})
	accumulatedMessages = append(accumulatedMessages, initialUserMsg)

	var pastPromptTokens []int

	for _, step := range workflow {
		t.Run(fmt.Sprintf("Turn%d_%s", step.turnNum, step.expectedTool), func(t *testing.T) {
			// Step 1: Simulate SSE Streaming execution through writeAnthropicStream
			w := httptest.NewRecorder()
			rawOut := step.simulatedRaw
			opts := upstream.ChatOpts{}

			writeAnthropicStream(w, fmt.Sprintf("msg_turn_%d", step.turnNum), "claude-3-7-sonnet-20250219",
				accumulatedMessages, defs, false, opts,
				func(onText, onReason func(string)) (string, string, error) {
					onText(rawOut)
					return rawOut, "", nil
				})

			body := w.Body.String()
			events := decodeAnthropicEvents(t, body)

			// Assertion 1: Stop Reason Fidelity
			var foundStopReason string
			var emittedTools []string
			var emittedText strings.Builder

			for _, e := range events {
				if e.Type == "content_block_start" && e.ContentBlock.Type == "tool_use" {
					emittedTools = append(emittedTools, e.ContentBlock.Name)
				}
				if e.Type == "content_block_delta" && e.Delta.Type == "text_delta" {
					emittedText.WriteString(e.Delta.Text)
				}
				if e.Type == "message_delta" && e.Delta.StopReason != "" {
					foundStopReason = e.Delta.StopReason
				}
			}

			if foundStopReason != step.expectedStop {
				t.Fatalf("turn %d: stop_reason mismatch: want %q, got %q (events: %+v)", step.turnNum, step.expectedStop, foundStopReason, events)
			}

			if step.expectedTool != "" {
				if len(emittedTools) != 1 || emittedTools[0] != step.expectedTool {
					t.Fatalf("turn %d: expected tool %q, got %v", step.turnNum, step.expectedTool, emittedTools)
				}
			} else {
				if len(emittedTools) != 0 {
					t.Fatalf("turn %d: expected no tool calls, got %v", step.turnNum, emittedTools)
				}
			}

			// Assertion 2: Protocol Hygiene across all emitted text chunks
			fullEmittedText := emittedText.String()
			for _, marker := range forbiddenMarkers {
				if strings.Contains(fullEmittedText, marker) {
					t.Fatalf("turn %d: protocol marker leak detected in emitted text: %q (text: %q)", step.turnNum, marker, fullEmittedText)
				}
			}

			// Assertion 3: Zero Self-Narration
			for _, narr := range forbiddenNarrationPrefixes {
				if strings.Contains(fullEmittedText, narr) {
					t.Fatalf("turn %d: model self-narration leak detected in emitted text: %q (text: %q)", step.turnNum, narr, fullEmittedText)
				}
			}

			// Assertion 5: Vietnamese Language Anchoring in Turn 7
			if step.turnNum == 7 {
				if !strings.Contains(fullEmittedText, "Tôi đã") || !strings.Contains(fullEmittedText, "lỗi") || !strings.Contains(fullEmittedText, "thành công") {
					t.Fatalf("turn 7: final conclusion missing expected Vietnamese phrasing: %q", fullEmittedText)
				}
				// Verify zero Chinese characters in Turn 7
				for _, r := range fullEmittedText {
					if r >= 0x4e00 && r <= 0x9fff {
						t.Fatalf("turn 7: Chinese character leak detected in final answer: %c", r)
					}
				}
			}

			// Build history for the next turn
			if step.expectedTool != "" {
				toolCallID := fmt.Sprintf("call_step_%d", step.turnNum)
				// Record Assistant turn
				asstMsg, _ := json.Marshal(map[string]interface{}{
					"role": "assistant",
					"content": []map[string]interface{}{
						{
							"type":  "tool_use",
							"id":    toolCallID,
							"name":  step.expectedTool,
							"input": map[string]interface{}{},
						},
					},
				})
				accumulatedMessages = append(accumulatedMessages, asstMsg)

				// Record User tool_result turn
				resultBlock := map[string]interface{}{
					"type":         "tool_result",
					"tool_use_id":  toolCallID,
					"content":      step.toolResultBody,
				}
				if step.toolIsError {
					resultBlock["is_error"] = true
				}
				userResultMsg, _ := json.Marshal(map[string]interface{}{
					"role":    "user",
					"content": []map[string]interface{}{resultBlock},
				})
				accumulatedMessages = append(accumulatedMessages, userResultMsg)

				// Assertion 4: Context Continuity & Token Scaling
				reqObj := anthropicRequest{
					Model:    "claude-3-7-sonnet-20250219",
					Messages: accumulatedMessages,
					Tools:    nil,
				}
				openAIMsgs, err := toOpenAI(reqObj)
				if err != nil {
					t.Fatalf("toOpenAI conversion failed: %v", err)
				}

				contractFraming := buildContract(defs)
				converted := convertForTools(openAIMsgs, contractFraming)
				currentPrompt := upstream.FlattenPrompt(converted)
				currentTokens := util.EstimateTokens(currentPrompt)

				pastPromptTokens = append(pastPromptTokens, currentTokens)
				t.Logf("Turn %d token footprint: %d tokens (messages count: %d)", step.turnNum, currentTokens, len(converted))

				// Check that token growth is not quadratic
				if len(pastPromptTokens) >= 2 {
					delta := currentTokens - pastPromptTokens[len(pastPromptTokens)-2]
					if delta > 1500 { // Large jump indicates quadratic duplication
						t.Errorf("turn %d: abnormal token expansion detected (+%d tokens), potential history duplication", step.turnNum, delta)
					}
				}
			}
		})
	}
}

// ════════════════════════════════════════════════════════════════════════════════
// TIER 3: MCP TOOL INTEGRATION BENCHMARK (T3.1 - T3.4)
// ════════════════════════════════════════════════════════════════════════════════

// T3.1 Namespaced MCP Tool Calling:
// Tool name mcp__gitnexus__search_symbols with parameters query: string, limit: integer.
// Verify contract generation and call extraction without losing namespace.
func TestBenchmark_Tier3_MCP_T3_1_NamespacedMCPToolCalling(t *testing.T) {
	defs := []ToolDef{
		{
			Name:        "mcp__gitnexus__search_symbols",
			Description: "Search code symbols via GitNexus MCP server",
			Parameters: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"query": map[string]interface{}{
						"type":        "string",
						"description": "Symbol name or pattern",
					},
					"limit": map[string]interface{}{
						"type":        "integer",
						"description": "Maximum number of results",
					},
				},
				"required": []interface{}{"query"},
			},
		},
	}

	// 1. Verify contract generation maintains the exact namespaced name
	contract := buildContract(defs)
	if !strings.Contains(contract, "mcp__gitnexus__search_symbols") {
		t.Fatalf("contract does not contain namespaced MCP tool name: %s", contract)
	}

	// 2. Verify parsing from model output
	rawModelOutput := "<<<TOOL_CALL>>>\n{\"name\":\"mcp__gitnexus__search_symbols\",\"arguments\":{\"query\":\"Server\",\"limit\":10}}\n<<<END_TOOL_CALL>>>"
	calls := parseToolCalls(rawModelOutput, defs)
	if len(calls) != 1 {
		t.Fatalf("expected 1 tool call, got %d", len(calls))
	}
	if calls[0].Name != "mcp__gitnexus__search_symbols" {
		t.Fatalf("expected tool name 'mcp__gitnexus__search_symbols', got %q", calls[0].Name)
	}

	var args map[string]interface{}
	if err := json.Unmarshal([]byte(calls[0].Arguments), &args); err != nil {
		t.Fatalf("unmarshal arguments failed: %v", err)
	}
	if args["query"] != "Server" || args["limit"] != float64(10) {
		t.Fatalf("arguments mismatch: %+v", args)
	}

	// 3. Verify SSE stream emission produces correct Anthropic tool_use event
	w := httptest.NewRecorder()
	opts := upstream.ChatOpts{}
	writeAnthropicStream(w, "msg_mcp_test", "claude-3-7-sonnet-20250219", nil, defs, false, opts,
		func(onText, onReason func(string)) (string, string, error) {
			onText(rawModelOutput)
			return rawModelOutput, "", nil
		})

	events := decodeAnthropicEvents(t, w.Body.String())
	var toolBlockFound bool
	for _, e := range events {
		if e.Type == "content_block_start" && e.ContentBlock.Type == "tool_use" {
			if e.ContentBlock.Name == "mcp__gitnexus__search_symbols" {
				toolBlockFound = true
			}
		}
	}
	if !toolBlockFound {
		t.Fatalf("SSE events did not contain tool_use block for 'mcp__gitnexus__search_symbols'")
	}
}

// T3.2 Complex MCP Schema Validation:
// Tool mcp__database__execute_query with schema containing query: string, params: array, transaction_options: object.
func TestBenchmark_Tier3_MCP_T3_2_ComplexMCPSchemaValidation(t *testing.T) {
	defs := []ToolDef{
		{
			Name:        "mcp__database__execute_query",
			Description: "Execute a parameterized SQL query with transaction control",
			Parameters: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"query": map[string]interface{}{
						"type":        "string",
						"description": "SQL statement",
					},
					"params": map[string]interface{}{
						"type":        "array",
						"description": "Bind parameters",
						"items": map[string]interface{}{
							"type": "string",
						},
					},
					"transaction_options": map[string]interface{}{
						"type": "object",
						"properties": map[string]interface{}{
							"read_only":        map[string]interface{}{"type": "boolean"},
							"isolation_level":  map[string]interface{}{"type": "string"},
							"timeout_ms":       map[string]interface{}{"type": "integer"},
						},
					},
				},
				"required": []interface{}{"query"},
			},
		},
	}

	contract := buildContract(defs)
	if !strings.Contains(contract, "mcp__database__execute_query") {
		t.Fatalf("contract missing tool name: %s", contract)
	}

	complexSQL := "SELECT u.id, u.username, r.role_name FROM users u JOIN roles r ON u.role_id = r.id WHERE u.status = ? AND u.deleted_at IS NULL;"
	rawCall := fmt.Sprintf(`<<<TOOL_CALL>>>
{"name":"mcp__database__execute_query","arguments":{"query":%q,"params":["active"],"transaction_options":{"read_only":true,"isolation_level":"READ_COMMITTED","timeout_ms":5000}}}
<<<END_TOOL_CALL>>>`, complexSQL)

	calls := parseToolCalls(rawCall, defs)
	if len(calls) != 1 {
		t.Fatalf("expected 1 call, got %d", len(calls))
	}

	var args map[string]interface{}
	if err := json.Unmarshal([]byte(calls[0].Arguments), &args); err != nil {
		t.Fatalf("failed to unmarshal complex MCP args: %v", err)
	}

	if args["query"] != complexSQL {
		t.Fatalf("SQL query mismatch: %q", args["query"])
	}

	paramsList, ok := args["params"].([]interface{})
	if !ok || len(paramsList) != 1 || paramsList[0] != "active" {
		t.Fatalf("params mismatch: %+v", args["params"])
	}

	txOpts, ok := args["transaction_options"].(map[string]interface{})
	if !ok || txOpts["read_only"] != true || txOpts["isolation_level"] != "READ_COMMITTED" {
		t.Fatalf("transaction_options mismatch: %+v", args["transaction_options"])
	}
}

// T3.3 MCP Tool Result Round-Trip:
// Tool returns complex JSON string content; verify conversion into [Tool result for <id>]: <payload>.
func TestBenchmark_Tier3_MCP_T3_3_MCPToolResultRoundTrip(t *testing.T) {
	toolUseID := "call_mcp_gitnexus_987"
	mcpJSONResult := `{"symbols":[{"name":"Server","kind":"struct","line":25,"file":"internal/api/api.go"},{"name":"New","kind":"func","line":69,"file":"internal/api/api.go"}],"total_count":2}`

	anthroReq := anthropicRequest{
		Model: "claude-3-7-sonnet-20250219",
		Messages: []json.RawMessage{
			makeRawJSON(map[string]interface{}{
				"role":    "user",
				"content": "Search for Server struct in internal/api",
			}),
			makeRawJSON(map[string]interface{}{
				"role": "assistant",
				"content": []map[string]interface{}{
					{
						"type":  "tool_use",
						"id":    toolUseID,
						"name":  "mcp__gitnexus__search_symbols",
						"input": map[string]interface{}{"query": "Server"},
					},
				},
			}),
			makeRawJSON(map[string]interface{}{
				"role": "user",
				"content": []map[string]interface{}{
					{
						"type":        "tool_result",
						"tool_use_id": toolUseID,
						"content":     mcpJSONResult,
					},
				},
			}),
		},
	}

	openAIMsgs, err := toOpenAI(anthroReq)
	if err != nil {
		t.Fatalf("toOpenAI failed: %v", err)
	}

	if len(openAIMsgs) != 3 {
		t.Fatalf("expected 3 OpenAI messages, got %d", len(openAIMsgs))
	}

	converted := convertForTools(openAIMsgs, "TOOL_CONTRACT_HEADER")
	lastMsgRaw := converted[len(converted)-1]

	var lastMsg map[string]interface{}
	if err := json.Unmarshal(lastMsgRaw, &lastMsg); err != nil {
		t.Fatalf("unmarshal converted message failed: %v", err)
	}

	content, _ := lastMsg["content"].(string)
	expectedPrefix := fmt.Sprintf("[Tool result for %s]:", toolUseID)
	if !strings.Contains(content, expectedPrefix) {
		t.Fatalf("converted tool result missing prefix %q (got: %s)", expectedPrefix, content)
	}

	if !strings.Contains(content, mcpJSONResult) {
		t.Fatalf("converted tool result missing exact payload: %s", content)
	}
}

// T3.4 MCP Error Propagation:
// Tool result with is_error: true; verify error payload preservation and propagation.
func TestBenchmark_Tier3_MCP_T3_4_MCPErrorPropagation(t *testing.T) {
	toolUseID := "call_db_err_404"
	errorMessage := "Error: connection pool exhausted: timeout waiting for connection after 5000ms"

	anthroReq := anthropicRequest{
		Model: "claude-3-7-sonnet-20250219",
		Messages: []json.RawMessage{
			makeRawJSON(map[string]interface{}{
				"role":    "user",
				"content": "Run database query",
			}),
			makeRawJSON(map[string]interface{}{
				"role": "assistant",
				"content": []map[string]interface{}{
					{
						"type":  "tool_use",
						"id":    toolUseID,
						"name":  "mcp__database__execute_query",
						"input": map[string]interface{}{"query": "SELECT 1"},
					},
				},
			}),
			makeRawJSON(map[string]interface{}{
				"role": "user",
				"content": []map[string]interface{}{
					{
						"type":        "tool_result",
						"tool_use_id": toolUseID,
						"content":     errorMessage,
						"is_error":    true,
					},
				},
			}),
		},
	}

	openAIMsgs, err := toOpenAI(anthroReq)
	if err != nil {
		t.Fatalf("toOpenAI failed: %v", err)
	}

	converted := convertForTools(openAIMsgs, "TOOL_CONTRACT_HEADER")
	lastMsgRaw := converted[len(converted)-1]

	var lastMsg map[string]interface{}
	if err := json.Unmarshal(lastMsgRaw, &lastMsg); err != nil {
		t.Fatalf("unmarshal converted message failed: %v", err)
	}

	content, _ := lastMsg["content"].(string)

	// 1. Verify Tool ID and Error message are strictly preserved
	if !strings.Contains(content, toolUseID) {
		t.Fatalf("toolUseID %q missing from converted error message: %s", toolUseID, content)
	}
	if !strings.Contains(content, errorMessage) {
		t.Fatalf("error message %q missing from converted message: %s", errorMessage, content)
	}

	// 2. Check for prominent error tag ([Tool ERROR for <id>] vs [Tool result for <id>])
	hasErrorTag := strings.Contains(content, fmt.Sprintf("[Tool ERROR for %s]:", toolUseID))
	hasResultTag := strings.Contains(content, fmt.Sprintf("[Tool result for %s]:", toolUseID))

	if !hasErrorTag {
		t.Fatalf("expected dedicated [Tool ERROR for %s] tag when is_error: true, got: %s", toolUseID, content)
	}
	if hasResultTag {
		t.Fatalf("unexpected generic [Tool result for %s] tag when is_error: true, got: %s", toolUseID, content)
	}
	t.Logf("T3.4: Detected dedicated '[Tool ERROR for %s]' tag for is_error: true", toolUseID)
}

// ════════════════════════════════════════════════════════════════════════════════
// LIVE SERVER INTEGRATION VERIFICATION TEST (http://127.0.0.1:5084)
// ════════════════════════════════════════════════════════════════════════════════

// TestBenchmark_LiveServerRoundtrip tests against the local proxy daemon on port 5084:
// - GET  /status
// - POST /v1/messages/count_tokens
// - In-process mock messages roundtrip validation
func TestBenchmark_LiveServerRoundtrip(t *testing.T) {
	liveBaseURL := "http://127.0.0.1:5084"
	client := &http.Client{Timeout: 5 * time.Second}

	// 1. Verify GET /status
	t.Run("Live_StatusEndpoint", func(t *testing.T) {
		resp, err := client.Get(liveBaseURL + "/status")
		if err != nil {
			t.Skipf("Live server at %s not reachable (%v); skipping live test", liveBaseURL, err)
			return
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			t.Fatalf("expected 200 OK from /status, got %d", resp.StatusCode)
		}

		var status map[string]interface{}
		if err := json.NewDecoder(resp.Body).Decode(&status); err != nil {
			t.Fatalf("failed to decode /status JSON: %v", err)
		}

		if status["service"] != "glm-aki-proxy" {
			t.Errorf("expected service 'glm-aki-proxy', got %v", status["service"])
		}
		if status["connected"] != true {
			t.Errorf("expected connected=true, got %v", status["connected"])
		}
		accountsTotal, _ := status["accounts_total"].(float64)
		if accountsTotal < 1 {
			t.Errorf("expected at least 1 account, got %v", accountsTotal)
		}
		t.Logf("Live server /status OK: %v accounts healthy (%v total)", status["accounts_healthy"], status["accounts_total"])
	})

	// 2. Verify POST /v1/messages/count_tokens
	t.Run("Live_CountTokensEndpoint", func(t *testing.T) {
		reqBody := map[string]interface{}{
			"model": "claude-3-7-sonnet-20250219",
			"messages": []map[string]interface{}{
				{"role": "user", "content": "Benchmark test prompt for token estimation."},
			},
		}
		b, _ := json.Marshal(reqBody)
		req, err := http.NewRequest(http.MethodPost, liveBaseURL+"/v1/messages/count_tokens", bytes.NewReader(b))
		if err != nil {
			t.Fatalf("create request failed: %v", err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("x-api-key", "aki-local-key")

		resp, err := client.Do(req)
		if err != nil {
			t.Skipf("Live server at %s not reachable (%v); skipping live test", liveBaseURL, err)
			return
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			t.Fatalf("expected 200 OK from /v1/messages/count_tokens, got %d", resp.StatusCode)
		}

		var res map[string]interface{}
		if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
			t.Fatalf("failed to decode count_tokens response: %v", err)
		}

		inputTokens, ok := res["input_tokens"].(float64)
		if !ok || inputTokens <= 0 {
			t.Fatalf("invalid input_tokens value: %v", res["input_tokens"])
		}
		t.Logf("Live server /v1/messages/count_tokens OK: %v tokens calculated", inputTokens)
	})

	// 3. In-Process Full Pipeline Verification for /v1/messages
	t.Run("InProcess_MessagesPipelineRoundtrip", func(t *testing.T) {
		// Mock upstream that simulates a standard tool call turn
		defs := []ToolDef{
			{Name: "Echo", Description: "Echo text", Parameters: map[string]interface{}{"type": "object"}},
		}
		w := httptest.NewRecorder()
		rawToolOutput := "<<<TOOL_CALL>>>\n{\"name\":\"Echo\",\"arguments\":{\"text\":\"hello\"}}\n<<<END_TOOL_CALL>>>"

		writeAnthropicStream(w, "msg_mock_pipeline", "claude-3-7-sonnet-20250219", nil, defs, false, upstream.ChatOpts{},
			func(onText, onReason func(string)) (string, string, error) {
				onText(rawToolOutput)
				return rawToolOutput, "", nil
			})

		events := decodeAnthropicEvents(t, w.Body.String())
		if len(events) < 4 {
			t.Fatalf("expected at least 4 SSE events in lifecycle, got %d", len(events))
		}

		if events[0].Type != "message_start" {
			t.Errorf("first event must be message_start, got %q", events[0].Type)
		}
		lastEvent := events[len(events)-1]
		if lastEvent.Type != "message_stop" {
			t.Errorf("last event must be message_stop, got %q", lastEvent.Type)
		}

		penultimate := events[len(events)-2]
		if penultimate.Delta.StopReason != "tool_use" {
			t.Errorf("stop_reason must be tool_use, got %q", penultimate.Delta.StopReason)
		}
	})
}

// ── helper ──
func makeRawJSON(v interface{}) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}
