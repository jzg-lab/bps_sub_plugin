// Package config 解析、校验并规范化插件配置。
package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
)

// 路由模式。
const (
	// RouteModeAll：白名单内的模型全部改走 basispoints。
	RouteModeAll = "all"
	// RouteModeSuffix：只有带 ModelSuffix 后缀的模型改走 basispoints，发出前去掉后缀。
	RouteModeSuffix = "suffix"
)

// DefaultBPSUserAgent 是实测能通过 basispoints 的浏览器 UA（Excel 插件运行在 Edge WebView 里）。
const DefaultBPSUserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/140.0.0.0 Safari/537.36 Edg/140.0.0.0"

// DefaultModels 是 2026-09-25 实测 basispoints 可用的模型。
var DefaultModels = []string{"gpt-5.6-sol", "gpt-5.6-terra", "gpt-5.6-luna", "gpt-6-astra"}

// Config 是插件的完整配置。字段统一 snake_case，空对象规范化为全部默认值。
type Config struct {
	// ResponseHeaderTimeoutSeconds 是等待上游响应头的超时（秒），0 表示不限制。
	// 与宿主 gateway.openai_response_header_timeout 的默认值保持一致。
	ResponseHeaderTimeoutSeconds int `json:"response_header_timeout_seconds"`
	// IdleConnTimeoutSeconds 是空闲连接保留时间（秒）。
	IdleConnTimeoutSeconds int `json:"idle_conn_timeout_seconds"`
	// MaxIdleConnsPerHost 是每个上游主机保留的最大空闲连接数。
	MaxIdleConnsPerHost int `json:"max_idle_conns_per_host"`
	// EnableHTTP2 控制是否尝试与上游协商 HTTP/2。
	EnableHTTP2 bool `json:"enable_http2"`
	// UseAccountProxy 为 true 时使用宿主为账号下发的代理；false 时直连。
	UseAccountProxy bool `json:"use_account_proxy"`

	// BPSEnabled 是 basispoints 改写的总开关，关闭时所有请求原样透传。
	BPSEnabled bool `json:"bps_enabled"`
	// RouteMode 见 RouteModeAll / RouteModeSuffix。
	RouteMode string `json:"route_mode"`
	// ModelSuffix 是 suffix 模式下标记"走 basispoints"的模型名后缀。
	ModelSuffix string `json:"model_suffix"`
	// Models 是允许改走 basispoints 的上游模型名（不含后缀）。
	Models []string `json:"models"`
	// FallbackToCodex 为 true 时，basispoints 拒绝请求（模型权限、请求体不兼容、
	// Cloudflare 拦截、连接失败）就改用原始请求发往 codex。
	FallbackToCodex bool `json:"fallback_to_codex"`
	// BPSUserAgent 是发往 basispoints 的 User-Agent。
	BPSUserAgent string `json:"bps_user_agent"`
	// MaxBodyBytes 是允许改写的最大请求体，超过就原样透传。
	MaxBodyBytes int64 `json:"max_body_bytes"`

	// ToolRelay 为 true 时，带工具的流式请求也改走 basispoints（工具中转，阶段 3）；
	// 关闭时带工具的请求继续走 codex（阶段 2 行为）。仅在 BPSEnabled 时有意义。
	ToolRelay bool `json:"tool_relay"`
	// ToolCallTTLSeconds 是工具调用映射在 KV 里的存活时间（秒）。
	ToolCallTTLSeconds int `json:"tool_call_ttl_seconds"`

	// ImageSupport 为 true 时，带图片的请求把图片上传到 basispoints 附件端点后改走 basispoints；
	// 关闭时带图片的请求走 codex（阶段 3 行为）。仅在 BPSEnabled 时有意义。
	ImageSupport bool `json:"image_support"`
	// MaxImageBytes 是单张图片解码后允许上传的上限，超过就降级成文本。
	MaxImageBytes int64 `json:"max_image_bytes"`
}

// Default 返回默认配置。
func Default() Config {
	return Config{
		ResponseHeaderTimeoutSeconds: 300,
		IdleConnTimeoutSeconds:       90,
		MaxIdleConnsPerHost:          32,
		EnableHTTP2:                  true,
		UseAccountProxy:              true,

		BPSEnabled:      false,
		RouteMode:       RouteModeAll,
		ModelSuffix:     "-bps",
		Models:          append([]string(nil), DefaultModels...),
		FallbackToCodex: true,
		BPSUserAgent:    DefaultBPSUserAgent,
		MaxBodyBytes:    32 << 20,

		ToolRelay:          true,
		ToolCallTTLSeconds: 7 * 24 * 60 * 60,

		ImageSupport:  true,
		MaxImageBytes: 10 << 20,
	}
}

// Parse 严格解析配置 JSON：拒绝未知字段、多余内容和越界值，缺省字段取默认值。
// 空输入视为空对象。返回的配置已规范化（去空白、模型去重）。
func Parse(raw []byte) (Config, error) {
	cfg := Default()
	if len(bytes.TrimSpace(raw)) == 0 {
		return cfg, nil
	}
	if !strings.HasPrefix(strings.TrimSpace(string(raw)), "{") {
		return Config{}, errors.New("配置根节点必须是对象")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&cfg); err != nil {
		return Config{}, fmt.Errorf("解析配置: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return Config{}, errors.New("配置只能包含一个 JSON 对象")
	}
	cfg.normalize()
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func (c *Config) normalize() {
	c.RouteMode = strings.ToLower(strings.TrimSpace(c.RouteMode))
	c.ModelSuffix = strings.TrimSpace(c.ModelSuffix)
	c.BPSUserAgent = strings.TrimSpace(c.BPSUserAgent)
	seen := make(map[string]bool, len(c.Models))
	models := make([]string, 0, len(c.Models))
	for _, model := range c.Models {
		model = strings.TrimSpace(model)
		if model == "" || seen[model] {
			continue
		}
		seen[model] = true
		models = append(models, model)
	}
	c.Models = models
}

// Validate 检查取值范围。
func (c Config) Validate() error {
	if c.ResponseHeaderTimeoutSeconds < 0 || c.ResponseHeaderTimeoutSeconds > 3600 {
		return errors.New("response_header_timeout_seconds 必须在 0 到 3600 之间")
	}
	if c.IdleConnTimeoutSeconds < 1 || c.IdleConnTimeoutSeconds > 3600 {
		return errors.New("idle_conn_timeout_seconds 必须在 1 到 3600 之间")
	}
	if c.MaxIdleConnsPerHost < 1 || c.MaxIdleConnsPerHost > 1024 {
		return errors.New("max_idle_conns_per_host 必须在 1 到 1024 之间")
	}
	if c.RouteMode != RouteModeAll && c.RouteMode != RouteModeSuffix {
		return fmt.Errorf("route_mode 只能是 %q 或 %q", RouteModeAll, RouteModeSuffix)
	}
	if c.RouteMode == RouteModeSuffix && c.ModelSuffix == "" {
		return errors.New("suffix 模式下 model_suffix 不能为空")
	}
	if len(c.ModelSuffix) > 32 {
		return errors.New("model_suffix 不能超过 32 个字符")
	}
	if len(c.Models) > 64 {
		return errors.New("models 最多 64 个")
	}
	for _, model := range c.Models {
		if len(model) > 128 || strings.ContainsAny(model, " \t\r\n") {
			return fmt.Errorf("模型名无效: %q", model)
		}
	}
	if c.BPSEnabled && len(c.Models) == 0 {
		return errors.New("启用 basispoints 时 models 不能为空")
	}
	if c.BPSUserAgent == "" || len(c.BPSUserAgent) > 512 || strings.ContainsAny(c.BPSUserAgent, "\r\n") {
		return errors.New("bps_user_agent 不能为空、不能超过 512 个字符、不能换行")
	}
	if c.MaxBodyBytes < 1<<20 || c.MaxBodyBytes > 256<<20 {
		return errors.New("max_body_bytes 必须在 1 MiB 到 256 MiB 之间")
	}
	if c.ToolCallTTLSeconds < 60 || c.ToolCallTTLSeconds > 90*24*60*60 {
		return errors.New("tool_call_ttl_seconds 必须在 60 秒到 90 天之间")
	}
	if c.MaxImageBytes < 1<<10 || c.MaxImageBytes > 64<<20 {
		return errors.New("max_image_bytes 必须在 1 KiB 到 64 MiB 之间")
	}
	return nil
}

// Clone 返回深拷贝，避免共享 Models 切片。
func (c Config) Clone() Config {
	c.Models = append([]string(nil), c.Models...)
	return c
}

// Equal 比较两份配置是否完全相同。
func (c Config) Equal(other Config) bool {
	return bytes.Equal(c.JSON(), other.JSON())
}

// JSON 返回规范化后的配置 JSON。
func (c Config) JSON() []byte {
	data, _ := json.Marshal(c)
	return data
}
