package transport

import (
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
