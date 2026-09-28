package transport

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/jzg-lab/bps_sub_plugin/internal/tracelog"
)

var diagnosticHost = func() string { host, _ := os.Hostname(); return tracelog.Fingerprint("host", host) }()
var diagnosticInstance = tracelog.Fingerprint("instance", diagnosticHost+strconv.Itoa(os.Getpid())+time.Now().Format(time.RFC3339Nano))
var forwardSequence atomic.Uint64

func requestIdentity(h http.Header, proxy string, useProxy bool) tracelog.Identity {
	workspace := strings.TrimSpace(h.Get("Chatgpt-Account-Id"))
	identity := tracelog.Identity{
		Workspace:     tracelog.Fingerprint("workspace", workspace),
		HeaderSession: tracelog.Fingerprint("session", h.Get("session_id")),
		Thread:        tracelog.Fingerprint("thread", h.Get("conversation_id")),
		ClientRequest: tracelog.Fingerprint("client_request", h.Get("X-Client-Request-Id")),
		ProxyMode:     "direct",
	}
	if useProxy && strings.TrimSpace(proxy) != "" {
		identity.ProxyMode = "account_proxy"
		if u, err := url.Parse(proxy); err == nil && u.Host != "" {
			endpoint := strings.ToLower(u.Scheme + "://" + u.Host)
			identity.ProxyEndpoint = tracelog.Fingerprint("proxy_endpoint", endpoint)
			if u.User != nil {
				identity.ProxyIdentity = tracelog.Fingerprint("proxy_identity", endpoint+"|"+u.User.Username())
			}
		}
	}
	authorization := strings.Fields(h.Get("Authorization"))
	if len(authorization) != 2 || !strings.EqualFold(authorization[0], "Bearer") || len(authorization[1]) > 64<<10 {
		return identity
	}
	parts := strings.Split(authorization[1], ".")
	if len(parts) != 3 {
		return identity
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return identity
	}
	var claims struct {
		Subject string      `json:"sub"`
		Expires json.Number `json:"exp"`
		Auth    struct {
			Workspace string `json:"chatgpt_account_id"`
			User      string `json:"chatgpt_user_id"`
		} `json:"https://api.openai.com/auth"`
	}
	if json.Unmarshal(payload, &claims) != nil {
		return identity
	}
	identity.JWTParsed = true
	identity.JWTWorkspace = tracelog.Fingerprint("workspace", claims.Auth.Workspace)
	identity.User = tracelog.Fingerprint("user", claims.Auth.User)
	identity.Subject = tracelog.Fingerprint("subject", claims.Subject)
	if workspace != "" && claims.Auth.Workspace != "" {
		match := workspace == strings.TrimSpace(claims.Auth.Workspace)
		identity.WorkspaceMatch = &match
	}
	if exp, err := claims.Expires.Int64(); err == nil && exp > 0 && exp < 253402300800 {
		identity.TokenExpiresAt = time.Unix(exp, 0).UTC().Format(time.RFC3339)
	}
	return identity
}
