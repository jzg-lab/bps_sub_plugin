package tracelog

import (
	"encoding/json"
	"math"
	"mime"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var safeID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.:-]{0,159}$`)
var rpmLimit = regexp.MustCompile(`(?i)exceeded the ([0-9,]+) request[(]s[)] every ([0-9,]+) (second|minute|hour)[(]s[)]`)
var orgLimit = regexp.MustCompile(`(?i)for ([A-Za-z0-9_.-]+) in organization ([A-Za-z0-9_-]+) on (tokens|requests) per min`)
var limitValues = regexp.MustCompile(`(?i)(Limit|Used|Requested)[: ]+([0-9][0-9,]*)`)

// SafeHeaders records a fixed allowlist, never authentication or cookie values.
func SafeHeaders(h http.Header) map[string]string {
	out := make(map[string]string)
	for _, key := range []string{
		"x-request-id", "request-id", "cf-ray", "cf-cache-status", "server",
		"date", "retry-after", "retry-after-ms", "content-type", "openai-processing-ms",
		"x-ratelimit-limit-requests", "x-ratelimit-remaining-requests", "x-ratelimit-reset-requests",
		"x-ratelimit-limit-tokens", "x-ratelimit-remaining-tokens", "x-ratelimit-reset-tokens",
		"x-codex-primary-used-percent", "x-codex-primary-window-minutes", "x-codex-primary-reset-after-seconds",
		"x-codex-secondary-used-percent", "x-codex-secondary-window-minutes", "x-codex-secondary-reset-after-seconds",
	} {
		v := strings.TrimSpace(h.Get(key))
		if v == "" || len(v) > 160 {
			continue
		}
		switch key {
		case "x-request-id", "request-id", "cf-ray", "cf-cache-status", "server":
			if safeID.MatchString(v) {
				out[key] = v
			}
		case "content-type":
			if media, _, err := mime.ParseMediaType(v); err == nil {
				out[key] = media
			}
		case "date", "retry-after":
			if t, err := http.ParseTime(v); err == nil {
				out[key] = t.UTC().Format(time.RFC3339Nano)
			} else if key == "retry-after" && safeNumber(v) {
				out[key] = v
			}
		default:
			if safeNumber(v) {
				out[key] = v
			} else if strings.Contains(key, "reset") {
				if d, err := time.ParseDuration(v); err == nil && d >= 0 {
					out[key] = v
				}
			}
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func safeNumber(s string) bool {
	n, err := strconv.ParseFloat(s, 64)
	return err == nil && !math.IsNaN(n) && !math.IsInf(n, 0) && n >= 0 && n < 1e16
}
func identifier(s string) string {
	if safeID.MatchString(s) {
		return s
	}
	return ""
}
func parseInteger(s string) *int64 {
	n, err := strconv.ParseInt(strings.ReplaceAll(s, ",", ""), 10, 64)
	if err != nil || n < 0 {
		return nil
	}
	return &n
}

func signalFor(raw json.RawMessage, source string, status int) ErrorSignal {
	var e struct {
		Code    string          `json:"code"`
		Type    string          `json:"type"`
		Message string          `json:"message"`
		Reason  string          `json:"reason"`
		Detail  string          `json:"detail"`
		Headers json.RawMessage `json:"headers"`
	}
	_ = json.Unmarshal(raw, &e)
	if e.Message == "" {
		e.Message = e.Detail
	}
	if e.Message == "" {
		_ = json.Unmarshal(raw, &e.Message)
	}
	signal := ErrorSignal{Source: source, Code: identifier(e.Code), Type: identifier(e.Type), MessageHash: Fingerprint("error_message", e.Message), Class: "upstream_error"}
	if status >= 400 {
		signal.Status = status
	}
	text := strings.ToLower(e.Code + " " + e.Type + " " + e.Reason + " " + e.Message)
	switch {
	case strings.Contains(text, "model_not_found") || strings.Contains(text, "model_access"):
		signal.Class = "model_access"
	case strings.Contains(text, "policy"):
		signal.Class = "usage_policy"
	case strings.Contains(text, "quota") || strings.Contains(text, "usage_limit"):
		signal.Class = "quota_exhausted"
	case strings.Contains(text, "overloaded"):
		signal.Class = "overloaded"
	case strings.Contains(text, "rate_limit") || status == 429:
		signal.Class = "rate_limit_unknown"
	case status == 403:
		signal.Class = "forbidden_unknown"
	case status == 401:
		signal.Class = "authentication"
	case source == "response.incomplete":
		signal.Class = "incomplete"
	case status >= 500:
		signal.Class = "upstream_5xx"
	}
	if m := rpmLimit.FindStringSubmatch(e.Message); m != nil {
		signal.Class, signal.Metric, signal.Limit = "rate_limit", "requests", parseInteger(m[1])
		if n := parseInteger(m[2]); n != nil {
			multiplier := int64(1)
			switch strings.ToLower(m[3]) {
			case "minute":
				multiplier = 60
			case "hour":
				multiplier = 3600
			}
			if *n <= math.MaxInt64/multiplier {
				v := *n * multiplier
				signal.WindowSec = &v
			}
		}
	}
	if m := orgLimit.FindStringSubmatch(e.Message); m != nil {
		signal.Class, signal.Model, signal.Organization, signal.Metric = "rate_limit", identifier(m[1]), Fingerprint("organization", m[2]), strings.ToLower(m[3])
		v := int64(60)
		signal.WindowSec = &v
		for _, pair := range limitValues.FindAllStringSubmatch(e.Message, -1) {
			switch strings.ToLower(pair[1]) {
			case "limit":
				signal.Limit = parseInteger(pair[2])
			case "used":
				signal.Used = parseInteger(pair[2])
			case "requested":
				signal.Requested = parseInteger(pair[2])
			}
		}
	}
	signal.Headers = safeErrorHeaders(e.Headers)
	return signal
}

func safeErrorHeaders(raw json.RawMessage) map[string]string {
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil {
		return nil
	}
	headers := make(http.Header)
	for key, value := range fields {
		var text string
		if json.Unmarshal(value, &text) == nil {
			headers.Set(key, text)
			continue
		}
		var values []string
		if json.Unmarshal(value, &values) == nil && len(values) > 0 {
			headers.Set(key, values[0])
		}
	}
	return SafeHeaders(headers)
}
