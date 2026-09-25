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
}

// Default 返回默认配置。
func Default() Config {
	return Config{
		ResponseHeaderTimeoutSeconds: 300,
		IdleConnTimeoutSeconds:       90,
		MaxIdleConnsPerHost:          32,
		EnableHTTP2:                  true,
		UseAccountProxy:              true,
	}
}

// Parse 严格解析配置 JSON：拒绝未知字段、多余内容和越界值，缺省字段取默认值。
// 空输入视为空对象。
func Parse(raw []byte) (Config, error) {
	cfg := Default()
	if len(bytes.TrimSpace(raw)) == 0 {
		return cfg, nil
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&cfg); err != nil {
		return Config{}, fmt.Errorf("解析配置: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return Config{}, errors.New("配置只能包含一个 JSON 对象")
	}
	if !strings.HasPrefix(strings.TrimSpace(string(raw)), "{") {
		return Config{}, errors.New("配置根节点必须是对象")
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
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
	return nil
}

// JSON 返回规范化后的配置 JSON。
func (c Config) JSON() []byte {
	data, _ := json.Marshal(c)
	return data
}
