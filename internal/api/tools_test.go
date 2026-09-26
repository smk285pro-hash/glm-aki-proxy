// Tests for the tool-call adapter: contract parsing strategies,
// argument repair, the streaming interceptor, message conversion,
// and conversation turn locking.
package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"
)

func testDefs() []ToolDef {
	return []ToolDef{
		{Name: "get_time", Description: "Get the current time", Parameters: map[string]interface{}{"type": "object"}},
		{Name: "write_file", Description: "Write content to a file", Parameters: map[string]interface{}{
			"type":       "object",
			"required":   []interface{}{"path"},
			"properties": map[string]interface{}{"path": map[string]interface{}{"type": "string"}, "content": map[string]interface{}{"type": "string"}},
		}},
	}
}

// ── parsing strategies ──

func TestParseMarkerCalls(t *testing.T) {
	text := "Let me check.\n\n<<<TOOL_CALL>>>\n{\"name\":\"get_time\",\"arguments\":{}}\n<<<END_TOOL_CALL>>>\n\nDone."
	calls := parseToolCalls(text, testDefs())
	if len(calls) != 1 {
		t.Fatalf("want 1 call, got %d", len(calls))
	}
	if calls[0].Name != "get_time" || calls[0].Arguments != "{}" {
		t.Fatalf("bad call: %+v", calls[0])
	}
}

func TestParseMarkerCorruptedEnd(t *testing.T) {
	text := "<<<TOOL_CALL>>>\n{\"name\":\"get_time\",\"arguments\":{}}\n<<<END_TOOL_CALL   >>>"
	if got := parseToolCalls(text, testDefs()); len(got) != 1 {
		t.Fatalf("want 1 call with corrupted end marker, got %d", len(got))
	}
}

func TestParseFencedCalls(t *testing.T) {
	text := "Calling now:\n```json\n{\"name\":\"write_file\",\"arguments\":{\"path\":\"a.txt\",\"content\":\"hi\"}}\n```"
	calls := parseToolCalls(text, testDefs())
	if len(calls) != 1 || calls[0].Name != "write_file" {
		t.Fatalf("want write_file, got %+v", calls)
	}
	var args map[string]string
	if json.Unmarshal([]byte(calls[0].Arguments), &args) != nil || args["path"] != "a.txt" {
		t.Fatalf("bad args: %s", calls[0].Arguments)
	}
}

func TestParseBareAndEmbedded(t *testing.T) {
	bare := `{"name":"get_time","arguments":{}}`
	if got := parseToolCalls(bare, testDefs()); len(got) != 1 {
		t.Fatalf("bare JSON: want 1, got %d", len(got))
	}
	embedded := `Sure thing, here: {"name":"get_time","arguments":{}} ok?`
	if got := parseToolCalls(embedded, testDefs()); len(got) != 1 {
		t.Fatalf("embedded JSON: want 1, got %d", len(got))
	}
}

func TestParseLineAndTagCalls(t *testing.T) {
	multi := "{\"name\":\"get_time\",\"arguments\":{}}\n{\"name\":\"get_time\",\"arguments\":{}}"
	if got := parseToolCalls(multi, testDefs()); len(got) != 1 { // deduped
		t.Fatalf("multiline: want 1 deduped, got %d", len(got))
	}
	tag := "<<TOOL>>\n{\"name\":\"get_time\",\"arguments\":{}}\n<</TOOL>>"
	if got := parseToolCalls(tag, testDefs()); len(got) != 1 {
		t.Fatalf("tag: want 1, got %d", len(got))
	}
}

func TestParseSingleTagVariant(t *testing.T) {
	text := `Let me do it:
<tool-call>
{"name":"get_time","arguments":{}}
</tool-call>`
	calls := parseToolCalls(text, testDefs())
	if len(calls) != 1 || calls[0].Name != "get_time" {
		t.Fatalf("want get_time from <tool-call>, got %+v", calls)
	}
	if got := stripToolBlocks(text); strings.Contains(got, "tool-call") || strings.Contains(got, "get_time") {
		t.Fatalf("single-tag must be stripped: %q", got)
	}
	text2 := `<tool_call>{"name":"write_file","arguments":{"path":"a.txt"}}</tool_call>`
	if got := parseToolCalls(text2, testDefs()); len(got) != 1 || got[0].Name != "write_file" {
		t.Fatalf("want write_file from <tool_call>, got %+v", got)
	}
}

func TestParseClaudeXMLToolCall(t *testing.T) {
	defs := []ToolDef{{Name: "Read"}, {Name: "Glob"}}
	text := `<function_calls><invoke name="Read"><parameter name="file_path">C:\work\PLAN.md</parameter><parameter name="limit">60</parameter></invoke></function_calls>`
	calls := parseToolCalls(text, defs)
	if len(calls) != 1 || calls[0].Name != "Read" {
		t.Fatalf("want one Read call, got %+v", calls)
	}
	var args map[string]interface{}
	if err := json.Unmarshal([]byte(calls[0].Arguments), &args); err != nil {
		t.Fatalf("invalid arguments: %v (%s)", err, calls[0].Arguments)
	}
	if args["file_path"] != `C:\work\PLAN.md` || args["limit"] != float64(60) {
		t.Fatalf("wrong arguments: %#v", args)
	}
}

func TestParseClaudePseudoToolCalls(t *testing.T) {
	defs := []ToolDef{{Name: "Glob"}, {Name: "Read"}}
	text := `Mình xem nhanh. Glob(pattern: "**/*.md") Read(file_path: C:\work\PLAN.md, limit: 60)`
	calls := parseToolCalls(text, defs)
	if len(calls) != 2 || calls[0].Name != "Glob" || calls[1].Name != "Read" {
		t.Fatalf("want Glob + Read, got %+v", calls)
	}
	var args map[string]interface{}
	if json.Unmarshal([]byte(calls[1].Arguments), &args) != nil || args["file_path"] != `C:\work\PLAN.md` || args["limit"] != float64(60) {
		t.Fatalf("wrong pseudo arguments: %s", calls[1].Arguments)
	}
	if got := stripToolBlocksForDefs(text, defs); strings.Contains(got, "Glob(") || strings.Contains(got, "Read(") {
		t.Fatalf("pseudo call leaked into residue: %q", got)
	}
}

func TestParseClaudeCodeWindowsBatchCalls(t *testing.T) {
	// Claude Code sometimes emits a compact batch without separators between
	// calls. Keep this exact shape covered because it must become three
	// structured tool_use blocks instead of assistant text.
	defs := []ToolDef{{Name: "Read"}, {Name: "Glob"}}
	text := `Read(file_path="C:\Users\smk28\Desktop\reals lab extension\PLAN.md")Read(file_path="C:\Users\smk28\Desktop\reals lab extension\SPEC.md")Glob(pattern="README.md", path="C:\Users\smk28\Desktop\reals lab extension")`
	calls := parseToolCalls(text, defs)
	if len(calls) != 3 {
		t.Fatalf("want three calls, got %+v", calls)
	}
	if calls[0].Name != "Read" || calls[1].Name != "Read" || calls[2].Name != "Glob" {
		t.Fatalf("wrong call sequence: %+v", calls)
	}
	var first, second, third map[string]interface{}
	if err := json.Unmarshal([]byte(calls[0].Arguments), &first); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(calls[1].Arguments), &second); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(calls[2].Arguments), &third); err != nil {
		t.Fatal(err)
	}
	if first["file_path"] != `C:\Users\smk28\Desktop\reals lab extension\PLAN.md` ||
		second["file_path"] != `C:\Users\smk28\Desktop\reals lab extension\SPEC.md` ||
		third["pattern"] != "README.md" ||
		third["path"] != `C:\Users\smk28\Desktop\reals lab extension` {
		t.Fatalf("wrong arguments: %#v / %#v / %#v", first, second, third)
	}
	if residue := stripToolBlocksForDefs(text, defs); residue != "" {
		t.Fatalf("batch syntax leaked into answer: %q", residue)
	}
}

func TestParseClaudeAssignmentPseudoToolCalls(t *testing.T) {
	defs := []ToolDef{{Name: "Glob"}, {Name: "Read"}}
	text := `Để mình đọc tài liệu.Read(file_path:="C:\Users\smk28\Desktop\reals lab extension\PLAN.md")Read(file_path:="C:\Users\smk28\Desktop\reals lab extension\SPEC.md")Glob(pattern:="README*", path:="C:\Users\smk28\Desktop\reals lab extension")`
	calls := parseToolCalls(text, defs)
	if len(calls) != 3 || calls[0].Name != "Read" || calls[1].Name != "Read" || calls[2].Name != "Glob" {
		t.Fatalf("want Read + Read + Glob, got %+v", calls)
	}
	var args map[string]interface{}
	if err := json.Unmarshal([]byte(calls[0].Arguments), &args); err != nil || args["file_path"] != `C:\Users\smk28\Desktop\reals lab extension\PLAN.md` {
		t.Fatalf("wrong := Read args: %s", calls[0].Arguments)
	}
	if got := stripToolBlocksForDefs(text, defs); got != "Để mình đọc tài liệu." {
		t.Fatalf("assignment pseudo call leaked into residue: %q", got)
	}
}

func TestParseMalformedClaudeDesktopToolCalls(t *testing.T) {
	defs := []ToolDef{{Name: "Glob"}, {Name: "Read"}}
	text := `Mình xem nhanh cấu trúc dự án để trả lời chính xác.Glob(pattern: *</arg_value>Read(file_path: C:\work\PLAN.md</arg_value><arg_key>limit: 60</arg_value>Read(file_path: C:\work\SPEC.md</arg_value><arg_key>limit: 80</arg_value>Read(file_path: C:\work\AGENTS.md</arg_value><arg_key>limit: 60</arg_value>`
	calls := parseToolCalls(text, defs)
	if len(calls) != 4 {
		t.Fatalf("want 4 calls from the malformed Desktop reply, got %+v", calls)
	}
	if calls[0].Name != "Glob" || calls[1].Name != "Read" || calls[2].Name != "Read" || calls[3].Name != "Read" {
		t.Fatalf("wrong call sequence: %+v", calls)
	}
	for i, want := range []float64{60, 80, 60} {
		var args map[string]interface{}
		if err := json.Unmarshal([]byte(calls[i+1].Arguments), &args); err != nil {
			t.Fatalf("call %d arguments: %v", i+1, err)
		}
		if args["limit"] != want {
			t.Fatalf("call %d limit: %#v", i+1, args)
		}
	}
	if got := stripToolBlocksForDefs(text, defs); got != "Mình xem nhanh cấu trúc dự án để trả lời chính xác." {
		t.Fatalf("malformed syntax leaked into answer: %q", got)
	}
}

func TestLegacyToolStreamHoldback(t *testing.T) {
	defs := []ToolDef{{Name: "Read"}}
	f := newToolFilter(defs)
	if got, _ := f.feed("I will Read(file_"); strings.Contains(got, "Read") {
		t.Fatalf("legacy call leaked before completion: %q", got)
	}
	if got, _ := f.feed(`path: C:\work\PLAN.md)`); got != "" {
		t.Fatalf("held legacy call produced text: %q", got)
	}
	if got := f.flushText(); got != "" {
		t.Fatalf("held legacy call must not flush as text: %q", got)
	}
}

func TestParseNaturalEcho(t *testing.T) {
	text := `I'll write it: echo "Hello World" > hello.txt`
	calls := parseToolCalls(text, testDefs())
	if len(calls) != 1 || calls[0].Name != "write_file" {
		t.Fatalf("want write_file from echo, got %+v", calls)
	}
}

func TestParseRejectsUnknownAndBadArgs(t *testing.T) {
	defs := testDefs()
	if got := parseToolCalls(`<<<TOOL_CALL>>>
{"name":"nope","arguments":{}}
<<<END_TOOL_CALL>>>`, defs); len(got) != 0 {
		t.Fatalf("unknown tool must be dropped, got %+v", got)
	}
	if got := parseToolCalls(`{"name":"get_time","arguments":[1,2]}`, defs); len(got) != 0 {
		t.Fatalf("non-object args must be dropped, got %+v", got)
	}
}

func TestParseFirstStrategyWins(t *testing.T) {
	text := "<<<TOOL_CALL>>>\n{\"name\":\"get_time\",\"arguments\":{}}\n<<<END_TOOL_CALL>>>\n```json\n{\"name\":\"write_file\",\"arguments\":{\"path\":\"x\"}}\n```"
	calls := parseToolCalls(text, testDefs())
	if len(calls) != 1 || calls[0].Name != "get_time" {
		t.Fatalf("marker strategy must win, got %+v", calls)
	}
}

// ── repair & strip ──

func TestRepairArgs(t *testing.T) {
	fixed := repairArgs(`{"x":"say "hi" ok"}`)
	if !json.Valid([]byte(fixed)) {
		t.Fatalf("repairArgs did not produce valid JSON: %s", fixed)
	}
	var m map[string]string
	if json.Unmarshal([]byte(fixed), &m) != nil || m["x"] != `say "hi" ok` {
		t.Fatalf("repairArgs mangled content: %s", fixed)
	}
	raw := repairArgs(`{"a":"line1
line2"}`)
	if !strings.Contains(raw, `\n`) || strings.Contains(raw, "\n") {
		t.Fatalf("repairArgs must escape raw newlines inside strings: %q", raw)
	}
}

func TestStripToolBlocks(t *testing.T) {
	text := "Preamble here.\n\n<<<TOOL_CALL>>>\n{\"name\":\"get_time\",\"arguments\":{}}\n<<<END_TOOL_CALL>>>\n\nTail."
	got := stripToolBlocks(text)
	if strings.Contains(got, "TOOL_CALL") || strings.Contains(got, "get_time") {
		t.Fatalf("marker leaked: %q", got)
	}
	if !strings.Contains(got, "Preamble") || !strings.Contains(got, "Tail") {
		t.Fatalf("residue lost: %q", got)
	}
}

// ── streaming filter ──

func TestFilterPlainHoldback(t *testing.T) {
	f := newToolFilter()
	if txt, deltas := f.feed("hello"); txt != "" || len(deltas) != 0 {
		t.Fatalf("short plain text must be held back, got %q %+v", txt, deltas)
	}
	if tail := f.flushText(); tail != "hello" {
		t.Fatalf("flushText must release held text, got %q", tail)
	}
}

func TestFilterSplitMarker(t *testing.T) {
	f := newToolFilter()
	if txt, _ := f.feed("Hi <<<TOOL"); txt != "" {
		t.Fatalf("partial marker must not leak, got %q", txt)
	}
	txt, deltas := f.feed("_CALL>>>\n{\"name\":\"get_time\",\"arguments\":{}}\n<<<END_TOOL_CALL>>>\nBye")
	if txt != "Hi " {
		t.Fatalf("want preamble %q, got %q", "Hi ", txt)
	}
	var sawName, sawArgs bool
	for _, d := range deltas {
		if d.HasNameOnly && d.Name == "get_time" {
			sawName = true
		}
		if d.ArgsFrag != "" {
			sawArgs = true
		}
	}
	if !sawName || !sawArgs {
		t.Fatalf("want name+args deltas, got %+v", deltas)
	}
	if tail := f.flushText(); tail != "Bye" {
		t.Fatalf("want trailing %q, got %q", "Bye", tail)
	}
}

func TestFilterIncrementalArgs(t *testing.T) {
	f := newToolFilter()
	f.feed(">>> pre ")
	_, d1 := f.feed("<<<TOOL_CALL>>>\n{\"name\":\"write_file\",\"arguments\":")
	var sawName bool
	for _, d := range d1 {
		if d.HasNameOnly && d.Name == "write_file" {
			sawName = true
		}
		if d.ArgsFrag != "" {
			t.Fatalf("args must not stream before the object opens: %+v", d)
		}
	}
	if !sawName {
		t.Fatalf("name must stream as soon as visible: %+v", d1)
	}
	_, d2 := f.feed(`{"path":"a.txt"}}` + "\n<<<END_TOOL_CALL>>>")
	joined := ""
	for _, d := range d2 {
		joined += d.ArgsFrag
	}
	if joined != `{"path":"a.txt"}` {
		t.Fatalf("args must reassemble exactly, got %q", joined)
	}
}

func TestFilterBufferedFallback(t *testing.T) {
	f := newToolFilter()
	_, deltas := f.feed("<<<TOOL_CALL>>>\n{\"name\":\"get_time\",\"arguments\":\"oops\"}\n<<<END_TOOL_CALL>>>")
	found := false
	for _, d := range deltas {
		if d.Name == "get_time" && d.CallDone {
			found = true
		}
	}
	if !found {
		t.Fatalf("fallback must emit completed call, got %+v", deltas)
	}
}

// ── definitions & conversion ──

func TestNormalizeTools(t *testing.T) {
	openAI := json.RawMessage(`{"type":"function","function":{"name":"f","description":"d","parameters":{"type":"object"}}}`)
	anthropic := json.RawMessage(`{"name":"g","description":"e","input_schema":{"type":"object"}}`)
	defs := normalizeTools([]json.RawMessage{openAI, anthropic, json.RawMessage(`{"nope":1}`)})
	if len(defs) != 2 || defs[0].Name != "f" || defs[1].Name != "g" {
		t.Fatalf("bad normalize: %+v", defs)
	}
}

func TestBuildContract(t *testing.T) {
	c := buildContract(testDefs())
	if !strings.Contains(c, "<<<TOOL_CALL>>>") || !strings.Contains(c, "- get_time") {
		t.Fatalf("contract missing marker/list:\n%s", c)
	}
	if !strings.Contains(c, "write_file(path: str") {
		t.Fatalf("contract missing compact signature:\n%s", c)
	}
}

func msg(role, content string) json.RawMessage {
	b, _ := json.Marshal(map[string]string{"role": role, "content": content})
	return b
}

func TestConvertForTools(t *testing.T) {
	framing := "CONTRACT"
	toolMsg, _ := json.Marshal(map[string]string{"role": "tool", "tool_call_id": "call_1", "content": "02:30"})
	asst := map[string]interface{}{
		"role": "assistant", "content": "checking",
		"tool_calls": []interface{}{map[string]interface{}{
			"id": "call_1", "type": "function",
			"function": map[string]string{"name": "get_time", "arguments": "{}"},
		}},
	}
	asstRaw, _ := json.Marshal(asst)
	in := []json.RawMessage{msg("user", "what time?"), asstRaw, toolMsg, msg("user", "and now?")}
	out := convertForTools(in, framing)
	if len(out) != 4 {
		t.Fatalf("want 4 messages, got %d", len(out))
	}
	var last map[string]string
	json.Unmarshal(out[3], &last)
	if !strings.HasPrefix(last["content"], "CONTRACT") {
		t.Fatalf("framing must ride the last user message: %q", last["content"])
	}
	var tool map[string]string
	json.Unmarshal(out[2], &tool)
	if tool["role"] != "user" || !strings.Contains(tool["content"], "02:30") {
		t.Fatalf("tool result must become user message: %q", tool["content"])
	}
	var prev map[string]string
	json.Unmarshal(out[1], &prev)
	if !strings.Contains(prev["content"], `"name":"get_time"`) {
		t.Fatalf("assistant calls must flatten to text: %q", prev["content"])
	}
}

func TestConvertEmptyNudge(t *testing.T) {
	out := convertForTools([]json.RawMessage{msg("user", "hi"), msg("user", "")}, "CONTRACT")
	var last map[string]string
	json.Unmarshal(out[len(out)-1], &last)
	if !strings.Contains(last["content"], "emit its tool call block immediately") {
		t.Fatalf("empty nudge must get continue-directive: %q", last["content"])
	}
}

// ── UTF-8 safety: no emitted fragment may split a multi-byte rune
// (json.Marshal would replace the halves with U+FFFD) ──

func TestFilterUTF8Splits(t *testing.T) {
	src := "Chào bạn, cấu trúc repo này rất hay nhé!"
	f := newToolFilter()
	var out strings.Builder
	// Feed in awkward 5-byte slices to force cuts inside runes.
	for i := 0; i < len(src); i += 5 {
		end := i + 5
		if end > len(src) {
			end = len(src)
		}
		txt, deltas := f.feed(src[i:end])
		if len(deltas) != 0 {
			t.Fatalf("plain text must yield no tool deltas: %+v", deltas)
		}
		if !utf8.ValidString(txt) {
			t.Fatalf("emitted fragment is not valid UTF-8: %q", txt)
		}
		if raw, _ := json.Marshal(txt); strings.Contains(string(raw), "�") {
			t.Fatalf("fragment would marshal to U+FFFD: %q", txt)
		}
		out.WriteString(txt)
	}
	out.WriteString(f.flushText())
	if out.String() != src {
		t.Fatalf("reassembly mismatch:\nwant %q\ngot  %q", src, out.String())
	}
}

func TestFilterUTF8ArgsSplit(t *testing.T) {
	// "path" value holds Vietnamese; the args object is split mid-rune.
	f := newToolFilter()
	joined := ""
	collect := func(deltas []toolDelta) {
		for _, d := range deltas {
			if d.ArgsFrag != "" && !utf8.ValidString(d.ArgsFrag) {
				t.Fatalf("args fragment is not valid UTF-8: %q", d.ArgsFrag)
			}
			joined += d.ArgsFrag
		}
	}
	_, d1 := f.feed("ok <<<TOOL_CALL>>>\n{\"name\":\"write_file\",\"arguments\":{\"path\":\"thư")
	collect(d1)
	_, d2 := f.feed(" mục\",\"content\":\"x\"}}\n<<<END_TOOL_CALL>>>")
	collect(d2)
	if joined != `{"path":"thư mục","content":"x"}` {
		t.Fatalf("args must reassemble exactly, got %q", joined)
	}
}

func TestSystemPromptCapping(t *testing.T) {
	hugeSys := strings.Repeat("A", 15000) + "MIDDLE" + strings.Repeat("B", 15000)
	req := anthropicRequest{
		System: json.RawMessage(fmt.Sprintf("%q", hugeSys)),
		Messages: []json.RawMessage{
			json.RawMessage(`{"role":"user","content":"hi"}`),
		},
	}
	msgs, err := toOpenAI(req)
	if err != nil {
		t.Fatalf("toOpenAI failed: %v", err)
	}
	if len(msgs) < 2 {
		t.Fatalf("expected at least 2 messages, got %d", len(msgs))
	}
	var sysMsg struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	}
	if err := json.Unmarshal(msgs[0], &sysMsg); err != nil {
		t.Fatalf("failed to unmarshal sys msg: %v", err)
	}
	if sysMsg.Role != "system" {
		t.Fatalf("expected role system, got %s", sysMsg.Role)
	}
	if len(sysMsg.Content) > maxSystem+50 {
		t.Fatalf("system prompt should be capped at maxSystem=%d, got %d", maxSystem, len(sysMsg.Content))
	}
	if !strings.Contains(sysMsg.Content, "...[truncated]...") {
		t.Fatalf("expected truncation marker in huge system prompt")
	}
	if !strings.HasPrefix(sysMsg.Content, MoraAkiPersona) {
		t.Fatalf("system prompt missing MoraAkiPersona prefix")
	}
	if !strings.HasSuffix(sysMsg.Content, "BBBBBBBBBB") {
		t.Fatalf("expected tail of system prompt to be preserved")
	}
}

func TestStripAnthropicNoise(t *testing.T) {
	input := "Hello world\n<system-reminder>\nSome huge git status or reminder\n</system-reminder>\nx-anthropic-billing-header: test\nEnd"
	cleaned := stripAnthropicNoise(input)
	if strings.Contains(cleaned, "system-reminder") || strings.Contains(cleaned, "billing-header") {
		t.Fatalf("noise not stripped: %q", cleaned)
	}
	if !strings.Contains(cleaned, "Hello world") || !strings.Contains(cleaned, "End") {
		t.Fatalf("content lost: %q", cleaned)
	}
}

func TestLimitAnthropicTools(t *testing.T) {
	var tools []ToolDef
	for i := 0; i < 40; i++ {
		tools = append(tools, ToolDef{Name: fmt.Sprintf("custom_tool_%d", i)})
	}
	tools = append(tools, ToolDef{Name: "Edit"}, ToolDef{Name: "Read"}, ToolDef{Name: "Bash"})
	limited := limitAnthropicTools(tools)
	if len(limited) != maxAnthropicTools {
		t.Fatalf("expected %d tools, got %d", maxAnthropicTools, len(limited))
	}
	if limited[0].Name != "Read" || limited[1].Name != "Edit" || limited[2].Name != "Bash" {
		t.Fatalf("expected core tools at front, got: %s, %s, %s", limited[0].Name, limited[1].Name, limited[2].Name)
	}
}

func TestParseScreenshotDirectXMLCall(t *testing.T) {
	text := `The user is asking in Vietnamese: "dự án này có chức năng gì" which means "What functionality does this project have?"

I need to explore the project to understand what it does. Let me look at the current directory structure first. I should respond in Vietnamese per the instructions.

Let me start by listing files in the current directory.
Bash<arg_key>command</arg_key><arg_value>ls -la</arg_value><arg_key>description</arg_key><arg_value>Liệt kê file trong thư mục hiện tại</arg_value>`

	defs := []ToolDef{{Name: "Bash", Parameters: map[string]interface{}{"command": map[string]interface{}{"type": "string"}}}}
	calls := parseToolCalls(text, defs)
	if len(calls) != 1 {
		t.Fatalf("expected 1 call, got %d", len(calls))
	}
	if calls[0].Name != "Bash" {
		t.Fatalf("expected tool Bash, got %s", calls[0].Name)
	}
	if !strings.Contains(calls[0].Arguments, "ls -la") {
		t.Fatalf("expected ls -la in args, got %s", calls[0].Arguments)
	}
	residual := sanitizeOutputText(stripToolBlocksForDefs(text, defs))
	if !isPreambleFluff(residual) {
		t.Fatalf("expected residual to be recognized as preamble fluff, got %q", residual)
	}
}

func TestParseDirectXMLSequentialMultiCalls(t *testing.T) {
	text := "Bash<arg_key>command</arg_key><arg_value>git status</arg_value>\nRead<arg_key>file_path</arg_key><arg_value>go.mod</arg_value>"
	defs := []ToolDef{
		{Name: "Bash", Parameters: map[string]interface{}{"command": map[string]interface{}{"type": "string"}}},
		{Name: "Read", Parameters: map[string]interface{}{"file_path": map[string]interface{}{"type": "string"}}},
	}
	calls := parseToolCalls(text, defs)
	if len(calls) != 2 {
		t.Fatalf("expected 2 calls, got %d", len(calls))
	}
	if calls[0].Name != "Bash" || !strings.Contains(calls[0].Arguments, "git status") {
		t.Fatalf("call 0 mismatch: %+v", calls[0])
	}
	if calls[1].Name != "Read" || !strings.Contains(calls[1].Arguments, "go.mod") {
		t.Fatalf("call 1 mismatch: %+v", calls[1])
	}
}

func TestParseDirectXMLParameterTag(t *testing.T) {
	text := `Bash<parameter name="command">go build ./...</parameter>`
	defs := []ToolDef{{Name: "Bash", Parameters: map[string]interface{}{"command": map[string]interface{}{"type": "string"}}}}
	calls := parseToolCalls(text, defs)
	if len(calls) != 1 {
		t.Fatalf("expected 1 call, got %d", len(calls))
	}
	if calls[0].Name != "Bash" || !strings.Contains(calls[0].Arguments, "go build ./...") {
		t.Fatalf("call mismatch: %+v", calls[0])
	}
	residual := sanitizeOutputText(stripToolBlocksForDefs(text, defs))
	if strings.TrimSpace(residual) != "" {
		t.Fatalf("expected empty residual after stripping, got %q", residual)
	}
}

func TestMergeBashDescriptionAndCommand(t *testing.T) {
	text := "Bash<arg_key>description</arg_key><arg_value>Failed to list files in current directory</arg_value>\nBash<arg_key>command</arg_key><arg_value>ls -la</arg_value>"
	defs := []ToolDef{{Name: "Bash", Parameters: map[string]interface{}{"command": map[string]interface{}{"type": "string"}}}}
	calls := parseToolCalls(text, defs)
	if len(calls) != 1 {
		t.Fatalf("expected 1 merged call, got %d: %+v", len(calls), calls)
	}
	if calls[0].Name != "Bash" {
		t.Fatalf("expected Bash, got %s", calls[0].Name)
	}
	if !strings.Contains(calls[0].Arguments, "ls -la") {
		t.Fatalf("expected command in args, got %s", calls[0].Arguments)
	}
	if !strings.Contains(calls[0].Arguments, "Failed to list files") {
		t.Fatalf("expected description in args, got %s", calls[0].Arguments)
	}
}

func TestDropBashWithoutCommand(t *testing.T) {
	text := "Bash<arg_key>description</arg_key><arg_value>only description without command</arg_value>"
	defs := []ToolDef{{Name: "Bash", Parameters: map[string]interface{}{"command": map[string]interface{}{"type": "string"}}}}
	calls := parseToolCalls(text, defs)
	if len(calls) != 0 {
		t.Fatalf("expected 0 calls (dropped invalid Bash call), got %d: %+v", len(calls), calls)
	}
}

func TestSanitizeFilePath(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{
			in:   "/mnt/[/PROJECT.md|/workspace/[/PROJECT.md|./PROJECT.md",
			want: "./PROJECT.md",
		},
		{
			in:   "[/PROJECT.md|./PROJECT.md",
			want: "./PROJECT.md",
		},
		{
			in:   "[PROJECT.md](file:///foo/bar/PROJECT.md)",
			want: "PROJECT.md",
		},
		{
			in:   "`src/index.ts`",
			want: "src/index.ts",
		},
		{
			in:   "internal/api/tools.go",
			want: "internal/api/tools.go",
		},
	}
	for _, tc := range cases {
		got := sanitizeFilePath(tc.in)
		if got != tc.want {
			t.Errorf("sanitizeFilePath(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestReadToolHallucinatedPathSanitized(t *testing.T) {
	text := `Read<arg_key>file_path</arg_key><arg_value>/mnt/[/PROJECT.md|/workspace/[/PROJECT.md|./PROJECT.md</arg_value>`
	defs := []ToolDef{{Name: "Read", Parameters: map[string]interface{}{"file_path": map[string]interface{}{"type": "string"}}}}
	calls := parseToolCalls(text, defs)
	if len(calls) != 1 {
		t.Fatalf("expected 1 call, got %d", len(calls))
	}
	if !strings.Contains(calls[0].Arguments, `"./PROJECT.md"`) {
		t.Fatalf("expected sanitized path ./PROJECT.md, got %s", calls[0].Arguments)
	}
}

func TestParseDirectAttributeCall_LeakedBashSyntax(t *testing.T) {
	defs := []ToolDef{
		{
			Name: "Bash",
			Parameters: map[string]interface{}{
				"properties": map[string]interface{}{
					"command":     map[string]interface{}{"type": "string"},
					"description": map[string]interface{}{"type": "string"},
				},
				"required": []interface{}{"command"},
			},
		},
	}

	text := `The user is asking in Vietnamese "repo này có chức năng gì" (what does this repo do / what's the function of this repo). I need to explore the repository to understand what it does. Let me look at the files in the current directory.

I should use tools to explore. Let me start by listing files and reading README if it exists.

Bash command="ls -la</arg_value><arg_key>descriptionLiệt
Explore`

	calls := parseToolCalls(text, defs)
	if len(calls) != 1 {
		t.Fatalf("expected 1 call, got %d", len(calls))
	}
	if calls[0].Name != "Bash" {
		t.Errorf("expected Bash, got %s", calls[0].Name)
	}
	if calls[0].argsMap()["command"] != "ls -la" {
		t.Errorf("expected command 'ls -la', got %v", calls[0].argsMap()["command"])
	}

	residual := stripToolBlocksForDefs(text, defs)
	if !isPreambleFluff(residual) {
		t.Errorf("expected residual to be preamble fluff, got %q", residual)
	}
}

func TestStripLegacyCallSyntax_PreservesNormalProse(t *testing.T) {
	defs := []ToolDef{
		{
			Name: "Bash",
			Parameters: map[string]interface{}{
				"properties": map[string]interface{}{
					"command": map[string]interface{}{"type": "string"},
				},
				"required": []interface{}{"command"},
			},
		},
	}

	text := "Bash command=\"git status\"\nĐây là kết quả kiểm tra trạng thái git hiện tại."
	calls := parseToolCalls(text, defs)
	if len(calls) != 1 {
		t.Fatalf("expected 1 call, got %d", len(calls))
	}
	if calls[0].argsMap()["command"] != "git status" {
		t.Errorf("expected command 'git status', got %v", calls[0].argsMap()["command"])
	}

	residual := stripToolBlocksForDefs(text, defs)
	expected := "Đây là kết quả kiểm tra trạng thái git hiện tại."
	if residual != expected {
		t.Errorf("expected residual %q, got %q", expected, residual)
	}
}

func TestParseDirectAttributeCall_SequentialMultiLine(t *testing.T) {
	defs := []ToolDef{
		{
			Name: "Bash",
			Parameters: map[string]interface{}{
				"properties": map[string]interface{}{
					"command": map[string]interface{}{"type": "string"},
				},
				"required": []interface{}{"command"},
			},
		},
		{
			Name: "Read",
			Parameters: map[string]interface{}{
				"properties": map[string]interface{}{
					"file_path": map[string]interface{}{"type": "string"},
				},
				"required": []interface{}{"file_path"},
			},
		},
	}

	text := "Bash command=\"echo 1\"\nRead file_path=\"foo.txt\""
	calls := parseToolCalls(text, defs)
	if len(calls) != 2 {
		t.Fatalf("expected 2 calls, got %d", len(calls))
	}
	if calls[0].Name != "Bash" || calls[0].argsMap()["command"] != "echo 1" {
		t.Errorf("unexpected call 0: %+v", calls[0])
	}
	if calls[1].Name != "Read" || calls[1].argsMap()["file_path"] != "foo.txt" {
		t.Errorf("unexpected call 1: %+v", calls[1])
	}

	residual := stripToolBlocksForDefs(text, defs)
	if residual != "" {
		t.Errorf("expected empty residual, got %q", residual)
	}
}

func TestParseDirectAttributeCall_MultiLineScriptWithBlankLines(t *testing.T) {
	defs := []ToolDef{
		{
			Name: "Bash",
			Parameters: map[string]interface{}{
				"properties": map[string]interface{}{
					"command": map[string]interface{}{"type": "string"},
				},
				"required": []interface{}{"command"},
			},
		},
	}

	cmd := "cat << 'EOF' > file.txt\nline 1\n\nline 2\nEOF"
	text := "Bash command=\"cat << 'EOF' > file.txt\nline 1\n\nline 2\nEOF\"\nTiếp tục xử lý."
	calls := parseToolCalls(text, defs)
	if len(calls) != 1 {
		t.Fatalf("expected 1 call, got %d", len(calls))
	}
	if calls[0].argsMap()["command"] != cmd {
		t.Errorf("expected multiline command, got %v", calls[0].argsMap()["command"])
	}

	residual := stripToolBlocksForDefs(text, defs)
	expected := "Tiếp tục xử lý."
	if residual != expected {
		t.Errorf("expected residual %q, got %q", expected, residual)
	}
}

// ── conversation locking ──

func TestConversationBeginCommitConcurrent(t *testing.T) {
	c := &conversation{chatID: "test"}
	const n = 50
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			h := c.begin()
			_ = h
			b, _ := json.Marshal(map[string]string{"role": "user", "content": fmt.Sprintf("m%d", i)})
			c.commit(b)
		}(i)
	}
	wg.Wait()
	h := c.begin()
	defer c.abort()
	if len(h) != n {
		t.Fatalf("lost updates under concurrency: want %d, got %d", n, len(h))
	}
	seen := map[string]bool{}
	for _, raw := range h {
		var m map[string]string
		json.Unmarshal(raw, &m)
		seen[m["content"]] = true
	}
	for i := 0; i < n; i++ {
		if !seen[fmt.Sprintf("m%d", i)] {
			t.Fatalf("missing message m%d", i)
		}
	}
}

func TestConversationAbortReleases(t *testing.T) {
	c := &conversation{chatID: "test"}
	c.begin()
	c.abort()
	done := make(chan struct{})
	go func() { c.begin(); c.abort(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatalf("abort must release the turn lock")
	}
}

func TestSanitizeOutputText(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"<tool_call>TOOL_CALL回放结束。继续分析。\nXin chào", "Xin chào"},
		{"TOOL_CALL回放结束。继续分析。\nĐang kiểm tra", "Đang kiểm tra"},
		{"回放结束。继续分析。\nOK", "OK"},
		{"<tool_call>TOOL_CALL回放结束", ""},
		{"<tool_call>Xin chào</tool_call>", "Xin chào"},
		{"<tool-call>Xin chào</tool-call>", "Xin chào"},
		{"Bình thường không có tag", "Bình thường không có tag"},
	}
	for _, tc := range cases {
		got := strings.TrimSpace(sanitizeOutputText(tc.in))
		if got != tc.want {
			t.Errorf("sanitizeOutputText(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestFilterStripsReplayMarker(t *testing.T) {
	f := newToolFilter()
	chunk := "<tool_call>TOOL_CALL回放结束。继续分析。\nEm xem qua cấu trúc dự án để trả lời anh."
	plain, deltas := f.feed(chunk)
	tail := f.flushText()
	all := plain + tail
	if strings.Contains(all, "回放") || strings.Contains(all, "<tool_call>") {
		t.Fatalf("replay marker leaked into stream: %q", all)
	}
	if !strings.Contains(all, "Em xem qua cấu trúc") {
		t.Fatalf("legitimate text lost: %q", all)
	}
	if len(deltas) != 0 {
		t.Fatalf("should not parse replay marker as deltas: %+v", deltas)
	}
}

func TestFilterStripsSplitReplayMarker(t *testing.T) {
	f := newToolFilter()
	p1, _ := f.feed("<tool_call>TOOL_")
	p2, _ := f.feed("CALL回放结束。继续分析。\nĐang kiểm tra.")
	tail := f.flushText()
	all := p1 + p2 + tail
	if strings.Contains(all, "回放") || strings.Contains(all, "<tool_call>") {
		t.Fatalf("split replay marker leaked: %q", all)
	}
	if !strings.Contains(all, "Đang kiểm tra.") {
		t.Fatalf("legitimate text lost: %q", all)
	}
}

func TestFilterTagToolCallDetection(t *testing.T) {
	f := newToolFilter()
	chunk := "<tool_call>\n{\"name\":\"get_time\",\"arguments\":{}}\n</tool_call>"
	_, deltas := f.feed(chunk)
	f.flushText()
	found := false
	for _, d := range deltas {
		if d.Name == "get_time" {
			found = true
		}
	}
	if !found {
		t.Fatalf("failed to detect <tool_call> with JSON as tool call: %+v", deltas)
	}
}

func TestConvertForToolsAssistantEncapsulation(t *testing.T) {
	framing := "CONTRACT"
	asst := map[string]interface{}{
		"role": "assistant", "content": "Em xem qua cấu trúc dự án",
		"tool_calls": []interface{}{map[string]interface{}{
			"id": "call_1", "type": "function",
			"function": map[string]string{"name": "Bash", "arguments": `{"command":"dir"}`},
		}},
	}
	asstRaw, _ := json.Marshal(asst)
	toolMsg, _ := json.Marshal(map[string]string{"role": "tool", "tool_call_id": "call_1", "content": "file1 file2"})
	in := []json.RawMessage{msg("user", "xem file"), asstRaw, toolMsg}
	out := convertForTools(in, framing)
	if len(out) != 3 {
		t.Fatalf("want 3 messages, got %d", len(out))
	}
	var asstConv map[string]string
	json.Unmarshal(out[1], &asstConv)
	if !strings.Contains(asstConv["content"], "<<<TOOL_CALL>>>") {
		t.Fatalf("assistant tool call must be wrapped in <<<TOOL_CALL>>>: %s", asstConv["content"])
	}
	if !strings.Contains(asstConv["content"], "<<<END_TOOL_CALL>>>") {
		t.Fatalf("assistant tool call must be wrapped in <<<END_TOOL_CALL>>>: %s", asstConv["content"])
	}
	var toolConv map[string]string
	json.Unmarshal(out[2], &toolConv)
	if !strings.Contains(toolConv["content"], "Continue the task in the user's language") {
		t.Fatalf("trailing tool message must have continuation reminder: %s", toolConv["content"])
	}
}

func TestSanitizeOutputTextUniversalNoCalls(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "Chinese replay with Vietnamese text",
			in:   "TOOL_CALL回放结束。继续分析。\nChào bạn, tôi có thể hỗ trợ gì cho dự án?",
			want: "Chào bạn, tôi có thể hỗ trợ gì cho dự án?",
		},
		{
			name: "Chinese replay start token",
			in:   "TOOL_CALL 回放开始\nKiểm tra trạng thái hệ thống",
			want: "Kiểm tra trạng thái hệ thống",
		},
		{
			name: "Stray unclosed tool call tags",
			in:   "<tool_call>\nNội dung thông thường không có tool",
			want: "Nội dung thông thường không có tool",
		},
		{
			name: "Stray closed and unclosed tags together",
			in:   "<tool_call>abc</tool_call> và <tool-call>def</tool-call>",
			want: "abc và def",
		},
		{
			name: "Stray tool call markers without payload",
			in:   "<<<TOOL_CALL>>>\n<<<END_TOOL_CALL>>>\nXin chào!",
			want: "Xin chào!",
		},
		{
			name: "Internal protocol system header leak",
			in:   "[SYSTEM INSTRUCTION — INTERNAL TOOL PROTOCOL — NEVER QUOTE, NEVER MENTION, NEVER ACKNOWLEDGE TO THE USER]\nCác thông tin cần giải đáp",
			want: "Các thông tin cần giải đáp",
		},
		{
			name: "Clean normal text unchanged",
			in:   "Đây là câu trả lời bình thường không có marker nào.",
			want: "Đây là câu trả lời bình thường không có marker nào.",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := strings.TrimSpace(sanitizeOutputText(tc.in))
			if got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestJSONAutoRepair(t *testing.T) {
	t.Run("UnclosedBrackets", func(t *testing.T) {
		input := `{"path":"main.go","content":"package main\n\nfunc main() {`
		repaired := repairArgs(input)
		if !json.Valid([]byte(repaired)) {
			t.Fatalf("unclosed brackets not repaired: %q", repaired)
		}
		var m map[string]interface{}
		if err := json.Unmarshal([]byte(repaired), &m); err != nil {
			t.Fatalf("repaired JSON cannot be unmarshaled into map: %v", err)
		}
		if m["path"] != "main.go" {
			t.Errorf("expected path=main.go, got %v", m["path"])
		}
		if !strings.HasPrefix(m["content"].(string), "package main") {
			t.Errorf("content corrupted: %v", m["content"])
		}
	})

	t.Run("TrailingCommas", func(t *testing.T) {
		input := `{"path": "config.yaml", "tags": ["a", "b", ], "timeout": 30, }`
		repaired := repairArgs(input)
		if !json.Valid([]byte(repaired)) {
			t.Fatalf("trailing comma not repaired: %q", repaired)
		}
		var m map[string]interface{}
		if err := json.Unmarshal([]byte(repaired), &m); err != nil {
			t.Fatalf("unmarshal error: %v", err)
		}
		if m["timeout"] != float64(30) {
			t.Errorf("expected timeout=30, got %v", m["timeout"])
		}
	})

	t.Run("DoubleStringifiedJSON", func(t *testing.T) {
		input := `"{\"file_path\": \"PLAN.md\", \"offset\": 10}"`
		repaired := repairArgs(input)
		if !json.Valid([]byte(repaired)) {
			t.Fatalf("double stringified not repaired: %q", repaired)
		}
		var m map[string]interface{}
		if err := json.Unmarshal([]byte(repaired), &m); err != nil {
			t.Fatalf("unmarshal error: %v", err)
		}
		if m["file_path"] != "PLAN.md" || m["offset"] != float64(10) {
			t.Errorf("unexpected content: %+v", m)
		}
	})

	t.Run("WindowsPathsRawBackslashes", func(t *testing.T) {
		input := `{"file": "C:\Users\smk28\Desktop\project\main.go"}`
		repaired := repairArgs(input)
		if !json.Valid([]byte(repaired)) {
			t.Fatalf("raw backslashes in Windows path not repaired: %q", repaired)
		}
		var m map[string]string
		if err := json.Unmarshal([]byte(repaired), &m); err != nil {
			t.Fatalf("unmarshal error: %v", err)
		}
		if !strings.Contains(m["file"], `Users`) {
			t.Errorf("path mangled: %q", m["file"])
		}
	})

	t.Run("AliasKeysInNameArgs", func(t *testing.T) {
		// 1. input alias
		name, args, ok := nameArgs(`{"name": "Bash", "input": {"command": "go test ./..."}}`)
		if !ok || name != "Bash" || !strings.Contains(args, "go test ./...") {
			t.Errorf("failed input alias: name=%s, args=%s, ok=%v", name, args, ok)
		}

		// 2. parameters alias
		name, args, ok = nameArgs(`{"name": "Read", "parameters": {"file_path": "main.go"}}`)
		if !ok || name != "Read" || !strings.Contains(args, "main.go") {
			t.Errorf("failed parameters alias: name=%s, args=%s, ok=%v", name, args, ok)
		}

		// 3. args alias with action
		name, args, ok = nameArgs(`{"action": "Grep", "args": {"pattern": "Test"}}`)
		if !ok || name != "Grep" || !strings.Contains(args, "Test") {
			t.Errorf("failed args alias: name=%s, args=%s, ok=%v", name, args, ok)
		}

		// 4. double-stringified arguments in nameArgs
		name, args, ok = nameArgs(`{"name": "Read", "arguments": "{\"file_path\": \"PLAN.md\"}"}`)
		if !ok || name != "Read" || !strings.Contains(args, "PLAN.md") {
			t.Errorf("failed double-stringified in nameArgs: name=%s, args=%s, ok=%v", name, args, ok)
		}
	})
}

func TestUTF8DescriptionTruncation(t *testing.T) {
	longVietnameseDesc := "Đây là công cụ hỗ trợ người dùng quét và phân tích mã nguồn một cách toàn diện và chính xác nhất theo tiêu chuẩn dự án."
	defs := []ToolDef{
		{
			Name:        "CodeScanner",
			Description: longVietnameseDesc,
			Parameters:  map[string]interface{}{"type": "object"},
		},
	}
	contract := buildContract(defs)
	if !utf8.ValidString(contract) {
		t.Fatalf("buildContract produced invalid UTF-8 string")
	}
	if !strings.Contains(contract, "- CodeScanner") {
		t.Fatalf("missing tool name in contract")
	}
	if !strings.Contains(contract, "...") {
		t.Fatalf("expected truncation ellipsis in description")
	}
	runes := []rune(longVietnameseDesc)
	expectedPrefix := string(runes[:80]) + "..."
	if !strings.Contains(contract, expectedPrefix) {
		t.Errorf("contract does not contain expected rune-truncated prefix: %q", expectedPrefix)
	}
}

func TestPreambleRemovalWhenToolCallsGenerated(t *testing.T) {
	fluffCases := []string{
		"Tôi sẽ dùng công cụ Read để đọc file.",
		"Tôi sẽ sử dụng tool Bash để chạy test.",
		"Theo quy tắc công cụ, tôi sẽ gọi Read.",
		"Theo protocol, tôi sẽ thực hiện thao tác sau:",
		"I will use tool to read the file.",
		"Let me check the project files:",
		"Calling tool now...",
		"   \n\t  \n",
	}
	for _, text := range fluffCases {
		if !isPreambleFluff(text) {
			t.Errorf("expected isPreambleFluff(%q) = true, got false", text)
		}
	}

	nonFluffCases := []string{
		"Kết quả kiểm tra toàn diện cho thấy 100% test case đã vượt qua xuất sắc.",
		"Sau khi xem xét mã nguồn, tôi phát hiện 2 điểm cần cải tiến quan trọng.",
	}
	for _, text := range nonFluffCases {
		if isPreambleFluff(text) {
			t.Errorf("expected isPreambleFluff(%q) = false, got true", text)
		}
	}

	defs := []ToolDef{{Name: "Read", Parameters: map[string]interface{}{"type": "object"}}}
	rawResponse := "Theo quy tắc công cụ, tôi sẽ gọi Read:\n<<<TOOL_CALL>>>\n{\"name\":\"Read\",\"arguments\":{\"file_path\":\"config.go\"}}\n<<<END_TOOL_CALL>>>"

	calls := parseToolCalls(rawResponse, defs)
	if len(calls) != 1 || calls[0].Name != "Read" {
		t.Fatalf("failed to parse tool call: %+v", calls)
	}
	residual := stripToolBlocksForDefs(rawResponse, defs)
	if !isPreambleFluff(residual) {
		t.Fatalf("expected residual to be preamble fluff, got %q", residual)
	}
}

func TestCountTokensEndpoint(t *testing.T) {
	s := &Server{}
	reqBody := `{
		"model": "claude-3-7-sonnet-20250219",
		"system": "You are a helpful assistant.",
		"messages": [
			{"role": "user", "content": "Hello, how many tokens is this?"}
		],
		"tools": [
			{
				"name": "Read",
				"description": "Read a file from disk",
				"input_schema": {
					"type": "object",
					"properties": {
						"file_path": {"type": "string"}
					}
				}
			}
		]
	}`

	req := httptest.NewRequest(http.MethodPost, "/v1/messages/count_tokens", strings.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	s.handleCountTokens(w, req)

	resp := w.Result()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected status 200 OK, got %d", resp.StatusCode)
	}

	var result map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatalf("failed to decode response JSON: %v", err)
	}

	tokens, ok := result["input_tokens"].(float64)
	if !ok || tokens < 1 {
		t.Fatalf("expected input_tokens >= 1, got %v", result["input_tokens"])
	}
}

func TestIsWindowsPathString_DelimiterBoundaries(t *testing.T) {
	// 1. False-positive edge cases that must NOT be classified as Windows paths
	nonPathCases := []struct {
		name string
		s    string
		key  string
	}{
		{"WordEndingInTabEscape", "Tab:\tSeparated", "message"},
		{"WordEndingInNewlineEscape", "Label:\nIndented", "text"},
		{"HttpBackslash", `http:\something`, "url"},
		{"HttpUrl", "http://example.com/api", "url"},
		{"HttpsUrl", "https://api.anthropic.com/v1/messages", "endpoint"},
		{"ColonStatus", "Status: active", "state"},
		{"CodeWithLabel", "main:\n\treturn", "code"},
	}

	for _, tc := range nonPathCases {
		t.Run("NonPath_"+tc.name, func(t *testing.T) {
			if isWindowsPathString(tc.s, tc.key) {
				t.Errorf("isWindowsPathString(%q, %q) = true, expected false", tc.s, tc.key)
			}
		})
	}

	// 2. Genuine Windows paths that MUST be classified as Windows paths
	pathCases := []struct {
		name string
		s    string
		key  string
	}{
		{"DriveBackslashUppercase", `C:\Users\admin\project\main.go`, "file"},
		{"DriveBackslashLowercase", `c:\users\admin\project\main.go`, "file"},
		{"DriveForwardSlash", `D:/projects/go/main.go`, "file"},
		{"QuotedDrivePath", `"E:\data\dataset.csv"`, "dest"},
		{"FlagAssignedDrivePath", `path=C:\tools\build.bat`, "cmd"},
		{"ParenDrivePath", `(C:\tools\app.exe)`, "target"},
		{"UNCPath", `\\server\share\file.txt`, "file"},
		{"RelativeCurrentDir", `.\relative\path\file.go`, "file"},
		{"RelativeParentDir", `..\parent\path\file.go`, "file"},
		{"KeyNamedPathWithBackslash", `somedir\somefile.txt`, "path"},
		{"KeyNamedDirWithBackslash", `vendor\modules`, "dir"},
		{"KeyNamedCwdWithBackslash", `src\internal`, "cwd"},
	}

	for _, tc := range pathCases {
		t.Run("Path_"+tc.name, func(t *testing.T) {
			if !isWindowsPathString(tc.s, tc.key) {
				t.Errorf("isWindowsPathString(%q, %q) = false, expected true", tc.s, tc.key)
			}
		})
	}

	// 3. Verify repairArgs preserves tabs in non-path strings like Tab:\t
	t.Run("RepairArgs_PreservesTabsInNonPath", func(t *testing.T) {
		input := `{"message": "Tab:\tValue"}`
		repaired := repairArgs(input)
		var m map[string]string
		if err := json.Unmarshal([]byte(repaired), &m); err != nil {
			t.Fatalf("unmarshal error: %v", err)
		}
		// The value should contain the tab character '\t', not escaped '\\t'
		if m["message"] != "Tab:\tValue" {
			t.Errorf("expected Tab:\\tValue, got %q", m["message"])
		}
	})
}

func TestConvertForTools_ToolErrorTag(t *testing.T) {
	// 1. Tool result with is_error = false or omitted -> [Tool result for <id>]
	toolMsgSuccess, _ := json.Marshal(map[string]interface{}{
		"role":         "tool",
		"tool_call_id": "call_succ_123",
		"content":      "File saved successfully",
	})
	convertedSucc := convertForTools([]json.RawMessage{toolMsgSuccess}, "CONTRACT")
	var succMap map[string]interface{}
	json.Unmarshal(convertedSucc[len(convertedSucc)-1], &succMap)
	succContent, _ := succMap["content"].(string)
	if !strings.Contains(succContent, "[Tool result for call_succ_123]: File saved successfully") {
		t.Errorf("expected [Tool result for ...] header, got %q", succContent)
	}

	// 2. Tool result with is_error = true (bool) -> [Tool ERROR for <id>]
	toolMsgErrBool, _ := json.Marshal(map[string]interface{}{
		"role":         "tool",
		"tool_call_id": "call_err_456",
		"content":      "Permission denied",
		"is_error":     true,
	})
	convertedErrBool := convertForTools([]json.RawMessage{toolMsgErrBool}, "CONTRACT")
	var errBoolMap map[string]interface{}
	json.Unmarshal(convertedErrBool[len(convertedErrBool)-1], &errBoolMap)
	errBoolContent, _ := errBoolMap["content"].(string)
	if !strings.Contains(errBoolContent, "[Tool ERROR for call_err_456]: Permission denied") {
		t.Errorf("expected [Tool ERROR for ...] header, got %q", errBoolContent)
	}

	// 3. Tool result with is_error = "true" (string) -> [Tool ERROR for <id>]
	toolMsgErrStr, _ := json.Marshal(map[string]interface{}{
		"role":         "tool",
		"tool_call_id": "call_err_789",
		"content":      "Command timed out",
		"is_error":     "true",
	})
	convertedErrStr := convertForTools([]json.RawMessage{toolMsgErrStr}, "CONTRACT")
	var errStrMap map[string]interface{}
	json.Unmarshal(convertedErrStr[len(convertedErrStr)-1], &errStrMap)
	errStrContent, _ := errStrMap["content"].(string)
	if !strings.Contains(errStrContent, "[Tool ERROR for call_err_789]: Command timed out") {
		t.Errorf("expected [Tool ERROR for ...] header, got %q", errStrContent)
	}
}

func TestMoraAkiPersonaInjection(t *testing.T) {
	// Test injectPersonaToSystem empty
	if injectPersonaToSystem("") != MoraAkiPersona {
		t.Fatalf("expected MoraAkiPersona on empty system")
	}
	// Test injectPersonaToSystem with existing
	combined := injectPersonaToSystem("custom system instruction")
	if !strings.HasPrefix(combined, MoraAkiPersona) || !strings.HasSuffix(combined, "custom system instruction") {
		t.Fatalf("expected persona prefix and original suffix, got %q", combined)
	}

	// Test injectPersonaToMessages with empty
	emptyMsgs := injectPersonaToMessages(nil)
	if len(emptyMsgs) != 1 {
		t.Fatalf("expected 1 message for empty input, got %d", len(emptyMsgs))
	}

	// Test injectPersonaToMessages with user message only
	userMsg, _ := json.Marshal(map[string]string{"role": "user", "content": "hello"})
	msgsWithUser := injectPersonaToMessages([]json.RawMessage{userMsg})
	if len(msgsWithUser) != 2 {
		t.Fatalf("expected 2 messages (system prepended), got %d", len(msgsWithUser))
	}

	// Test injectPersonaToMessages with existing system message
	sysMsg, _ := json.Marshal(map[string]string{"role": "system", "content": "existing prompt"})
	msgsWithSys := injectPersonaToMessages([]json.RawMessage{sysMsg, userMsg})
	if len(msgsWithSys) != 2 {
		t.Fatalf("expected 2 messages (system augmented), got %d", len(msgsWithSys))
	}
	var augmentedSys map[string]interface{}
	json.Unmarshal(msgsWithSys[0], &augmentedSys)
	augContent, _ := augmentedSys["content"].(string)
	if !strings.HasPrefix(augContent, MoraAkiPersona) || !strings.HasSuffix(augContent, "existing prompt") {
		t.Fatalf("augmented system content wrong: %q", augContent)
	}
}
