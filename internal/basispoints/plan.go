package basispoints

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"
)

// PlanType 从 Authorization 里的 ChatGPT access token（JWT）读出套餐类型（如 free、
// self_serve_business_prolite）。只解码 payload、不验签：这里只用来做路由决策，
// token 的真伪由上游校验。读不出返回空串。
func PlanType(header http.Header) string {
	token := strings.TrimSpace(header.Get("Authorization"))
	if len(token) > 7 && strings.EqualFold(token[:7], "bearer ") {
		token = strings.TrimSpace(token[7:])
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return ""
	}
	payload, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if err != nil {
		return ""
	}
	var claims struct {
		Auth struct {
			PlanType string `json:"chatgpt_plan_type"`
		} `json:"https://api.openai.com/auth"`
	}
	if json.Unmarshal(payload, &claims) != nil {
		return ""
	}
	return strings.ToLower(strings.TrimSpace(claims.Auth.PlanType))
}
