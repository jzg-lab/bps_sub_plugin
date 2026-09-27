package tracelog

import (
	"bytes"
	"encoding/json"
	"strings"
)

// 一轮的结局。
const (
	OutcomeToolCall       = "tool_call"       // 调用了客户端声明过的工具
	OutcomeUnknownTool    = "unknown_tool"    // 调用了客户端没声明的工具（Codex 会回 unsupported call）
	OutcomeText           = "text"            // 给出最终回答
	OutcomeCommentaryOnly = "commentary_only" // 只说"我现在去做"，没调工具就结束了
	OutcomeNoCompleted    = "no_completed"    // 没收到 response.completed 就结束了
	OutcomeFailed         = "failed"          // response.failed / incomplete
	OutcomeNonSSE         = "non_sse"         // 不是 SSE（非流式或错误体）
	OutcomeError          = "error"           // 插件转发出错
	OutcomeCancelled      = "cancelled"       // 宿主在收到结局前主动结束（客户端断开、插件停用）
)

// Abnormal 判断结局是否属于要保存原文的异常。
func Abnormal(outcome string) bool {
	switch outcome {
	case OutcomeToolCall, OutcomeText, OutcomeNonSSE, OutcomeCancelled:
		return false
	}
	return true
}

// maxText 是摘要里保留的助手文字长度。
const maxText = 300

// Result 是从 SSE 响应里看出来的这一轮结局。
type Result struct {
	Outcome      string
	Tools        []string
	UnknownTools []string
	Text         string
	Error        string
}

// Analyze 解析发给宿主的 SSE 响应体。declared 是客户端声明过的工具名（含 namespace 前缀和裸名），
// 为 nil 时不判断未知工具。
func Analyze(body []byte, declared map[string]bool) Result {
	var result Result
	var completed map[string]any
	terminal := ""
	for _, block := range bytes.Split(bytes.ReplaceAll(body, []byte("\r\n"), []byte("\n")), []byte("\n\n")) {
		data := sseData(block)
		if len(data) == 0 || data[0] != '{' {
			continue
		}
		if !bytes.Contains(data, []byte(`"response.completed"`)) && !bytes.Contains(data, []byte(`"response.failed"`)) &&
			!bytes.Contains(data, []byte(`"response.incomplete"`)) && !bytes.Contains(data, []byte(`"error"`)) {
			continue
		}
		var event map[string]any
		if json.Unmarshal(data, &event) != nil {
			continue
		}
		switch kind, _ := event["type"].(string); kind {
		case "response.completed":
			completed, _ = event["response"].(map[string]any)
			terminal = kind
		case "response.failed", "response.incomplete":
			terminal = kind
			if resp, ok := event["response"].(map[string]any); ok {
				result.Error = errorText(resp["error"])
				if result.Error == "" {
					result.Error = errorText(resp["incomplete_details"])
				}
			}
		case "error":
			result.Error = errorText(event["error"])
			if result.Error == "" {
				result.Error = errorText(event)
			}
		}
	}

	switch {
	case completed != nil:
		output, _ := completed["output"].([]any)
		lastPhase := ""
		for _, raw := range output {
			item, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			switch kind, _ := item["type"].(string); kind {
			case "function_call", "custom_tool_call", "local_shell_call", "mcp_call", "tool_search_call":
				name, _ := item["name"].(string)
				if namespace, _ := item["namespace"].(string); namespace != "" && name != "" {
					name = namespace + "." + name
				}
				if name == "" {
					name = kind
				}
				result.Tools = append(result.Tools, name)
				if declared != nil && !isDeclared(declared, item) {
					result.UnknownTools = append(result.UnknownTools, name)
				}
			case "message":
				lastPhase, _ = item["phase"].(string)
				if text := messageText(item); text != "" {
					result.Text = text
				}
			}
		}
		switch {
		case len(result.UnknownTools) > 0:
			result.Outcome = OutcomeUnknownTool
		case len(result.Tools) > 0:
			result.Outcome = OutcomeToolCall
		case lastPhase == "commentary":
			result.Outcome = OutcomeCommentaryOnly
		default:
			result.Outcome = OutcomeText
		}
	case terminal != "":
		result.Outcome = OutcomeFailed
	case !bytes.Contains(body, []byte("data:")):
		result.Outcome = OutcomeNonSSE
	default:
		result.Outcome = OutcomeNoCompleted
	}
	if len(result.Text) > maxText {
		result.Text = truncateRunes(result.Text, maxText)
	}
	return result
}

func isDeclared(declared map[string]bool, item map[string]any) bool {
	name, _ := item["name"].(string)
	if name == "" {
		return true
	}
	if declared[name] {
		return true
	}
	if namespace, _ := item["namespace"].(string); namespace != "" && declared[namespace+"."+name] {
		return true
	}
	return false
}

// DeclaredTools 从 Codex 请求体里收集声明的工具名：顶层 tools 和 input 里的 additional_tools，
// namespace 展开成 "ns.name" 和裸名两种写法。没有任何工具声明时返回 nil。
func DeclaredTools(body map[string]any) map[string]bool {
	declared := map[string]bool{}
	var collect func(any, string)
	collect = func(raw any, namespace string) {
		list, _ := raw.([]any)
		for _, entry := range list {
			tool, ok := entry.(map[string]any)
			if !ok {
				continue
			}
			kind, _ := tool["type"].(string)
			name, _ := tool["name"].(string)
			if kind == "namespace" {
				collect(tool["tools"], name)
				continue
			}
			if name == "" {
				name = kind // web_search、image_generation 等只有 type
			}
			declared[name] = true
			if namespace != "" {
				declared[namespace+"."+name] = true
			}
		}
	}
	collect(body["tools"], "")
	if input, ok := body["input"].([]any); ok {
		for _, raw := range input {
			if item, ok := raw.(map[string]any); ok && item["type"] == "additional_tools" {
				collect(item["tools"], "")
			}
		}
	}
	if len(declared) == 0 {
		return nil
	}
	return declared
}

func sseData(block []byte) []byte {
	var lines [][]byte
	for _, line := range bytes.Split(block, []byte("\n")) {
		if rest, ok := bytes.CutPrefix(line, []byte("data:")); ok {
			lines = append(lines, bytes.TrimPrefix(rest, []byte(" ")))
		}
	}
	return bytes.Join(lines, []byte("\n"))
}

func messageText(item map[string]any) string {
	content, _ := item["content"].([]any)
	var b strings.Builder
	for _, raw := range content {
		if part, ok := raw.(map[string]any); ok {
			if text, ok := part["text"].(string); ok {
				b.WriteString(text)
			}
		}
	}
	return strings.TrimSpace(b.String())
}

func errorText(raw any) string {
	switch typed := raw.(type) {
	case map[string]any:
		for _, key := range []string{"message", "reason", "code", "type"} {
			if text, ok := typed[key].(string); ok && text != "" {
				return truncateRunes(text, maxText)
			}
		}
	case string:
		return truncateRunes(typed, maxText)
	}
	return ""
}

func truncateRunes(text string, limit int) string {
	runes := []rune(text)
	if len(runes) <= limit {
		return text
	}
	return string(runes[:limit]) + "…"
}
