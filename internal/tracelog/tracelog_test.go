package tracelog

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// codexDeclared 是新版 Codex 的工具声明（additional_tools，namespace functions）。
var codexDeclared = DeclaredTools(mustJSON(`{"input":[{"type":"additional_tools","tools":[
	{"type":"namespace","name":"functions","tools":[{"type":"custom","name":"exec"},{"type":"function","name":"wait"},{"type":"function","name":"request_user_input"}]},
	{"type":"namespace","name":"collaboration","tools":[{"type":"function","name":"spawn_agent"}]}]}]}`))

func mustJSON(raw string) map[string]any {
	var m map[string]any
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		panic(err)
	}
	return m
}

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// 真实 basispoints 返回（2026-09-27 抓包，已脱敏）。
func TestAnalyzeRealResponses(t *testing.T) {
	cases := map[string]struct {
		outcome string
		unknown []string
	}{
		"bps_exec.sse":               {OutcomeToolCall, nil},
		"bps_request_user_input.sse": {OutcomeUnknownTool, []string{"request_user_input_basispoints"}},
		"bps_excel_tools.sse":        {OutcomeUnknownTool, []string{"list_skills", "read_sheets_metadata"}},
	}
	for name, tc := range cases {
		result := Analyze(fixture(t, name), codexDeclared)
		if result.Outcome != tc.outcome || strings.Join(result.UnknownTools, ",") != strings.Join(tc.unknown, ",") {
			t.Errorf("%s: outcome=%s unknown=%v tools=%v", name, result.Outcome, result.UnknownTools, result.Tools)
		}
	}
}

func sse(events ...string) []byte {
	var b strings.Builder
	for _, e := range events {
		b.WriteString("event: x\ndata: " + e + "\n\n")
	}
	return []byte(b.String())
}

func TestAnalyzeOutcomes(t *testing.T) {
	commentary := `{"type":"response.completed","response":{"output":[{"type":"message","phase":"commentary","content":[{"type":"output_text","text":"我现在直接写入修改。"}]}]}}`
	final := `{"type":"response.completed","response":{"output":[{"type":"message","phase":"final_answer","content":[{"type":"output_text","text":"done"}]}]}}`
	nsCall := `{"type":"response.completed","response":{"output":[{"type":"function_call","name":"spawn_agent","namespace":"collaboration"}]}}`
	cases := []struct {
		name    string
		body    []byte
		outcome string
		text    string
	}{
		{"commentary only", sse(`{"type":"response.created"}`, commentary), OutcomeCommentaryOnly, "我现在直接写入修改。"},
		{"final answer", sse(final), OutcomeText, "done"},
		{"namespaced declared call", sse(nsCall), OutcomeToolCall, ""},
		{"cut off", sse(`{"type":"response.created"}`, `{"type":"response.output_text.delta","delta":"x"}`), OutcomeNoCompleted, ""},
		{"failed", sse(`{"type":"response.failed","response":{"error":{"message":"boom"}}}`), OutcomeFailed, ""},
		{"json error body", []byte(`{"error":{"message":"403: blocked"}}`), OutcomeNonSSE, ""},
	}
	for _, tc := range cases {
		result := Analyze(tc.body, codexDeclared)
		if result.Outcome != tc.outcome || result.Text != tc.text {
			t.Errorf("%s: got %+v", tc.name, result)
		}
	}
	if Analyze(sse(`{"type":"response.failed","response":{"error":{"message":"boom"}}}`), nil).Error != "boom" {
		t.Error("failed error message lost")
	}
	// 没有声明信息时不判未知工具。
	if got := Analyze(fixture(t, "bps_excel_tools.sse"), nil); got.Outcome != OutcomeToolCall {
		t.Errorf("nil declared: %+v", got)
	}
	if !Abnormal(OutcomeCommentaryOnly) || !Abnormal(OutcomeUnknownTool) || Abnormal(OutcomeToolCall) || Abnormal(OutcomeText) {
		t.Error("Abnormal classification wrong")
	}
}

func TestDeclaredToolsTopLevel(t *testing.T) {
	declared := DeclaredTools(mustJSON(`{"tools":[{"type":"function","name":"shell"},{"type":"web_search"},{"type":"namespace","name":"mcp","tools":[{"type":"function","name":"x"}]}]}`))
	for _, name := range []string{"shell", "web_search", "mcp.x", "x"} {
		if !declared[name] {
			t.Errorf("%s not declared: %v", name, declared)
		}
	}
	if DeclaredTools(mustJSON(`{"input":"hi"}`)) != nil {
		t.Error("no tools must give nil")
	}
}

func newTestWriter(t *testing.T, bodies bool) *Writer {
	t.Helper()
	w, err := New(Options{Dir: t.TempDir(), Bodies: bodies})
	if err != nil {
		t.Fatal(err)
	}
	return w
}

func readLines(t *testing.T, w *Writer) []Entry {
	t.Helper()
	matches, _ := filepath.Glob(filepath.Join(w.Dir(), "requests-*.jsonl"))
	var entries []Entry
	for _, path := range matches {
		data, _ := os.ReadFile(path)
		for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
			if line == "" {
				continue
			}
			var e Entry
			if err := json.Unmarshal([]byte(line), &e); err != nil {
				t.Fatal(err)
			}
			entries = append(entries, e)
		}
	}
	return entries
}

func TestWriterWritesSummaryAndBodies(t *testing.T) {
	w := newTestWriter(t, true)
	w.Log(Record{Entry: Entry{RequestID: "req/../1", Route: "bps", Outcome: OutcomeCommentaryOnly}, Request: []byte(`{"a":1}`), Response: []byte("data: x\n\n"), Abnormal: true})
	w.Log(Record{Entry: Entry{RequestID: "req2", Route: "codex", Outcome: OutcomeText}})
	w.Close()
	entries := readLines(t, w)
	if len(entries) != 2 || !entries[0].Bodies || entries[1].Bodies || entries[0].Time == "" {
		t.Fatalf("entries=%+v", entries)
	}
	bodies, _ := filepath.Glob(filepath.Join(w.Dir(), "bodies", "*", "*"))
	if len(bodies) != 2 {
		t.Fatalf("bodies=%v", bodies)
	}
	for _, path := range bodies {
		if strings.Contains(filepath.Base(path), "/") || !strings.HasPrefix(filepath.Base(path), "req1") {
			t.Fatalf("unsafe body file name: %s", path)
		}
	}
	if w.Written.Load() != 2 || w.Errors.Load() != 0 || w.Abnormals.Load() != 1 {
		t.Fatalf("written=%d errors=%d abnormal=%d", w.Written.Load(), w.Errors.Load(), w.Abnormals.Load())
	}
}

func TestWriterKeepsOnlyRecentNormalBodies(t *testing.T) {
	w := newTestWriter(t, true)
	for i := 0; i < recentNormal+5; i++ {
		w.Log(Record{Entry: Entry{RequestID: "n" + string(rune('a'+i%26)) + strings.Repeat("x", i/26), Outcome: OutcomeText}, Request: []byte("{}"), Response: []byte("x")})
	}
	w.Log(Record{Entry: Entry{RequestID: "bad", Outcome: OutcomeUnknownTool}, Request: []byte("{}"), Response: []byte("x"), Abnormal: true})
	w.Close()
	requests, _ := filepath.Glob(filepath.Join(w.Dir(), "bodies", "*", "*.req.json"))
	if len(requests) != recentNormal+1 {
		t.Fatalf("kept %d request bodies, want %d", len(requests), recentNormal+1)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(requests[0]), "bad.req.json")); err != nil {
		t.Fatal("abnormal body must never be evicted")
	}
}

func TestWriterWithoutBodies(t *testing.T) {
	w := newTestWriter(t, false)
	w.Log(Record{Entry: Entry{RequestID: "r", Outcome: OutcomeUnknownTool}, Request: []byte("{}"), Abnormal: true})
	w.Close()
	if bodies, _ := filepath.Glob(filepath.Join(w.Dir(), "bodies", "*", "*")); len(bodies) != 0 {
		t.Fatalf("bodies must not be written: %v", bodies)
	}
}

func TestCleanupRetentionAndCap(t *testing.T) {
	dir := t.TempDir()
	w := &Writer{dir: dir, retention: time.Hour, maxBytes: 100, now: time.Now}
	old := filepath.Join(dir, "requests-old.jsonl")
	_ = os.WriteFile(old, []byte("x"), 0o600)
	past := time.Now().Add(-2 * time.Hour)
	_ = os.Chtimes(old, past, past)
	for i, name := range []string{"a", "b", "c"} {
		path := filepath.Join(dir, name)
		_ = os.WriteFile(path, make([]byte, 60), 0o600)
		stamp := time.Now().Add(time.Duration(i-10) * time.Minute)
		_ = os.Chtimes(path, stamp, stamp)
	}
	w.cleanup()
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Fatal("expired file must be removed")
	}
	left, _ := filepath.Glob(filepath.Join(dir, "*"))
	if len(left) != 1 || filepath.Base(left[0]) != "c" {
		t.Fatalf("cap must remove oldest first, left=%v", left)
	}
}

func TestNilWriterIsNoop(t *testing.T) {
	var w *Writer
	w.Log(Record{})
	w.Close()
	if w.Dir() != "" {
		t.Fatal("nil writer dir")
	}
}
