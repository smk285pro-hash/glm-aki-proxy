package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// ── 1. repairArgs Adversarial Stress Tests ──

func TestAdversarialRepairArgs(t *testing.T) {
	t.Run("WindowsPaths_LowercaseUsers", func(t *testing.T) {
		// LLMs very frequently output lowercase Windows paths: C:\users\...
		input := `{"file": "C:\users\admin\project\main.go"}`
		repaired := repairArgs(input)
		if !json.Valid([]byte(repaired)) {
			t.Errorf("FAIL: C:\\users was not repaired to valid JSON: %q", repaired)
		}
		var m map[string]string
		if err := json.Unmarshal([]byte(repaired), &m); err != nil {
			t.Errorf("FAIL: cannot unmarshal C:\\users: %v (raw repaired: %q)", err, repaired)
		} else if !strings.Contains(m["file"], "users") {
			t.Errorf("FAIL: path corrupted: %q", m["file"])
		}
	})

	t.Run("WindowsPaths_OtherControlEscapesInPath", func(t *testing.T) {
		// Test paths containing \t, \n, \r, \b, \f in folder names
		testCases := []struct {
			name  string
			input string
		}{
			{"NewFolder", `{"path": "C:\new_project\notes.txt"}`},
			{"TestDir", `{"path": "C:\tools\test_runner\run.bat"}`},
			{"BinDir", `{"path": "C:\bin\build.exe"}`},
			{"ReleaseDir", `{"path": "C:\release\v1.0\app.exe"}`},
			{"FilesDir", `{"path": "C:\files\data.csv"}`},
		}

		for _, tc := range testCases {
			t.Run(tc.name, func(t *testing.T) {
				repaired := repairArgs(tc.input)
				if !json.Valid([]byte(repaired)) {
					t.Errorf("FAIL: %s did not produce valid JSON: %q", tc.name, repaired)
				}
				var m map[string]string
				if err := json.Unmarshal([]byte(repaired), &m); err != nil {
					t.Errorf("FAIL: %s unmarshal failed: %v", tc.name, err)
				}
				t.Logf("[%s] unmarshaled path: %q", tc.name, m["path"])
				if strings.ContainsRune(m["path"], '\t') || strings.ContainsRune(m["path"], '\n') || strings.ContainsRune(m["path"], '\r') {
					t.Errorf("FAIL: %s converted Windows folder name to control character in path: %q", tc.name, m["path"])
				}
			})
		}
	})

	t.Run("NestedJSON_TruncationAtVariousPoints", func(t *testing.T) {
		testCases := []struct {
			name  string
			input string
		}{
			{"TruncatedInDeepArray", `{"a": {"b": [{"c": 1}]`},
			{"TruncatedMidValue", `{"a": {"b": [{"c": 1`},
			{"TruncatedMidString", `{"a": {"b": [{"c": "hello world`},
			{"TruncatedAfterArrayOpen", `{"a": {"b": [`},
			{"TruncatedAfterKey", `{"a": {"b": [{"c":`},
			{"TruncatedEmptyObject", `{"a": {"b": {`},
		}

		for _, tc := range testCases {
			t.Run(tc.name, func(t *testing.T) {
				repaired := repairArgs(tc.input)
				valid := json.Valid([]byte(repaired))
				t.Logf("[%s] input: %q -> repaired: %q (valid: %v)", tc.name, tc.input, repaired, valid)
				if !valid {
					t.Errorf("FAIL: %s was not repaired to valid JSON: %q", tc.name, repaired)
				}
			})
		}
	})

	t.Run("TrailingCommas_AdversarialPositions", func(t *testing.T) {
		testCases := []struct {
			name  string
			input string
		}{
			{"EmptyArrayTrailingComma", `[,]`},
			{"ArrayTrailingCommaWithSpaces", `[ 1 , 2 ,   ]`},
			{"ObjectTrailingCommaWithSpaces", `{ "a" : 1 ,   }`},
			{"NestedTrailingCommas", `{"a": [1, 2, ], "b": {"c": 3, }, }`},
			{"DoubleTrailingComma", `{"a": 1, , }`},
		}

		for _, tc := range testCases {
			t.Run(tc.name, func(t *testing.T) {
				repaired := repairArgs(tc.input)
				valid := json.Valid([]byte(repaired))
				t.Logf("[%s] input: %q -> repaired: %q (valid: %v)", tc.name, tc.input, repaired, valid)
				if !valid {
					t.Errorf("FAIL: %s was not repaired to valid JSON: %q", tc.name, repaired)
				}
			})
		}
	})

	t.Run("MultiLayerStringifiedJSON", func(t *testing.T) {
		testCases := []struct {
			name  string
			input string
		}{
			{"TripleStringified", `"\"{\\\"foo\\\": \\\"bar\\\"}\""`},
			{"QuadrupleStringified", `"\"\\\"{\\\\\\\"foo\\\\\\\": \\\\\\\"bar\\\\\\\"}\\\"\""`},
		}

		for _, tc := range testCases {
			t.Run(tc.name, func(t *testing.T) {
				repaired := repairArgs(tc.input)
				t.Logf("[%s] input: %q -> repaired: %q", tc.name, tc.input, repaired)
				var m map[string]interface{}
				err := json.Unmarshal([]byte(repaired), &m)
				if err != nil {
					t.Errorf("FAIL: %s unmarshal failed: %v (got %q)", tc.name, err, repaired)
				} else if m["foo"] != "bar" {
					t.Errorf("FAIL: %s unexpected value: %+v", tc.name, m)
				}
			})
		}
	})

	t.Run("RegexAndEscapedQuotesInCode", func(t *testing.T) {
		testCases := []struct {
			name  string
			input string
		}{
			{"RegexDigitsAndSpaces", `{"pattern": "\d+\s*"}`},
			{"RegexWordBoundary", `{"pattern": "\bword\b"}`},
			{"CodeWithEscapedQuotes", `{"code": "const x = \"hello \\\"world\\\"\";"}`},
			{"CodeWithColonInsideQuote", `{"code": "const x = \":\";"}`},
			{"CodeWithCommaInsideQuote", `{"code": "const x = \",\";"}`},
			{"CodeWithBraceInsideQuote", `{"code": "const x = \"}\";"}`},
		}

		for _, tc := range testCases {
			t.Run(tc.name, func(t *testing.T) {
				repaired := repairArgs(tc.input)
				if !json.Valid([]byte(repaired)) {
					t.Errorf("FAIL: %s produced invalid JSON: %q", tc.name, repaired)
				}
				var m map[string]string
				if err := json.Unmarshal([]byte(repaired), &m); err != nil {
					t.Errorf("FAIL: %s cannot unmarshal: %v", tc.name, err)
				}
			})
		}
	})
}

// ── 2. sanitizeOutputText Adversarial Stress Tests ──

func TestAdversarialSanitizeOutputText(t *testing.T) {
	t.Run("ReplayTokensAndStrayMarkers", func(t *testing.T) {
		testCases := []struct {
			name        string
			input       string
			mustNotHave []string
		}{
			{
				"ChineseReplayVariants",
				"TOOL_CALL回放开始...\nTOOL_CALL 回放\n回放结束。 继续分析。\nTOOL_CALL回放：正在调用工具。\n回放开始：\nKết quả phân tích hoàn tất.",
				[]string{"TOOL_CALL", "回放", "继续分析"},
			},
			{
				"ChineseLegitimateReplayTV",
				"请问昨天的新闻联播回放可以在哪里观看？",
				nil,
			},
			{
				"StrayXMLAndMarkerTags",
				"<tool_call>{\"name\": \"test\"}</tool_call>\n<<<TOOL_CALL>>>\n<<<END_TOOL_CALL>>>\n<<TOOL>>\n<<<TOOL_CALL\nNội dung chính",
				[]string{"<tool_call>", "</tool_call>", "<<<TOOL_CALL>>>", "<<<END_TOOL_CALL>>>", "<<TOOL>>", "<<<TOOL_CALL"},
			},
			{
				"InternalSystemHeaderLeak",
				"[SYSTEM INSTRUCTION — INTERNAL TOOL PROTOCOL V2]\nRule 1: call tools\n[END INTERNAL TOOL PROTOCOL]\nXin chào bạn!",
				[]string{"[SYSTEM INSTRUCTION", "INTERNAL TOOL PROTOCOL", "[END INTERNAL TOOL PROTOCOL]"},
			},
		}

		for _, tc := range testCases {
			t.Run(tc.name, func(t *testing.T) {
				sanitized := sanitizeOutputText(tc.input)
				t.Logf("[%s] sanitized: %q", tc.name, sanitized)
				for _, forbidden := range tc.mustNotHave {
					if strings.Contains(sanitized, forbidden) {
						t.Errorf("FAIL: %s contains forbidden token %q: %q", tc.name, forbidden, sanitized)
					}
				}
			})
		}
	})

	t.Run("PreservationOfLegitimateCodeAndText", func(t *testing.T) {
		testCases := []struct {
			name  string
			input string
			want  string
		}{
			{
				"CodeWithComparisonOperators",
				"if (a < b && c > d) { return a < d; }",
				"if (a < b && c > d) { return a < d; }",
			},
			{
				"HTMLStringInCode",
				"const el = '<div><span>Test</span></div>';",
				"const el = '<div><span>Test</span></div>';",
			},
			{
				"CppTemplateSyntax",
				"std::vector<std::string> items = get_items();",
				"std::vector<std::string> items = get_items();",
			},
			{
				"LegitimateChineseProse",
				"这是一个关于人工智能的讨论。我们将分析数据的增长趋势。\n开始处理数据，请稍候。\n分析结束，得出以下结论：",
				"这是一个关于人工智能的讨论。我们将分析数据的增长趋势。\n开始处理数据，请稍候。\n分析结束，得出以下结论：",
			},
		}

		for _, tc := range testCases {
			t.Run(tc.name, func(t *testing.T) {
				got := sanitizeOutputText(tc.input)
				if strings.TrimSpace(got) != strings.TrimSpace(tc.want) {
					t.Errorf("FAIL: %s mangled legitimate content:\nGot:  %q\nWant: %q", tc.name, got, tc.want)
				}
			})
		}
	})
}

// ── 3. Token Count Endpoint Adversarial Tests ──

func TestAdversarialCountTokensEndpoint(t *testing.T) {
	s := &Server{}

	t.Run("MethodNotAllowedOnGET", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/v1/messages/count_tokens", nil)
		w := httptest.NewRecorder()
		s.handleCountTokens(w, req)

		if w.Code != http.StatusMethodNotAllowed {
			t.Errorf("expected status 405 on GET, got %d", w.Code)
		}
	})

	t.Run("MalformedJSONBody", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/v1/messages/count_tokens", strings.NewReader(`{bad json`))
		w := httptest.NewRecorder()
		s.handleCountTokens(w, req)

		if w.Code != http.StatusBadRequest {
			t.Errorf("expected status 400 on malformed json, got %d", w.Code)
		}
	})

	t.Run("ValidPayloadWithToolsAndSystem", func(t *testing.T) {
		payload := map[string]interface{}{
			"model":  "claude-3-5-sonnet-20241022",
			"system": "You are a senior engineer.",
			"messages": []map[string]interface{}{
				{"role": "user", "content": "Analyze this codebase and run tests."},
			},
			"tools": []map[string]interface{}{
				{
					"name":        "Bash",
					"description": "Execute bash commands",
					"input_schema": map[string]interface{}{
						"type": "object",
						"properties": map[string]interface{}{
							"command": map[string]interface{}{"type": "string"},
						},
						"required": []string{"command"},
					},
				},
			},
		}

		body, _ := json.Marshal(payload)
		req := httptest.NewRequest(http.MethodPost, "/v1/messages/count_tokens", bytes.NewReader(body))
		w := httptest.NewRecorder()
		s.handleCountTokens(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("expected status 200, got %d, body: %s", w.Code, w.Body.String())
		}

		var resp map[string]interface{}
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatalf("failed to parse response: %v", err)
		}

		tokens, ok := resp["input_tokens"].(float64)
		if !ok || tokens <= 0 {
			t.Errorf("expected positive input_tokens, got %v", resp["input_tokens"])
		}
	})
}

// ── 4. isPreambleFluff Adversarial Stress Tests ──

func TestAdversarialPreambleFluff(t *testing.T) {
	testCases := []struct {
		name      string
		preamble  string
		wantFluff bool
	}{
		{"DocFile", "Tôi sẽ đọc file config.go.", true},
		{"XemFile", "Tôi sẽ xem nội dung file main.go.", true},
		{"SuaFile", "Tôi sẽ sửa file api.go.", true},
		{"TaoFile", "Tôi sẽ tạo file test.go.", true},
		{"TimKiem", "Tôi sẽ tìm kiếm hàm repairArgs.", true},
		{"LietKe", "Tôi sẽ liệt kê các file trong thư mục.", true},
		{"IWillInspect", "I will inspect the codebase.", true},
		{"IWillList", "I will list the directory contents.", true},
		{"LegitimateUserExplanation", "File config.go chứa cấu hình kết nối database của hệ thống.", false},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			got := isPreambleFluff(tc.preamble)
			t.Logf("[%s] %q -> isPreambleFluff: %v (want: %v)", tc.name, tc.preamble, got, tc.wantFluff)
			if got != tc.wantFluff {
				t.Errorf("FAIL: isPreambleFluff(%q) = %v, want %v", tc.preamble, got, tc.wantFluff)
			}
		})
	}
}

