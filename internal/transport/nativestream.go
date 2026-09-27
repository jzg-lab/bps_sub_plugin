package transport

import (
	"bufio"
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/jzg-lab/bps_sub_plugin/internal/basispoints"
	pluginv1 "github.com/jzg-lab/bps_sub_plugin/internal/pluginapi/v1"
)

// planToolName 是 basispoints 和 Codex 同名、但参数格式不同的计划工具。
const planToolName = "update_plan"

// relayNativeStream 处理原生工具（additional_tools）请求的 SSE 响应：其它事件逐字透传，
// 只把 basispoints 自带 update_plan 的参数（summary/description/id/result）改写成 Codex 的格式
// （explanation/step），否则 Codex 报 unknown field summary、计划丢失。
func (f *Forwarder) relayNativeStream(stream Stream, response *http.Response, started time.Time) error {
	if response.StatusCode/100 != 2 || !strings.Contains(strings.ToLower(response.Header.Get("Content-Type")), "text/event-stream") {
		return f.relay(stream, response, started)
	}
	defer func() { _ = response.Body.Close() }()
	if err := stream.Send(startFrameOf(response)); err != nil {
		return err
	}

	transform := &planTransform{held: map[string]bool{}}
	scanner := bufio.NewScanner(response.Body)
	scanner.Buffer(make([]byte, 64*1024), 16*1024*1024)
	scanner.Split(splitSSEBlocks)
	var received int64
	for scanner.Scan() {
		raw := scanner.Bytes()
		received += int64(len(raw)) + 2
		out := transform.block(raw)
		if len(out) == 0 {
			continue
		}
		if err := stream.Send(&pluginv1.ForwardResponse{Frame: &pluginv1.ForwardResponse_BodyChunk{BodyChunk: out}}); err != nil {
			return err
		}
	}
	if err := scanner.Err(); err != nil {
		return f.fail(stream, codeUpstreamBody, err.Error(), true)
	}
	return stream.Send(&pluginv1.ForwardResponse{Frame: &pluginv1.ForwardResponse_End{End: &pluginv1.ForwardResponseEnd{
		BytesReceived: received,
		DurationMs:    time.Since(started).Milliseconds(),
	}}})
}

// planTransform 记住哪些 item 是 update_plan 调用，扣住它们的参数增量，在 done 时一次性发出改写后的参数。
type planTransform struct {
	held map[string]bool // item id → 是 update_plan
}

func (t *planTransform) block(raw []byte) []byte {
	passthrough := append(append([]byte(nil), raw...), '\n', '\n')
	if !bytes.Contains(raw, []byte(planToolName)) && !(len(t.held) > 0 && bytes.Contains(raw, []byte("function_call_arguments"))) {
		return passthrough
	}
	eventName, data := parseSSEBlock(raw)
	var payload map[string]any
	if json.Unmarshal([]byte(data), &payload) != nil || payload == nil {
		return passthrough
	}
	eventType := eventName
	if eventType == "" {
		eventType, _ = payload["type"].(string)
	}

	switch eventType {
	case "response.output_item.added":
		if item, ok := payload["item"].(map[string]any); ok && isPlanCall(item) {
			if id := stringField(item, "id"); id != "" {
				t.held[id] = true
			}
		}
		return passthrough
	case "response.function_call_arguments.delta":
		if t.held[stringField(payload, "item_id")] {
			return nil // 原生格式的片段不能发，done 时补一条完整的
		}
		return passthrough
	case "response.function_call_arguments.done":
		id := stringField(payload, "item_id")
		if !t.held[id] {
			return passthrough
		}
		arguments := basispoints.ClientPlanArgumentsJSON(stringField(payload, "arguments"))
		payload["arguments"] = arguments
		delta := map[string]any{"type": "response.function_call_arguments.delta", "item_id": id, "delta": arguments}
		if index, ok := payload["output_index"]; ok {
			delta["output_index"] = index
		}
		return append(sseEncode("response.function_call_arguments.delta", delta), sseEncode(eventType, payload)...)
	case "response.output_item.done":
		if item, ok := payload["item"].(map[string]any); ok && isPlanCall(item) {
			item["arguments"] = basispoints.ClientPlanArgumentsJSON(stringField(item, "arguments"))
			delete(t.held, stringField(item, "id"))
			return sseEncode(eventType, payload)
		}
		return passthrough
	case "response.completed", "response.incomplete", "response.failed":
		resp, _ := payload["response"].(map[string]any)
		output, _ := resp["output"].([]any)
		changed := false
		for _, raw := range output {
			if item, ok := raw.(map[string]any); ok && isPlanCall(item) {
				item["arguments"] = basispoints.ClientPlanArgumentsJSON(stringField(item, "arguments"))
				changed = true
			}
		}
		if changed {
			return sseEncode(eventType, payload)
		}
		return passthrough
	}
	return passthrough
}

func isPlanCall(item map[string]any) bool {
	return item["type"] == "function_call" && item["name"] == planToolName
}
