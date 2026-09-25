package transport

import (
	"bufio"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jzg-lab/bps_sub_plugin/internal/basispoints"
	pluginv1 "github.com/jzg-lab/bps_sub_plugin/internal/pluginapi/v1"
)

var terminalEvents = map[string]bool{
	"response.completed":  true,
	"response.failed":     true,
	"response.incomplete": true,
}

// heldEventTypes 是必须扣留、不能直接发给宿主的原生工具事件：它们的参数是上游的
// server-tool schema，Codex 直接执行会失败并触发重试循环。到 completed 时统一还原。
var heldEventTypes = map[string]bool{
	"response.function_call_arguments.delta": true,
	"response.function_call_arguments.done":  true,
	"response.custom_tool_call_input.delta":  true,
	"response.custom_tool_call_input.done":   true,
}

// relayToolStream 处理带工具的 basispoints 流式响应：把 run_officejs 原生调用实时还原成
// 客户端声明的 function_call / custom_tool_call 再发给宿主，并把原生调用存进 KV 供下轮回放。
func (f *Forwarder) relayToolStream(stream Stream, response *http.Response, started time.Time, decision basispoints.Decision) error {
	defer func() { _ = response.Body.Close() }()

	// basispoints 只在 2xx 的 SSE 流里带工具事件；非流式或错误响应原样回传。
	if response.StatusCode/100 != 2 || !strings.Contains(strings.ToLower(response.Header.Get("Content-Type")), "text/event-stream") {
		return f.relay(stream, response, started)
	}
	if err := stream.Send(startFrameOf(response)); err != nil {
		return err
	}
	send := func(data []byte) error {
		return stream.Send(&pluginv1.ForwardResponse{Frame: &pluginv1.ForwardResponse_BodyChunk{BodyChunk: data}})
	}

	transform := newToolStreamTransform(decision, f.Stats, f.toolStore)
	scanner := bufio.NewScanner(response.Body)
	scanner.Buffer(make([]byte, 64*1024), 16*1024*1024)
	scanner.Split(splitSSEBlocks)

	var blockErr error
	for scanner.Scan() {
		out, err := transform.block(scanner.Bytes())
		if err != nil {
			blockErr = err
			break
		}
		if len(out) > 0 {
			if err := send(out); err != nil {
				return err
			}
		}
	}
	if blockErr == nil {
		blockErr = scanner.Err()
	}
	// 收尾：上游在 completed 之前断流时，用已完成项补一个 completed。
	tail := transform.finish(blockErr != nil)
	if len(tail) > 0 {
		if err := send(tail); err != nil {
			return err
		}
	}
	if blockErr != nil {
		// 已经发过响应头，宿主不能重放。
		return f.fail(stream, codeUpstreamBody, blockErr.Error(), true)
	}
	return stream.Send(&pluginv1.ForwardResponse{Frame: &pluginv1.ForwardResponse_End{End: &pluginv1.ForwardResponseEnd{
		BytesReceived: transform.bytesIn,
		DurationMs:    time.Since(started).Milliseconds(),
	}}})
}

func startFrameOf(response *http.Response) *pluginv1.ForwardResponse {
	return &pluginv1.ForwardResponse{Frame: &pluginv1.ForwardResponse_Start{Start: &pluginv1.ForwardResponseStart{
		StatusCode:    int32(response.StatusCode),
		Status:        response.Status,
		Protocol:      response.Proto,
		ProtocolMajor: int32(response.ProtoMajor),
		ProtocolMinor: int32(response.ProtoMinor),
		Headers:       headersToPlugin(response.Header),
		ContentLength: -1, // 改写后长度变了，用未知长度。
	}}}
}

// toolStreamTransform 是 SSE 变换的状态机。参考 excel-codex-bridge 的 excel_stream.py。
type toolStreamTransform struct {
	decision basispoints.Decision
	stats    *Stats
	store    ToolStore

	bytesIn       int64
	finished      bool
	startedResp   map[string]any
	finishedItems []indexedItem
	itemsAdded    int
}

type indexedItem struct {
	index int
	item  map[string]any
}

func newToolStreamTransform(decision basispoints.Decision, stats *Stats, store ToolStore) *toolStreamTransform {
	return &toolStreamTransform{decision: decision, stats: stats, store: store}
}

// block 处理一个 SSE 事件块，返回要发给宿主的字节（可能为空）。
func (t *toolStreamTransform) block(raw []byte) ([]byte, error) {
	t.bytesIn += int64(len(raw))
	eventName, data := parseSSEBlock(raw)
	if data == "" || data == "[DONE]" {
		if data == "[DONE]" {
			return []byte("data: [DONE]\n\n"), nil
		}
		return nil, nil
	}
	var payload map[string]any
	if json.Unmarshal([]byte(data), &payload) != nil || payload == nil {
		return nil, nil
	}
	eventType := eventName
	if eventType == "" {
		eventType, _ = payload["type"].(string)
	}
	eventType = strings.ToLower(strings.TrimSpace(eventType))

	switch {
	case eventType == "response.created" || eventType == "response.in_progress":
		if resp, ok := payload["response"].(map[string]any); ok && t.startedResp == nil {
			t.startedResp = resp
		}
		return sseEncode(eventType, payload), nil

	case eventType == "response.output_item.added":
		t.itemsAdded++
		if isToolItemEvent(payload) {
			return nil, nil // 扣留原生工具项
		}
		return sseEncode(eventType, payload), nil

	case eventType == "response.output_item.done":
		if item, ok := payload["item"].(map[string]any); ok {
			t.finishedItems = append(t.finishedItems, indexedItem{index: intField(payload, "output_index", len(t.finishedItems)), item: item})
		}
		if isToolItemEvent(payload) {
			return nil, nil // 扣留
		}
		return sseEncode(eventType, payload), nil

	case heldEventTypes[eventType]:
		return nil, nil // 扣留原生工具参数事件

	case terminalEvents[eventType]:
		return t.completeBlock(eventType, payload), nil

	default:
		// 文本、reasoning、content_part 等原样透传。
		return sseEncode(eventType, payload), nil
	}
}

// completeBlock 在终态事件时把 run_officejs 调用还原成客户端工具调用并合成事件。
func (t *toolStreamTransform) completeBlock(eventType string, payload map[string]any) []byte {
	t.finished = true
	resp, _ := payload["response"].(map[string]any)

	var calls []basispoints.ClientToolCall
	if eventType == "response.completed" && resp != nil {
		calls = t.extractCalls(resp)
	}
	if len(calls) == 0 {
		return sseEncode(eventType, payload) // 不是工具调用，原样发终态事件。
	}

	// 有工具调用：合成还原后的调用事件，再发改写过的 completed。
	rebuilt := basispoints.ResponseWithToolCalls(resp, calls, t.decision.Model)
	positions := map[string]int{}
	if output, ok := rebuilt["output"].([]any); ok {
		for i, item := range output {
			if m, ok := item.(map[string]any); ok {
				positions[stringField(m, "call_id")] = i
			}
		}
	}
	var out []byte
	for _, call := range calls {
		if t.store != nil && call.Native != nil {
			t.store.RememberNativeCall(call.CallID, call.Native)
		}
		t.stats.ToolRelayed.Add(1)
		out = append(out, toolCallEvents(call, positions[call.CallID])...)
	}
	out = append(out, sseEncode("response.completed", map[string]any{"type": "response.completed", "response": rebuilt})...)
	return out
}

// extractCalls 从 completed 的 output 里提取并还原客户端工具调用。
func (t *toolStreamTransform) extractCalls(resp map[string]any) []basispoints.ClientToolCall {
	output, ok := resp["output"].([]any)
	if !ok {
		return nil
	}
	parallel := t.decision.Body["parallel_tool_calls"] != false
	var calls []basispoints.ClientToolCall
	for _, raw := range output {
		item, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		native, ok := basispoints.ParseNativeCall(item)
		if !ok {
			continue
		}
		call, ok := basispoints.RestoreClientCall(native, t.decision.Catalog)
		if !ok {
			if basispoints.IsTransportItem(item) {
				t.stats.ToolDecodeFailed.Add(1)
			}
			continue
		}
		calls = append(calls, call)
		if !parallel {
			break
		}
	}
	return calls
}

// finish 在流结束时收尾：上游在 completed 之前断流且已有完成项，就补一个 completed。
func (t *toolStreamTransform) finish(cutOff bool) []byte {
	if t.finished || len(t.finishedItems) == 0 || len(t.finishedItems) < t.itemsAdded {
		return nil
	}
	output := make([]any, 0, len(t.finishedItems))
	for _, entry := range sortItems(t.finishedItems) {
		output = append(output, entry)
	}
	if last, ok := output[len(output)-1].(map[string]any); ok {
		if last["type"] == "message" && last["phase"] == "commentary" {
			return nil // 停在 commentary，说明是 tool 调用前被截断，交给 Codex 重试。
		}
	}
	resp := map[string]any{}
	for k, v := range t.startedResp {
		resp[k] = v
	}
	resp["status"] = "completed"
	resp["output"] = output
	payload := map[string]any{"type": "response.completed", "response": resp}
	return t.completeBlock("response.completed", payload)
}

func sortItems(items []indexedItem) []map[string]any {
	sorted := append([]indexedItem(nil), items...)
	for i := 1; i < len(sorted); i++ {
		for j := i; j > 0 && sorted[j-1].index > sorted[j].index; j-- {
			sorted[j-1], sorted[j] = sorted[j], sorted[j-1]
		}
	}
	out := make([]map[string]any, len(sorted))
	for i, entry := range sorted {
		out[i] = entry.item
	}
	return out
}

// toolCallEvents 把一个还原后的调用合成标准 SSE 事件序列（added → delta → done → item.done）。
func toolCallEvents(call basispoints.ClientToolCall, outputIndex int) []byte {
	item := map[string]any{"type": call.Type, "id": call.ID, "call_id": call.CallID, "name": call.Name, "status": "in_progress"}
	if call.Namespace != "" {
		item["namespace"] = call.Namespace
	}
	var valueKey, value, deltaEvent, doneEvent string
	if call.Type == "custom_tool_call" {
		valueKey, value = "input", call.Input
		deltaEvent, doneEvent = "response.custom_tool_call_input.delta", "response.custom_tool_call_input.done"
	} else {
		valueKey, value = "arguments", call.Arguments
		deltaEvent, doneEvent = "response.function_call_arguments.delta", "response.function_call_arguments.done"
	}
	item[valueKey] = ""

	completed := map[string]any{"type": call.Type, "id": call.ID, "call_id": call.CallID, "name": call.Name, "status": "completed", valueKey: value}
	if call.Namespace != "" {
		completed["namespace"] = call.Namespace
	}

	var out []byte
	out = append(out, sseEncode("response.output_item.added", map[string]any{"type": "response.output_item.added", "output_index": outputIndex, "item": item})...)
	out = append(out, sseEncode(deltaEvent, map[string]any{"type": deltaEvent, "output_index": outputIndex, "item_id": call.ID, "delta": value})...)
	out = append(out, sseEncode(doneEvent, map[string]any{"type": doneEvent, "output_index": outputIndex, "item_id": call.ID, valueKey: value})...)
	out = append(out, sseEncode("response.output_item.done", map[string]any{"type": "response.output_item.done", "output_index": outputIndex, "item": completed})...)
	return out
}

func isToolItemEvent(payload map[string]any) bool {
	item, ok := payload["item"].(map[string]any)
	if !ok {
		return false
	}
	kind, _ := item["type"].(string)
	return kind == "function_call" || kind == "custom_tool_call"
}

func intField(m map[string]any, key string, fallback int) int {
	switch v := m[key].(type) {
	case float64:
		return int(v)
	case json.Number:
		if n, err := v.Int64(); err == nil {
			return int(n)
		}
	}
	return fallback
}

func stringField(m map[string]any, key string) string {
	s, _ := m[key].(string)
	return s
}

// sseEncode 编码一个 SSE 事件。
func sseEncode(eventName string, payload map[string]any) []byte {
	data, _ := json.Marshal(payload)
	var b strings.Builder
	b.WriteString("event: ")
	b.WriteString(eventName)
	b.WriteString("\ndata: ")
	b.Write(data)
	b.WriteString("\n\n")
	return []byte(b.String())
}

// parseSSEBlock 解析一个 SSE 块，返回 event 名和拼接后的 data。
func parseSSEBlock(raw []byte) (string, string) {
	var eventName string
	var dataLines []string
	for _, line := range strings.Split(strings.ReplaceAll(string(raw), "\r\n", "\n"), "\n") {
		if line == "" || strings.HasPrefix(line, ":") {
			continue
		}
		if rest, ok := strings.CutPrefix(line, "event:"); ok {
			eventName = strings.TrimSpace(rest)
			continue
		}
		if rest, ok := strings.CutPrefix(line, "data:"); ok {
			dataLines = append(dataLines, strings.TrimPrefix(rest, " "))
		}
	}
	return eventName, strings.Join(dataLines, "\n")
}

// splitSSEBlocks 是 bufio.Scanner 的分割函数，按空行切分 SSE 事件块。
func splitSSEBlocks(data []byte, atEOF bool) (advance int, token []byte, err error) {
	if atEOF && len(data) == 0 {
		return 0, nil, nil
	}
	for i := 0; i+1 < len(data); i++ {
		if data[i] == '\n' && data[i+1] == '\n' {
			return i + 2, data[:i], nil
		}
		if i+3 < len(data) && data[i] == '\r' && data[i+1] == '\n' && data[i+2] == '\r' && data[i+3] == '\n' {
			return i + 4, data[:i], nil
		}
	}
	if atEOF {
		return len(data), data, nil
	}
	return 0, nil, nil
}

var _ = strconv.Itoa
