package basispoints

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
)

// ExternalClientInstructions 告诉模型这不是真实的 Excel 会话，不要调用 Excel 自带工具。
// 文案来自 excel-codex-bridge，已在其用户中验证过。
const ExternalClientInstructions = "This request is relayed by an external OpenAI Responses API client, not by " +
	"the live Excel workbook. Do not call server-injected Excel, Office, connector, " +
	"or workbook tools. Return the answer as assistant text."

// clientHeaders 是 Excel 插件的固定身份头（2026-09-25 实测可用）。
var clientHeaders = [][2]string{
	{"x-basispoints-auth-mode", "chatgpt"},
	{"x-openai-internal-basispoints-client-agent-profile", "excel"},
	{"x-openai-internal-basispoints-client-editor", "excel"},
	{"x-openai-internal-basispoints-client-host", "office"},
	{"x-openai-internal-basispoints-client-platform", "excel"},
	{"x-openai-internal-basispoints-client-platform-class", "PC"},
	{"x-openai-internal-basispoints-client-product", "basispoints-excel-plugin"},
	{"x-openai-internal-basispoints-client-runtime", "desktop"},
	{"x-openai-internal-basispoints-office-host", "Excel"},
	{"x-openai-internal-basispoints-office-platform", "PC"},
	{"x-stainless-arch", "unknown"},
	{"x-stainless-lang", "js"},
	{"x-stainless-os", "Unknown"},
	{"x-stainless-package-version", "6.31.0"},
	{"x-stainless-retry-count", "0"},
	{"x-stainless-runtime", "browser:chrome"},
	{"accept-encoding", "identity"},
	{"content-type", "application/json"},
	{"origin", "https://bps.openai.com"},
}

// BuildHeaders 从零构造发往 basispoints 的请求头：只从原请求带过去令牌和账号 ID，
// Codex 身份头（originator、version、session_id、x-codex-* 等）一律不带。
// 调用前 Decide 已确认 Authorization 和 chatgpt-account-id 存在。
func BuildHeaders(original http.Header, userAgent string, stream bool) http.Header {
	header := make(http.Header, len(clientHeaders)+5)
	accountID := strings.TrimSpace(original.Get("Chatgpt-Account-Id"))
	header.Set("Authorization", original.Get("Authorization"))
	header.Set("Chatgpt-Account-Id", accountID)
	header.Set("X-Openai-Account-Id", accountID)
	for _, pair := range clientHeaders {
		header.Set(pair[0], pair[1])
	}
	if stream {
		header.Set("Accept", "text/event-stream")
	} else {
		header.Set("Accept", "application/json")
	}
	header.Set("User-Agent", userAgent)
	return header
}

// Replayer 在多轮回放时按 call_id 取回上一轮记住的原生调用项（存在 KV 里）。
// 没有实现（或没命中）时回放走 fallback 重建。
type Replayer interface {
	RememberedNativeCall(callID string) (map[string]any, bool)
}

// BuildBody 按实测可用的字段白名单生成 basispoints 请求体。
// 只发已验证过的字段，其余（tools、include、text、client_metadata 等）一律丢掉，避免 422。
// 带工具时注入 catalog、把历史里的工具调用/结果回放成上游认识的形态。
func BuildBody(decision Decision, replayer Replayer) ([]byte, error) {
	source := decision.Body
	rawInput := source["input"]
	catalog := decision.Catalog
	if catalog == nil {
		catalog = &ToolCatalog{}
	}
	parallel := source["parallel_tool_calls"] != false
	history := translateInput(rawInput, catalog, replayer)

	input := make([]any, 0, len(history)+3)
	if instructions, ok := source["instructions"].(string); ok && strings.TrimSpace(instructions) != "" {
		input = append(input, messageItem("developer", instructions))
	}
	input = append(input, BuildToolCatalogMessages(catalog, parallel)...)
	input = append(input, history...)

	stream := true
	if value, ok := source["stream"].(bool); ok {
		stream = value
	}
	output := map[string]any{
		"model":              decision.Model,
		"model_selection":    "explicit",
		"stream":             stream,
		"store":              false,
		"input":              input,
		"reasoning_effort":   ReasoningEffort(source),
		"context_management": contextManagement(source["context_management"]),
		"metadata":           buildMetadata(source, history, rawInput),
	}
	if key, ok := source["prompt_cache_key"].(string); ok && strings.TrimSpace(key) != "" {
		output["prompt_cache_key"] = strings.TrimSpace(key)
	}

	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(output); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buffer.Bytes(), "\n"), nil
}

// ReasoningEffort 读取 reasoning.effort 或 reasoning_effort，映射到 basispoints 支持的
// low/medium/high/xhigh。basispoints 没有 max，降为 xhigh；没给或不认识的值取 medium。
func ReasoningEffort(source map[string]any) string {
	var requested string
	if reasoning, ok := source["reasoning"].(map[string]any); ok {
		requested, _ = reasoning["effort"].(string)
	}
	if requested == "" {
		requested, _ = source["reasoning_effort"].(string)
	}
	switch strings.ToLower(strings.TrimSpace(requested)) {
	case "none", "minimal", "low":
		return "low"
	case "high":
		return "high"
	case "xhigh", "x-high", "extra-high", "extra_high", "max":
		return "xhigh"
	default:
		return "medium"
	}
}

func contextManagement(raw any) any {
	if list, ok := raw.([]any); ok {
		return list
	}
	return []any{map[string]any{"type": "compaction", "compact_threshold": 200000}}
}

func messageItem(role, text string) map[string]any {
	contentType := "input_text"
	if role == "assistant" {
		contentType = "output_text"
	}
	return map[string]any{
		"type":    "message",
		"role":    role,
		"content": []any{map[string]any{"type": contentType, "text": text}},
	}
}

// translateInput 把 Codex 的 input 转成 basispoints 接受的形态：
//   - 字符串 input 包成一条 user 消息；
//   - 去掉 Codex 私有的 internal_chat_message_metadata_passthrough；
//   - reasoning 只保留带 encrypted_content 的（store=false 时上游拒收裸 reasoning）；
//   - 丢掉 item_reference（store=false 时上游无从解析）；
//   - 工具调用/结果回放成上游认识的形态（catalog 非空时）。
func translateInput(raw any, catalog *ToolCatalog, replayer Replayer) []any {
	if text, ok := raw.(string); ok {
		return []any{messageItem("user", text)}
	}
	items, ok := raw.([]any)
	if !ok {
		return []any{}
	}
	// callOrigins 记录 call_id → 回放后上游看到的工具名，供工具结果归一化用。
	callOrigins := map[string]string{}
	result := make([]any, 0, len(items))
	for _, rawItem := range items {
		item, ok := rawItem.(map[string]any)
		if !ok {
			continue
		}
		if _, has := item["internal_chat_message_metadata_passthrough"]; has {
			copied := make(map[string]any, len(item))
			for key, value := range item {
				if key != "internal_chat_message_metadata_passthrough" {
					copied[key] = value
				}
			}
			item = copied
		}
		switch kind, _ := item["type"].(string); kind {
		case "reasoning":
			if encrypted, ok := item["encrypted_content"].(string); ok && encrypted != "" {
				result = append(result, map[string]any{
					"type":              "reasoning",
					"summary":           []any{},
					"encrypted_content": encrypted,
				})
			}
		case "item_reference":
		case "function_call", "custom_tool_call":
			result = append(result, replayToolCall(item, catalog, replayer, callOrigins))
		case "function_call_output", "custom_tool_call_output":
			result = append(result, normalizedToolOutput(item, callOrigins))
		default:
			result = append(result, item)
		}
	}
	return result
}

// replayToolCall 把历史里的一条客户端工具调用回放成上游认识的形态：
//   - update_plan 是 basispoints 原生工具，转回原生 schema 直接发；
//   - KV 记住了对应的原生 run_officejs 调用就原样回放（保留上游 item 身份）；
//   - 否则用 fallback 重建一个 run_officejs 调用。
func replayToolCall(item map[string]any, catalog *ToolCatalog, replayer Replayer, origins map[string]string) map[string]any {
	callID := stringField(item, "call_id")
	name := stringField(item, "name")

	if name == "update_plan" {
		if callID != "" {
			origins[callID] = name
		}
		restored := restoreNativeFunctionArguments(name, decodeObject(stringField(item, "arguments")))
		if restored != nil {
			encoded, _ := json.Marshal(restored)
			copied := cloneItem(item)
			copied["arguments"] = string(encoded)
			return copied
		}
		return item
	}

	if replayer != nil && callID != "" {
		if remembered, ok := replayer.RememberedNativeCall(callID); ok {
			if nativeName := stringField(remembered, "name"); nativeName != "" {
				origins[callID] = nativeName
			}
			return remembered
		}
	}
	if callID != "" {
		origins[callID] = transportName
	}
	return fallbackTransportCall(item)
}

// fallbackTransportCall 在插件重启、KV 没命中时，用历史项重建一个 run_officejs 调用。
func fallbackTransportCall(item map[string]any) map[string]any {
	name := stringField(item, "name")
	var envelope map[string]any
	if item["type"] == "custom_tool_call" {
		input, _ := item["input"].(string)
		envelope = map[string]any{"name": name, "input": input}
	} else {
		arguments := decodeObject(stringField(item, "arguments"))
		if arguments == nil {
			arguments = map[string]any{}
		}
		envelope = map[string]any{"name": name, "arguments": arguments}
	}
	callID := stringField(item, "call_id")
	if callID == "" {
		callID = fallbackCallIDPrefix + shortHash(name)
	}
	code, _ := json.Marshal(envelope)
	nativeArgs, _ := json.Marshal(map[string]any{
		"summary":          "Run client tool " + name,
		"extended_summary": "Relay " + name + " through the external Codex client",
		"code":             string(code),
		"destructive":      false,
		"references":       []any{},
	})
	return map[string]any{
		"type":      "function_call",
		"id":        functionItemID(callID),
		"call_id":   callID,
		"name":      transportName,
		"arguments": string(nativeArgs),
		"status":    "completed",
	}
}

// normalizedToolOutput 归一化工具结果，让上游状态机接受：
//   - update_plan 的结果换成 {"status":"ok"}；
//   - 经 run_officejs 中转的 custom 输出改成 function_call_output；
//   - 对齐 function_call_output 的 id；空输出补一句成功提示。
func normalizedToolOutput(item map[string]any, origins map[string]string) map[string]any {
	result := cloneItem(item)
	callID := stringField(result, "call_id")
	origin := origins[callID]

	if origin == "update_plan" {
		result["output"] = `{"status":"ok"}`
		return result
	}
	if origin == transportName && callID != "" && result["type"] == "custom_tool_call_output" {
		result["type"] = "function_call_output"
	}
	if callID != "" && result["type"] == "function_call_output" {
		result["id"] = functionItemID(callID)
	}
	if text := itemOutputText(result["output"]); strings.TrimSpace(text) == "" {
		switch result["output"].(type) {
		case string, nil:
			result["output"] = "(tool call succeeded with no output)"
		}
	}
	return result
}

func itemOutputText(value any) string {
	switch typed := value.(type) {
	case string:
		return typed
	case []any:
		var sb strings.Builder
		for _, part := range typed {
			switch p := part.(type) {
			case string:
				sb.WriteString(p)
			case map[string]any:
				if text, ok := p["text"].(string); ok {
					sb.WriteString(text)
				}
			}
		}
		return sb.String()
	default:
		return ""
	}
}

func cloneItem(item map[string]any) map[string]any {
	copied := make(map[string]any, len(item))
	for key, value := range item {
		copied[key] = value
	}
	return copied
}
