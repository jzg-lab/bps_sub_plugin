// Package basispoints 决定请求是否改走 basispoints，并把 Codex 形态的请求改写成
// basispoints（ChatGPT Excel 插件后端）接受的形态。全部是纯函数，不做网络 IO。
package basispoints

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"

	"github.com/jzg-lab/bps_sub_plugin/internal/config"
)

const (
	// URL 是 basispoints 的 Responses 端点。
	URL = "https://bps.openai.com/basispoints/api/responses"
	// Host 是 basispoints 的主机名。
	Host = "bps.openai.com"
	// CodexResponsesPath 是宿主发往 codex 的 Responses 路径，只有它会被改写。
	CodexResponsesPath = "/backend-api/codex/responses"
)

// 不改写的原因码，出现在状态统计里。
const (
	ReasonDisabled        = "disabled"
	ReasonNotResponses    = "not_responses"
	ReasonBodyTooLarge    = "body_too_large"
	ReasonBadBody         = "bad_body"
	ReasonModelNotAllowed = "model_not_allowed"
	ReasonHasTools        = "has_tools"
	ReasonHasToolHistory  = "has_tool_history"
	ReasonHasAttachment   = "has_attachment"
	ReasonNoAuthorization = "no_authorization"
	ReasonNoAccountID     = "no_account_id"
	ReasonToolNonStream   = "tool_non_stream"
)

// Decision 是路由判定结果。
type Decision struct {
	// Route 为 true 表示改走 basispoints。
	Route bool
	// Reason 是不改写的原因码，Route 为 true 时为空。
	Reason string
	// Model 是发往 basispoints 的模型名（已去掉后缀）。
	Model string
	// Body 是解析后的原始请求体，供 BuildBody 使用。
	Body map[string]any
	// Catalog 是这轮的客户端工具目录（可能为空）。
	Catalog *ToolCatalog
	// Stream 是客户端是否要求流式。
	Stream bool
	// HasToolContext 表示这轮带工具或历史里有工具调用，走工具中转路径。
	HasToolContext bool
}

// IsResponsesRequest 判断是否是可改写的 Responses 请求（不含 /compact 等子路径）。
func IsResponsesRequest(method string, target *url.URL) bool {
	return method == http.MethodPost && target != nil &&
		strings.EqualFold(target.Hostname(), "chatgpt.com") &&
		strings.TrimSuffix(target.Path, "/") == CodexResponsesPath
}

// Decide 判断一个 Responses 请求是否改走 basispoints。调用方需先用 IsResponsesRequest
// 过滤路径，并确认请求体没有超过 cfg.MaxBodyBytes。
func Decide(cfg config.Config, header http.Header, body []byte) Decision {
	if !cfg.BPSEnabled {
		return Decision{Reason: ReasonDisabled}
	}
	parsed, ok := parseObject(body)
	if !ok {
		return Decision{Reason: ReasonBadBody}
	}
	model, ok := matchModel(cfg, parsed["model"])
	if !ok {
		return Decision{Reason: ReasonModelNotAllowed}
	}

	catalog := ParseTools(parsed)
	hasToolContext := !catalog.Empty() || hasToolHistory(parsed["input"])
	if hasToolContext && !cfg.ToolRelay {
		// 工具中转关闭：带工具的请求回到阶段 2 行为，走 codex。
		if !catalog.Empty() {
			return Decision{Reason: ReasonHasTools}
		}
		return Decision{Reason: ReasonHasToolHistory}
	}

	stream := true
	if value, ok := parsed["stream"].(bool); ok {
		stream = value
	}
	if hasToolContext && !stream {
		// 工具中转只在流式做（SSE 实时还原）；非流式带工具走 codex。
		return Decision{Reason: ReasonToolNonStream}
	}

	if hasAttachment(parsed["input"]) {
		return Decision{Reason: ReasonHasAttachment}
	}
	if strings.TrimSpace(header.Get("Authorization")) == "" {
		return Decision{Reason: ReasonNoAuthorization}
	}
	if strings.TrimSpace(header.Get("Chatgpt-Account-Id")) == "" {
		return Decision{Reason: ReasonNoAccountID}
	}
	return Decision{Route: true, Model: model, Body: parsed, Catalog: catalog, Stream: stream, HasToolContext: hasToolContext}
}

// parseObject 解析 JSON 对象，数字保留原样（json.Number），避免大整数失真。
func parseObject(body []byte) (map[string]any, bool) {
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	var parsed map[string]any
	if err := decoder.Decode(&parsed); err != nil || parsed == nil {
		return nil, false
	}
	if decoder.More() {
		return nil, false
	}
	return parsed, true
}

// matchModel 按路由模式处理后缀并检查白名单，返回发往上游的模型名。
func matchModel(cfg config.Config, raw any) (string, bool) {
	model, _ := raw.(string)
	model = strings.TrimSpace(model)
	if model == "" {
		return "", false
	}
	suffix := cfg.ModelSuffix
	hasSuffix := suffix != "" && strings.HasSuffix(model, suffix) && len(model) > len(suffix)
	switch cfg.RouteMode {
	case config.RouteModeSuffix:
		if !hasSuffix {
			return "", false
		}
		model = strings.TrimSuffix(model, suffix)
	default:
		if hasSuffix {
			model = strings.TrimSuffix(model, suffix)
		}
	}
	for _, allowed := range cfg.Models {
		if strings.EqualFold(allowed, model) {
			return allowed, true
		}
	}
	return "", false
}

var toolHistoryTypes = map[string]bool{
	"function_call":           true,
	"function_call_output":    true,
	"custom_tool_call":        true,
	"custom_tool_call_output": true,
	"local_shell_call":        true,
	"local_shell_call_output": true,
	"mcp_call":                true,
	"web_search_call":         true,
	"tool_search_call":        true,
	"tool_search_output":      true,
}

// hasToolHistory 判断会话历史里是否有工具调用。basispoints 不认识客户端工具，
// 这类历史要等阶段 3 的工具中转来处理。
func hasToolHistory(input any) bool {
	items, ok := input.([]any)
	if !ok {
		return false
	}
	for _, raw := range items {
		item, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if kind, _ := item["type"].(string); toolHistoryTypes[kind] {
			return true
		}
		if role, _ := item["role"].(string); role == "tool" {
			return true
		}
	}
	return false
}

var attachmentTypes = map[string]bool{
	"input_image": true,
	"image_url":   true,
	"input_file":  true,
	"input_audio": true,
}

// hasAttachment 判断输入里是否有图片、文件或音频。basispoints 拒收内联图片，
// 需要先上传附件，本阶段不做。
func hasAttachment(value any) bool {
	switch typed := value.(type) {
	case []any:
		for _, item := range typed {
			if hasAttachment(item) {
				return true
			}
		}
	case map[string]any:
		if kind, _ := typed["type"].(string); attachmentTypes[kind] {
			return true
		}
		if content, ok := typed["content"]; ok {
			return hasAttachment(content)
		}
	}
	return false
}
