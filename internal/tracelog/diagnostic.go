package tracelog

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

const DiagnosticSchema = 1

// Fingerprint is a stable, domain-separated correlation key, not anonymization.
func Fingerprint(kind, value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	sum := sha256.Sum256([]byte("bps-diagnostics-v1:" + kind + "\x00" + value))
	return hex.EncodeToString(sum[:16])
}

type Identity struct {
	Workspace      string `json:"workspace_hash,omitempty"`
	JWTWorkspace   string `json:"jwt_workspace_hash,omitempty"`
	User           string `json:"user_hash,omitempty"`
	Subject        string `json:"jwt_subject_hash,omitempty"`
	JWTParsed      bool   `json:"jwt_parsed_unverified,omitempty"`
	WorkspaceMatch *bool  `json:"jwt_workspace_matches_header,omitempty"`
	TokenExpiresAt string `json:"token_expires_at,omitempty"`
	CacheKey       string `json:"cache_key_hash,omitempty"`
	HeaderSession  string `json:"header_session_hash,omitempty"`
	BodySession    string `json:"body_session_hash,omitempty"`
	Thread         string `json:"thread_hash,omitempty"`
	ClientRequest  string `json:"client_request_hash,omitempty"`
	ProxyEndpoint  string `json:"proxy_endpoint_hash,omitempty"`
	ProxyIdentity  string `json:"proxy_identity_hash,omitempty"`
	ProxyMode      string `json:"proxy_mode"`
	ExitIPKnown    bool   `json:"exit_ip_known"`
}

type WireTiming struct {
	HeadersWrittenAt  string `json:"headers_written_at,omitempty"`
	RequestWrittenAt  string `json:"request_written_at,omitempty"`
	FirstResponseAt   string `json:"first_response_byte_at,omitempty"`
	HeadersReceivedAt string `json:"headers_received_at,omitempty"`
	HeadersWrites     int    `json:"header_write_count,omitempty"`
	RequestWrites     int    `json:"request_write_count,omitempty"`
	WriteFailed       bool   `json:"write_failed,omitempty"`
	ConnectionReused  *bool  `json:"connection_reused,omitempty"`
	ConnectionIdleMS  int64  `json:"connection_idle_ms,omitempty"`
}

type Usage struct {
	Input     *int64 `json:"input_tokens,omitempty"`
	Output    *int64 `json:"output_tokens,omitempty"`
	Cached    *int64 `json:"cached_input_tokens,omitempty"`
	Reasoning *int64 `json:"reasoning_output_tokens,omitempty"`
}

type ErrorSignal struct {
	Status       int               `json:"status_code,omitempty"`
	Source       string            `json:"source"`
	Class        string            `json:"class"`
	Code         string            `json:"code,omitempty"`
	Type         string            `json:"type,omitempty"`
	MessageHash  string            `json:"message_hash,omitempty"`
	Organization string            `json:"organization_hash,omitempty"`
	Model        string            `json:"model,omitempty"`
	Metric       string            `json:"metric,omitempty"`
	Limit        *int64            `json:"limit,omitempty"`
	Used         *int64            `json:"used,omitempty"`
	Requested    *int64            `json:"requested,omitempty"`
	WindowSec    *int64            `json:"window_seconds,omitempty"`
	Headers      map[string]string `json:"headers,omitempty"`
}

type Observation struct {
	Terminal       string        `json:"terminal_event,omitempty"`
	ResponseID     string        `json:"response_id_hash,omitempty"`
	ResponseModel  string        `json:"response_model,omitempty"`
	Usage          *Usage        `json:"usage,omitempty"`
	Signals        []ErrorSignal `json:"errors,omitempty"`
	Events         int64         `json:"sse_events,omitempty"`
	Oversized      int64         `json:"oversized_events,omitempty"`
	Invalid        int64         `json:"invalid_events,omitempty"`
	SignalsDropped int64         `json:"error_signals_dropped,omitempty"`
}

// UpstreamEvent contains metadata only. Each RoundTrip emits start and finish.
type UpstreamEvent struct {
	Schema             int               `json:"schema_version"`
	Event              string            `json:"event"`
	Time               string            `json:"time"`
	Instance           string            `json:"instance_id"`
	Host               string            `json:"host_hash,omitempty"`
	Version            string            `json:"plugin_version"`
	ForwardID          string            `json:"forward_id"`
	RequestID          string            `json:"request_id,omitempty"`
	Attempt            int               `json:"attempt"`
	AccountID          int64             `json:"account_id,omitempty"`
	AccountConcurrency int32             `json:"configured_account_concurrency,omitempty"`
	Identity           Identity          `json:"identity"`
	Plan               string            `json:"plan,omitempty"`
	Model              string            `json:"model,omitempty"`
	Endpoint           string            `json:"endpoint"`
	Method             string            `json:"method"`
	RequestBytes       int64             `json:"request_content_length"`
	UserAgentHash      string            `json:"user_agent_hash,omitempty"`
	InFlight           map[string]int64  `json:"local_inflight_at_start,omitempty"`
	StartedAt          string            `json:"started_at"`
	FinishedAt         string            `json:"finished_at,omitempty"`
	DurationMS         int64             `json:"duration_ms,omitempty"`
	Wire               WireTiming        `json:"wire"`
	HTTPStatus         int               `json:"http_status,omitempty"`
	Protocol           string            `json:"protocol,omitempty"`
	Headers            map[string]string `json:"headers,omitempty"`
	Organization       string            `json:"response_organization_hash,omitempty"`
	Project            string            `json:"response_project_hash,omitempty"`
	BytesRead          int64             `json:"bytes_read,omitempty"`
	FinishedBy         string            `json:"finished_by,omitempty"`
	Cancelled          bool              `json:"cancelled,omitempty"`
	TransportError     string            `json:"transport_error_kind,omitempty"`
	Observation        *Observation      `json:"observation,omitempty"`
	WriterDropped      int64             `json:"diagnostic_dropped_before_enqueue,omitempty"`
}
