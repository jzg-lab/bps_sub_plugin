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
	// ReasonAccountNotSelected：配置了账号白名单，当前账号不在其中。
	ReasonAccountNotSelected = "account_not_selected"
	// ReasonPlanExcluded：账号套餐在 exclude_plan_types 里（默认排除免费号）。
	ReasonPlanExcluded = "plan_excluded"
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
	// NativeTools 表示工具声明在 input 的 additional_tools 里（新版 Codex）。basispoints
	// 直接认识这种声明，工具调用和历史都原样透传，不走 run_officejs 中转。
	NativeTools bool
	// NativeToolNames 是 additional_tools 里声明的工具名（含 namespace 展开的 "ns.name" 和裸名）。
	// 用来识别模型误调的 basispoints 自带工具（Excel、技能等），只在 NativeTools 时有值。
	NativeToolNames map[string]bool
	// HasImages 表示 input 里有可上传的内联图片。
	HasImages bool
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
	nativeTools := catalog.Empty() && hasAdditionalTools(parsed["input"])
	hasToolContext := !catalog.Empty() || nativeTools || hasToolHistory(parsed["input"])
	if hasToolContext && !cfg.ToolRelay {
		// 工具中转关闭：带工具的请求回到阶段 2 行为，走 codex。
		if !catalog.Empty() || nativeTools {
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

	// 附件分两类：图片可以上传后走 basispoints（image_support 开时）；
	// 其它文件/音频，或不认识的附件，一律走 codex。
	images, others := classifyAttachments(parsed["input"])
	if others {
		return Decision{Reason: ReasonHasAttachment}
	}
	if images && !cfg.ImageSupport {
		return Decision{Reason: ReasonHasAttachment}
	}

	if strings.TrimSpace(header.Get("Authorization")) == "" {
		return Decision{Reason: ReasonNoAuthorization}
	}
	if strings.TrimSpace(header.Get("Chatgpt-Account-Id")) == "" {
		return Decision{Reason: ReasonNoAccountID}
	}
	if cfg.PlanExcluded(PlanType(header)) {
		return Decision{Reason: ReasonPlanExcluded}
	}
	if nativeTools {
		// 原生工具：响应里的调用就是客户端声明的工具，不需要 SSE 还原。
		return Decision{Route: true, Model: model, Body: parsed, Catalog: catalog, Stream: stream, NativeTools: true, NativeToolNames: additionalToolNames(parsed["input"]), HasImages: images}
	}
	return Decision{Route: true, Model: model, Body: parsed, Catalog: catalog, Stream: stream, HasToolContext: hasToolContext, HasImages: images}
}

// additionalToolNames 收集 additional_tools 里声明的工具名（含 namespace 展开）。
func additionalToolNames(input any) map[string]bool {
	names := map[string]bool{}
	items, ok := input.([]any)
	if !ok {
		return names
	}
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
				name = kind
			}
			names[name] = true
			if namespace != "" {
				names[namespace+"."+name] = true
			}
		}
	}
	for _, raw := range items {
		if item, ok := raw.(map[string]any); ok && item["type"] == "additional_tools" {
			collect(item["tools"], "")
		}
	}
	return names
}

// hasAdditionalTools 判断 input 里有没有 additional_tools 工具声明（新版 Codex 不再发顶层 tools）。
func hasAdditionalTools(input any) bool {
	items, ok := input.([]any)
	if !ok {
		return false
	}
	for _, raw := range items {
		item, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if kind, _ := item["type"].(string); kind == "additional_tools" {
			if tools, ok := item["tools"].([]any); ok && len(tools) > 0 {
				return true
			}
		}
	}
	return false
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

// classifyAttachments 遍历输入，返回是否有可上传的内联图片（images），以及是否有
// 其它无法处理的附件（others：非 data 的图片、文件、音频）。others 命中就必须走 codex。
func classifyAttachments(value any) (images bool, others bool) {
	switch typed := value.(type) {
	case []any:
		for _, item := range typed {
			i, o := classifyAttachments(item)
			images = images || i
			others = others || o
		}
	case map[string]any:
		switch kind, _ := typed["type"].(string); kind {
		case "input_image":
			if url, ok := typed["image_url"].(string); ok && strings.HasPrefix(url, "data:") {
				images = true
			} else {
				others = true // 非 data URL 的图片（远程 URL / file_id），交给 codex
			}
			return images, others
		case "image_url", "input_file", "input_audio":
			return false, true
		}
		if content, ok := typed["content"]; ok {
			return classifyAttachments(content)
		}
	}
	return images, others
}
