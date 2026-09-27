package transport

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jzg-lab/bps_sub_plugin/internal/config"
	"github.com/jzg-lab/bps_sub_plugin/internal/tracelog"
)

// tracedForwarder 返回一个开着排查日志的 Forwarder，以及读出日志的函数。
func tracedForwarder(t *testing.T, u *upstreams, mutate func(*config.Config)) (*Forwarder, func() []tracelog.Entry, string) {
	t.Helper()
	forwarder := u.forwarder(t, mutate)
	dir := t.TempDir()
	writer, err := tracelog.New(tracelog.Options{Dir: dir, Bodies: true})
	if err != nil {
		t.Fatal(err)
	}
	forwarder.SetTrace(writer)
	read := func() []tracelog.Entry {
		forwarder.SetTrace(nil)
		writer.Close()
		matches, _ := filepath.Glob(filepath.Join(dir, "requests-*.jsonl"))
		var entries []tracelog.Entry
		for _, path := range matches {
			data, _ := os.ReadFile(path)
			for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
				var entry tracelog.Entry
				if json.Unmarshal([]byte(line), &entry) == nil {
					entries = append(entries, entry)
				}
			}
		}
		return entries
	}
	return forwarder, read, dir
}

// 复现客户截图：模型只说"我现在去改"就结束了这一轮。要记成 commentary_only 并保存原文。
func TestTraceRecordsCommentaryOnlyWithBodies(t *testing.T) {
	commentary := map[string]any{"type": "message", "role": "assistant", "phase": "commentary", "content": []any{map[string]any{"type": "output_text", "text": "我现在直接写入修改。"}}}
	upstream := sse("response.created", map[string]any{"type": "response.created", "response": map[string]any{"id": "r"}}) +
		sse("response.completed", map[string]any{"type": "response.completed", "response": map[string]any{"id": "r", "output": []any{commentary}}})
	u := toolUpstreams(t, upstream)
	forwarder, read, dir := tracedForwarder(t, u, nil)
	stream := toolStreamWithID(additionalToolsBody, "req-1", 7)
	if err := forwarder.Forward(stream); err != nil {
		t.Fatal(err)
	}
	entries := read()
	if len(entries) != 1 {
		t.Fatalf("entries=%+v", entries)
	}
	entry := entries[0]
	if entry.Route != "bps" || entry.Outcome != tracelog.OutcomeCommentaryOnly || entry.Text != "我现在直接写入修改。" ||
		entry.RequestID != "req-1" || entry.AccountID != 7 || entry.Status != 200 || !entry.NativeTools || !entry.Bodies || entry.Model != "gpt-5.6-sol" {
		t.Fatalf("entry=%+v", entry)
	}
	request, err := os.ReadFile(filepath.Join(dir, "bodies", filepath.Base(mustGlob(t, filepath.Join(dir, "bodies", "*"))), "req-1.req.json"))
	if err != nil || !strings.Contains(string(request), `"model_selection":"explicit"`) || strings.Contains(string(request), "Bearer") {
		t.Fatalf("request body must be the rewritten basispoints body without tokens: %s err=%v", request, err)
	}
	if forwarder.Stats.Outcomes.Get(tracelog.OutcomeCommentaryOnly) != 1 {
		t.Fatalf("outcomes=%v", forwarder.Stats.Outcomes.Snapshot())
	}
}

// basispoints 模型调用 Codex 没声明的工具（Excel 工具）：记成 unknown_tool。
func TestTraceRecordsUnknownTool(t *testing.T) {
	call := map[string]any{"type": "function_call", "id": "fc_1", "call_id": "c1", "name": "read_sheets_metadata", "arguments": "{}"}
	upstream := sse("response.completed", map[string]any{"type": "response.completed", "response": map[string]any{"output": []any{call}}})
	forwarder, read, _ := tracedForwarder(t, toolUpstreams(t, upstream), nil)
	if err := forwarder.Forward(toolStream(additionalToolsBody)); err != nil {
		t.Fatal(err)
	}
	entry := read()[0]
	if entry.Outcome != tracelog.OutcomeUnknownTool || strings.Join(entry.UnknownTools, ",") != "read_sheets_metadata" {
		t.Fatalf("entry=%+v", entry)
	}
}

// 回落 codex 的请求：记下原因和 basispoints 的错误，且算异常（保存原文）。
func TestTraceRecordsFallback(t *testing.T) {
	u := newUpstreams(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(usagePolicyBody))
	})
	forwarder, read, _ := tracedForwarder(t, u, nil)
	run(t, forwarder, responsesStream(routableBody))
	entry := read()[0]
	if entry.Route != "codex" || entry.Reason != fallbackUsagePolicy || !strings.Contains(entry.UpstreamError, "usage policy") || !entry.Bodies {
		t.Fatalf("entry=%+v", entry)
	}
}

// 不走 bps 的请求也记一行（原因码），但正常结局不算异常。
func TestTraceRecordsSkip(t *testing.T) {
	u := newUpstreams(t, nil)
	forwarder, read, _ := tracedForwarder(t, u, nil)
	run(t, forwarder, responsesStream(`{"model":"gpt-5.5","input":"hi"}`))
	run(t, forwarder, responsesStream(routableBody))
	entries := read()
	if len(entries) != 2 || entries[0].Route != "codex" || entries[0].Reason != "model_not_allowed" || entries[1].Route != "bps" {
		t.Fatalf("entries=%+v", entries)
	}
}

func mustGlob(t *testing.T, pattern string) string {
	t.Helper()
	matches, _ := filepath.Glob(pattern)
	if len(matches) == 0 {
		t.Fatalf("no match for %s", pattern)
	}
	return matches[0]
}

// 直接发 codex 的请求只记摘要，不存原文。
func TestTraceSkipsBodiesForPlainCodex(t *testing.T) {
	u := newUpstreams(t, nil)
	forwarder, read, _ := tracedForwarder(t, u, nil)
	run(t, forwarder, responsesStream(`{"model":"gpt-5.5","input":"hi"}`))
	if entry := read()[0]; entry.Bodies || entry.Route != "codex" {
		t.Fatalf("entry=%+v", entry)
	}
}

// 用户第二轮只发了"继续"：摘要标 nudge，并把上一轮一起存进 incidents。
func TestTraceNudgeSavesIncident(t *testing.T) {
	commentary := map[string]any{"type": "message", "role": "assistant", "phase": "commentary", "content": []any{map[string]any{"type": "output_text", "text": "我先看看。"}}}
	upstream := sse("response.completed", map[string]any{"type": "response.completed", "response": map[string]any{"output": []any{commentary}}})
	forwarder, read, dir := tracedForwarder(t, toolUpstreams(t, upstream), nil)
	first := `{"model":"gpt-5.6-sol","stream":true,"prompt_cache_key":"sess-1234567890","input":[{"type":"message","role":"user","content":"改一下登录页"}]}`
	second := `{"model":"gpt-5.6-sol","stream":true,"prompt_cache_key":"sess-1234567890","input":[{"type":"message","role":"user","content":"改一下登录页"},{"type":"message","role":"assistant","phase":"commentary","content":[{"type":"output_text","text":"我先看看。"}]},{"type":"message","role":"user","content":"继续"}]}`
	for i, body := range []string{first, second} {
		if err := forwarder.Forward(toolStreamWithID(body, "r"+string(rune('1'+i)), 9)); err != nil {
			t.Fatal(err)
		}
	}
	entries := read()
	if len(entries) != 2 || entries[0].Nudge || !entries[1].Nudge || entries[1].Incident == "" || entries[0].Session != "sess-1234567890" {
		t.Fatalf("entries=%+v", entries)
	}
	for _, name := range []string{"01-r1.req.json", "01-r1.resp.sse", "02-r2.req.json", "02-r2.resp.sse", "summary.jsonl"} {
		if _, err := os.Stat(filepath.Join(dir, entries[1].Incident, name)); err != nil {
			t.Errorf("missing %s: %v", name, err)
		}
	}
}

// 宿主读到 completed 后主动关流（context canceled）：这一轮已经成功，不能记成 error（这是生产日志里 103 条误报的成因）。
func TestFinishTraceCompletedThenCancelIsNotError(t *testing.T) {
	msg := map[string]any{"type": "message", "role": "assistant", "phase": "final_answer", "content": []any{map[string]any{"type": "output_text", "text": "done"}}}
	body := []byte(sse("response.completed", map[string]any{"type": "response.completed", "response": map[string]any{"output": []any{msg}}}))
	dir := t.TempDir()
	writer, err := tracelog.New(tracelog.Options{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	f := &Forwarder{Stats: &Stats{}}
	f.SetTrace(writer)
	recorder := &recordingStream{status: 200, body: body}
	recorder.info.route = "bps"
	// forwardErr = context.Canceled, cancelled = true：宿主收到 completed 后关流。
	f.finishTrace(recorder, "r1", 1, context.Canceled, true)
	writer.Close()
	entry := readOne(t, dir)
	if entry.Outcome != tracelog.OutcomeText || entry.Error != "" {
		t.Fatalf("completed-then-cancel must stay text: %+v", entry)
	}
	// 对照：还没出结局就被取消 → cancelled（也不算 error）。
	f2 := &Forwarder{Stats: &Stats{}}
	dir2 := t.TempDir()
	w2, _ := tracelog.New(tracelog.Options{Dir: dir2})
	f2.SetTrace(w2)
	rec2 := &recordingStream{status: 200, body: []byte(sse("response.created", map[string]any{"type": "response.created"}) + sse("response.output_text.delta", map[string]any{"type": "response.output_text.delta", "delta": "x"}))}
	rec2.info.route = "bps"
	f2.finishTrace(rec2, "r2", 1, context.Canceled, true)
	w2.Close()
	if entry := readOne(t, dir2); entry.Outcome != tracelog.OutcomeCancelled {
		t.Fatalf("cut-off cancel must be cancelled: %+v", entry)
	}
}

func readOne(t *testing.T, dir string) tracelog.Entry {
	t.Helper()
	matches, _ := filepath.Glob(filepath.Join(dir, "requests-*.jsonl"))
	for _, path := range matches {
		data, _ := os.ReadFile(path)
		for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
			var entry tracelog.Entry
			if json.Unmarshal([]byte(line), &entry) == nil {
				return entry
			}
		}
	}
	t.Fatal("no entry")
	return tracelog.Entry{}
}
