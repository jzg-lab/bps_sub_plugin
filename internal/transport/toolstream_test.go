package transport

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	pluginv1 "github.com/jzg-lab/bps_sub_plugin/internal/pluginapi/v1"
)

// bpsToolBody 是一个带工具的可路由请求体。
const bpsToolBody = `{"model":"gpt-5.6-sol","stream":true,"input":[{"type":"message","role":"user","content":"run pwd"}],"tools":[{"type":"function","name":"exec_command","parameters":{"type":"object","properties":{"cmd":{"type":"string"}},"required":["cmd"]}},{"type":"custom","name":"apply_patch"}]}`

// runOfficejsSSE 合成一段"模型经 run_officejs 调用 exec_command"的上游 SSE 流。
func runOfficejsSSE(code string) string {
	fc := map[string]any{
		"type": "function_call", "id": "fc_up1", "call_id": "call_abc", "name": "run_officejs", "status": "completed",
		"arguments": `{"summary":"s","code":` + jsonString(code) + `,"destructive":false,"references":[]}`,
	}
	blocks := []string{
		sse("response.created", map[string]any{"type": "response.created", "response": map[string]any{"id": "resp_1", "output": []any{}}}),
		sse("response.output_item.added", map[string]any{"type": "response.output_item.added", "output_index": 0, "item": map[string]any{"type": "message", "role": "assistant", "phase": "commentary", "content": []any{}}}),
		sse("response.output_text.delta", map[string]any{"type": "response.output_text.delta", "output_index": 0, "item_id": "msg1", "delta": "I'll run pwd."}),
		sse("response.output_item.done", map[string]any{"type": "response.output_item.done", "output_index": 0, "item": map[string]any{"type": "message", "role": "assistant", "phase": "commentary", "content": []any{map[string]any{"type": "output_text", "text": "I'll run pwd."}}}}),
		sse("response.output_item.added", map[string]any{"type": "response.output_item.added", "output_index": 1, "item": fc}),
		sse("response.function_call_arguments.delta", map[string]any{"type": "response.function_call_arguments.delta", "output_index": 1, "item_id": "fc_up1", "delta": "{...}"}),
		sse("response.function_call_arguments.done", map[string]any{"type": "response.function_call_arguments.done", "output_index": 1, "item_id": "fc_up1", "arguments": fc["arguments"]}),
		sse("response.output_item.done", map[string]any{"type": "response.output_item.done", "output_index": 1, "item": fc}),
		sse("response.completed", map[string]any{"type": "response.completed", "response": map[string]any{"id": "resp_1", "status": "completed", "output": []any{
			map[string]any{"type": "message", "role": "assistant", "phase": "commentary", "content": []any{map[string]any{"type": "output_text", "text": "I'll run pwd."}}},
			fc,
		}}}),
	}
	return strings.Join(blocks, "")
}

func sse(event string, payload map[string]any) string {
	data, _ := json.Marshal(payload)
	return "event: " + event + "\ndata: " + string(data) + "\n\n"
}

func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// toolUpstreams starts a fake bps that returns the given SSE and a fake codex.
func toolUpstreams(t *testing.T, sseBody string) *upstreams {
	return newUpstreams(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
		_, _ = w.Write([]byte(sseBody))
	})
}

func toolStream(body string) *fakeStream {
	return newFakeStream(context.Background(),
		startFrame(&pluginv1.ForwardRequestStart{
			Method: http.MethodPost, Url: codexURL, Host: "chatgpt.com",
			Headers: map[string]*pluginv1.HeaderValues{
				"Authorization": {Values: []string{"Bearer tok"}}, "Chatgpt-Account-Id": {Values: []string{"acct"}},
			},
			HasBody: true, ContentLength: int64(len(body)),
		}),
		chunkFrame(body), endFrame(),
	)
}

// parseEvents splits the relayed body into (event, payload) pairs.
func parseEvents(t *testing.T, body string) []struct {
	Event   string
	Payload map[string]any
} {
	t.Helper()
	var out []struct {
		Event   string
		Payload map[string]any
	}
	for _, block := range strings.Split(body, "\n\n") {
		block = strings.TrimSpace(block)
		if block == "" {
			continue
		}
		var event, data string
		for _, line := range strings.Split(block, "\n") {
			if rest, ok := strings.CutPrefix(line, "event: "); ok {
				event = rest
			} else if rest, ok := strings.CutPrefix(line, "data: "); ok {
				data = rest
			}
		}
		if data == "" || data == "[DONE]" {
			continue
		}
		var payload map[string]any
		if json.Unmarshal([]byte(data), &payload) != nil {
			continue
		}
		out = append(out, struct {
			Event   string
			Payload map[string]any
		}{event, payload})
	}
	return out
}

type memStore struct {
	m map[string]map[string]any
}

func newMemStore() *memStore { return &memStore{m: map[string]map[string]any{}} }
func (s *memStore) RememberNativeCall(callID string, native map[string]any) {
	s.m[callID] = native
}
func (s *memStore) RememberedNativeCall(callID string) (map[string]any, bool) {
	v, ok := s.m[callID]
	return v, ok
}

func toolForwarder(t *testing.T, u *upstreams, store ToolStore) *Forwarder {
	f := u.forwarder(t, nil)
	f.toolStore = store
	f.replayer = store
	return f
}

func TestToolStreamRestoresFunctionCall(t *testing.T) {
	u := toolUpstreams(t, runOfficejsSSE(`{"name":"exec_command","arguments":{"cmd":"pwd"}}`))
	store := newMemStore()
	forwarder := toolForwarder(t, u, store)
	stream := toolStream(bpsToolBody)
	if err := forwarder.Forward(stream); err != nil {
		t.Fatal(err)
	}
	start, body, _, frameErr := collect(t, stream.frames())
	if frameErr != nil || start.StatusCode != 200 {
		t.Fatalf("start=%+v err=%+v", start, frameErr)
	}
	events := parseEvents(t, body)

	// run_officejs must never reach the host; the restored exec_command must.
	var fcAdded, completed map[string]any
	for _, e := range events {
		if strings.Contains(body, "run_officejs") {
			t.Fatal("run_officejs leaked to host")
		}
		if e.Event == "response.output_item.added" {
			if item, ok := e.Payload["item"].(map[string]any); ok && item["type"] == "function_call" {
				fcAdded = item
			}
		}
		if e.Event == "response.completed" {
			completed = e.Payload
		}
	}
	if fcAdded == nil || fcAdded["name"] != "exec_command" || fcAdded["call_id"] != "call_abc" {
		t.Fatalf("restored function_call event missing/wrong: %v", fcAdded)
	}
	// commentary text must still be delivered
	if !strings.Contains(body, "I'll run pwd.") {
		t.Fatal("assistant commentary lost")
	}
	// completed output has the restored call, not run_officejs
	output := completed["response"].(map[string]any)["output"].([]any)
	last := output[len(output)-1].(map[string]any)
	if last["name"] != "exec_command" {
		t.Fatalf("completed output not rewritten: %v", last)
	}
	// arguments.done must carry the real arguments
	var argsDone map[string]any
	for _, e := range events {
		if e.Event == "response.function_call_arguments.done" {
			argsDone = e.Payload
		}
	}
	if argsDone == nil || !strings.Contains(argsDone["arguments"].(string), "pwd") {
		t.Fatalf("arguments.done wrong: %v", argsDone)
	}
	// stored for replay
	if _, ok := store.RememberedNativeCall("call_abc"); !ok {
		t.Fatal("native call not stored for replay")
	}
	if forwarder.Stats.ToolRelayed.Load() != 1 || forwarder.Stats.RoutedBPS.Load() != 1 {
		t.Fatalf("stats relayed=%d bps=%d", forwarder.Stats.ToolRelayed.Load(), forwarder.Stats.RoutedBPS.Load())
	}
}

func TestToolStreamNonToolResponsePassesThrough(t *testing.T) {
	plain := sse("response.created", map[string]any{"type": "response.created", "response": map[string]any{"id": "r"}}) +
		sse("response.output_text.delta", map[string]any{"type": "response.output_text.delta", "delta": "hello"}) +
		sse("response.completed", map[string]any{"type": "response.completed", "response": map[string]any{"id": "r", "status": "completed", "output": []any{map[string]any{"type": "message", "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": "hello"}}}}}})
	u := toolUpstreams(t, plain)
	forwarder := toolForwarder(t, u, newMemStore())
	stream := toolStream(bpsToolBody)
	if err := forwarder.Forward(stream); err != nil {
		t.Fatal(err)
	}
	_, body, _, frameErr := collect(t, stream.frames())
	if frameErr != nil || !strings.Contains(body, "hello") {
		t.Fatalf("plain response mangled: body=%q err=%+v", body, frameErr)
	}
	if forwarder.Stats.ToolRelayed.Load() != 0 {
		t.Fatal("no tool calls should be counted")
	}
}

func TestToolStreamParallelCalls(t *testing.T) {
	fc1 := map[string]any{"type": "function_call", "id": "fc1", "call_id": "c1", "name": "run_officejs", "status": "completed", "arguments": `{"code":"{\"name\":\"exec_command\",\"arguments\":{\"cmd\":\"pwd\"}}"}`}
	fc2 := map[string]any{"type": "function_call", "id": "fc2", "call_id": "c2", "name": "run_officejs", "status": "completed", "arguments": `{"code":"{\"name\":\"exec_command\",\"arguments\":{\"cmd\":\"ls\"}}"}`}
	body := sse("response.created", map[string]any{"type": "response.created", "response": map[string]any{"id": "r"}}) +
		sse("response.completed", map[string]any{"type": "response.completed", "response": map[string]any{"id": "r", "status": "completed", "output": []any{fc1, fc2}}})
	u := toolUpstreams(t, body)
	forwarder := toolForwarder(t, u, newMemStore())
	stream := toolStream(bpsToolBody)
	if err := forwarder.Forward(stream); err != nil {
		t.Fatal(err)
	}
	_, relayed, _, _ := collect(t, stream.frames())
	events := parseEvents(t, relayed)
	names := []string{}
	for _, e := range events {
		if e.Event == "response.function_call_arguments.done" {
			var args map[string]any
			_ = json.Unmarshal([]byte(e.Payload["arguments"].(string)), &args)
			names = append(names, args["cmd"].(string))
		}
	}
	if len(names) != 2 || names[0] != "pwd" || names[1] != "ls" {
		t.Fatalf("parallel calls = %v", names)
	}
	if forwarder.Stats.ToolRelayed.Load() != 2 {
		t.Fatalf("relayed = %d", forwarder.Stats.ToolRelayed.Load())
	}
}

func TestToolStreamCutOffCompletesFromItems(t *testing.T) {
	fc := map[string]any{"type": "function_call", "id": "fc1", "call_id": "c1", "name": "run_officejs", "status": "completed", "arguments": `{"code":"{\"name\":\"exec_command\",\"arguments\":{\"cmd\":\"pwd\"}}"}`}
	// stream ends after the function_call item.done, with NO response.completed
	body := sse("response.created", map[string]any{"type": "response.created", "response": map[string]any{"id": "r"}}) +
		sse("response.output_item.added", map[string]any{"type": "response.output_item.added", "output_index": 0, "item": fc}) +
		sse("response.output_item.done", map[string]any{"type": "response.output_item.done", "output_index": 0, "item": fc})
	u := toolUpstreams(t, body)
	forwarder := toolForwarder(t, u, newMemStore())
	stream := toolStream(bpsToolBody)
	if err := forwarder.Forward(stream); err != nil {
		t.Fatal(err)
	}
	_, relayed, _, frameErr := collect(t, stream.frames())
	if frameErr != nil {
		t.Fatalf("unexpected error frame: %+v", frameErr)
	}
	if !strings.Contains(relayed, "response.completed") || !strings.Contains(relayed, "exec_command") {
		t.Fatalf("cut-off stream not completed from items: %q", relayed)
	}
}

func TestToolStreamErrorResponseFallsBackToCodex(t *testing.T) {
	// bps returns 422 for a tool request -> fallback to codex (config default)
	u := newUpstreams(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = w.Write([]byte(`{"error":{"message":"422: Invalid request body."}}`))
	})
	forwarder := toolForwarder(t, u, newMemStore())
	stream := toolStream(bpsToolBody)
	if err := forwarder.Forward(stream); err != nil {
		t.Fatal(err)
	}
	_, body, _, _ := collect(t, stream.frames())
	if body != "data: codex\n\n" || u.codexHits.Load() != 1 {
		t.Fatalf("expected codex fallback for tool request: body=%q codex=%d", body, u.codexHits.Load())
	}
}
