package tracelog

import (
	"encoding/json"
	"fmt"
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

func TestCleanupCapKeepsSummaryOverBodies(t *testing.T) {
	dir := t.TempDir()
	w := &Writer{dir: dir, retention: time.Hour, maxBytes: 100, now: time.Now}
	summary := filepath.Join(dir, "requests-20260101.jsonl")
	_ = os.WriteFile(summary, make([]byte, 30), 0o600)
	oldest := time.Now().Add(-30 * time.Minute)
	_ = os.Chtimes(summary, oldest, oldest)
	_ = os.MkdirAll(filepath.Join(dir, "bodies", "20260101"), 0o700)
	var bodies []string
	for i := 0; i < 3; i++ {
		path := filepath.Join(dir, "bodies", "20260101", fmt.Sprintf("r%d.resp.sse", i))
		_ = os.WriteFile(path, make([]byte, 40), 0o600)
		stamp := time.Now().Add(time.Duration(i-10) * time.Minute)
		_ = os.Chtimes(path, stamp, stamp)
		bodies = append(bodies, path)
	}
	w.cleanup()
	if _, err := os.Stat(summary); err != nil {
		t.Fatal("summary must survive while bodies can be removed")
	}
	if _, err := os.Stat(bodies[0]); !os.IsNotExist(err) {
		t.Fatal("oldest body must be removed first")
	}
	if _, err := os.Stat(bodies[2]); err != nil {
		t.Fatal("newest body must be kept once under the cap")
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

func TestIsNudge(t *testing.T) {
	yes := []string{"继续", "继续。", " 继续！", "？？？？", "???", "……", "Continue", "go on", "继续吧", "怎么不动了？", "继续做完"}
	no := []string{"", "继续把登录页的样式改成蓝色，然后加一个记住密码的选项", "帮我看看这个报错", "continue the refactor of the payment module and add tests"}
	for _, text := range yes {
		if !IsNudge(text) {
			t.Errorf("IsNudge(%q) = false", text)
		}
	}
	for _, text := range no {
		if IsNudge(text) {
			t.Errorf("IsNudge(%q) = true", text)
		}
	}
}

func TestLastUserText(t *testing.T) {
	body := mustJSON(`{"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"改代码"}]},
		{"type":"message","role":"assistant","content":[{"type":"output_text","text":"好"}]},
		{"type":"message","role":"user","content":[{"type":"input_text","text":"继续"}]}]}`)
	if got := LastUserText(body); got != "继续" {
		t.Fatalf("got %q", got)
	}
	// 最后一条 user 之后有工具结果：这是模型自己的后续轮次，不是用户刚说的。
	followUp := mustJSON(`{"input":[{"type":"message","role":"user","content":"继续"},{"type":"custom_tool_call","call_id":"c"},{"type":"custom_tool_call_output","call_id":"c","output":"x"}]}`)
	if got := LastUserText(followUp); got != "" {
		t.Fatalf("tool follow-up must not count as user text, got %q", got)
	}
	if got := LastUserText(mustJSON(`{"input":"继续"}`)); got != "继续" {
		t.Fatalf("string input: %q", got)
	}
}

// 用户发"继续"时，把这个会话前几轮加本轮一起存到 incidents 目录。
func TestNudgeWritesIncidentWithContext(t *testing.T) {
	w := newTestWriter(t, true)
	for i := 1; i <= historyTurns+2; i++ {
		id := "t" + string(rune('0'+i))
		w.Log(Record{Entry: Entry{RequestID: id, Session: "sess-abcdef123", Route: "bps", Outcome: OutcomeText}, Request: []byte(`{"turn":"` + id + `"}`), Response: []byte("data: " + id)})
	}
	w.Log(Record{Entry: Entry{RequestID: "other", Session: "sess-other", Route: "bps", Outcome: OutcomeText}, Request: []byte("{}"), Response: []byte("x")})
	w.Log(Record{Entry: Entry{RequestID: "codexturn", Session: "sess-abcdef123", Route: "codex", Outcome: OutcomeText}})
	w.Log(Record{Entry: Entry{RequestID: "nudge", Session: "sess-abcdef123", Route: "bps", Outcome: OutcomeCommentaryOnly, Nudge: true}, Request: []byte(`{"turn":"nudge"}`), Response: []byte("data: n"), Abnormal: true})
	w.Close()

	var nudge Entry
	for _, entry := range readLines(t, w) {
		if entry.RequestID == "nudge" {
			nudge = entry
		}
	}
	if !nudge.Nudge || nudge.Incident == "" || !strings.HasPrefix(nudge.Incident, "incidents/") {
		t.Fatalf("nudge entry=%+v", nudge)
	}
	dir := filepath.Join(w.Dir(), nudge.Incident)
	summary, err := os.ReadFile(filepath.Join(dir, "summary.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(summary)), "\n")
	if len(lines) != historyTurns+1 || !strings.Contains(lines[len(lines)-1], `"request_id":"nudge"`) || !strings.Contains(lines[len(lines)-2], "codexturn") {
		t.Fatalf("summary must hold the last %d turns plus the nudge, in order:\n%s", historyTurns, summary)
	}
	if strings.Contains(string(summary), `"other"`) || strings.Contains(string(summary), `"t1"`) {
		t.Fatalf("summary must only contain this session's latest turns:\n%s", summary)
	}
	files, _ := filepath.Glob(filepath.Join(dir, "*.req.json"))
	if len(files) != historyTurns { // 5 轮里有一轮是直接走 codex 的（没有原文），加上本轮 = 5 个请求文件
		t.Fatalf("request files=%v", files)
	}
	last, _ := os.ReadFile(filepath.Join(dir, "06-nudge.req.json"))
	if string(last) != `{"turn":"nudge"}` {
		t.Fatalf("nudge turn body=%q", last)
	}
	if w.Incidents.Load() != 1 {
		t.Fatalf("incidents=%d", w.Incidents.Load())
	}
}

func TestHistoryEvictsIdleAndOldestSessions(t *testing.T) {
	now := time.Now()
	w := &Writer{history: map[string]*sessionHistory{}, now: func() time.Time { return now }}
	for i := 0; i < historySessions+3; i++ {
		w.remember(Record{Entry: Entry{Session: "s" + strings.Repeat("x", i)}}, now.Add(time.Duration(i)*time.Second))
	}
	if len(w.history) != historySessions {
		t.Fatalf("sessions=%d", len(w.history))
	}
	if _, ok := w.history["s"]; ok {
		t.Fatal("oldest session must be evicted")
	}
	now = now.Add(historyIdle + time.Hour)
	w.expireHistory()
	if len(w.history) != 0 {
		t.Fatalf("idle sessions must expire, left %d", len(w.history))
	}
}
