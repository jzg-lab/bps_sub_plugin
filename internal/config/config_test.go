package config

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestParseEmptyNormalizesToDefaults(t *testing.T) {
	for _, raw := range []string{"", "  ", "{}"} {
		cfg, err := Parse([]byte(raw))
		if err != nil {
			t.Fatalf("Parse(%q): %v", raw, err)
		}
		if !cfg.Equal(Default()) {
			t.Fatalf("Parse(%q) = %+v, want defaults", raw, cfg)
		}
	}
}

func TestDefaultsKeepBasispointsOff(t *testing.T) {
	cfg := Default()
	if cfg.BPSEnabled {
		t.Fatal("basispoints must be off by default so upgrading does not change behavior")
	}
	if cfg.RouteMode != RouteModeAll || !cfg.FallbackToCodex || len(cfg.Models) != len(DefaultModels) {
		t.Fatalf("unexpected defaults: %+v", cfg)
	}
	if !cfg.ToolRelay || cfg.ToolCallTTLSeconds != 7*24*60*60 {
		t.Fatalf("tool relay defaults wrong: %+v", cfg)
	}
}

func TestParseKeepsExplicitValues(t *testing.T) {
	cfg, err := Parse([]byte(`{"enable_http2":false,"use_account_proxy":false,"response_header_timeout_seconds":0,
		"bps_enabled":true,"route_mode":" Suffix ","model_suffix":" -x ","models":[" gpt-5.6-sol ","gpt-5.6-sol",""],
		"fallback_to_codex":false}`))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.EnableHTTP2 || cfg.UseAccountProxy || cfg.ResponseHeaderTimeoutSeconds != 0 || cfg.FallbackToCodex {
		t.Fatalf("explicit values lost: %+v", cfg)
	}
	if !cfg.BPSEnabled || cfg.RouteMode != RouteModeSuffix || cfg.ModelSuffix != "-x" {
		t.Fatalf("route fields not normalized: %+v", cfg)
	}
	if len(cfg.Models) != 1 || cfg.Models[0] != "gpt-5.6-sol" {
		t.Fatalf("models not trimmed and deduplicated: %v", cfg.Models)
	}
	if cfg.MaxIdleConnsPerHost != Default().MaxIdleConnsPerHost {
		t.Fatalf("unset field should keep default: %+v", cfg)
	}
}

func TestParseRejectsInvalidInput(t *testing.T) {
	cases := map[string]string{
		"unknown field":         `{"extra":1}`,
		"trailing object":       `{} {}`,
		"array root":            `[]`,
		"null root":             `null`,
		"wrong type":            `{"enable_http2":"yes"}`,
		"negative timeout":      `{"response_header_timeout_seconds":-1}`,
		"timeout too large":     `{"response_header_timeout_seconds":3601}`,
		"zero idle timeout":     `{"idle_conn_timeout_seconds":0}`,
		"zero idle conns":       `{"max_idle_conns_per_host":0}`,
		"too many idle conn":    `{"max_idle_conns_per_host":1025}`,
		"bad route mode":        `{"route_mode":"some"}`,
		"suffix mode no suffix": `{"route_mode":"suffix","model_suffix":"  "}`,
		"enabled no models":     `{"bps_enabled":true,"models":[]}`,
		"model with space":      `{"models":["gpt 5"]}`,
		"empty user agent":      `{"bps_user_agent":" "}`,
		"user agent newline":    `{"bps_user_agent":"a\nb"}`,
		"body limit too small":  `{"max_body_bytes":1024}`,
		"body limit too large":  `{"max_body_bytes":1073741824}`,
		"ttl too small":         `{"tool_call_ttl_seconds":30}`,
		"ttl too large":         `{"tool_call_ttl_seconds":9999999}`,
	}
	for name, raw := range cases {
		if _, err := Parse([]byte(raw)); err == nil {
			t.Errorf("%s: Parse(%s) succeeded, want error", name, raw)
		}
	}
}

func TestDisabledAllowsEmptyModels(t *testing.T) {
	if _, err := Parse([]byte(`{"bps_enabled":false,"models":[]}`)); err != nil {
		t.Fatalf("empty models should be allowed while disabled: %v", err)
	}
}

func TestJSONRoundTrip(t *testing.T) {
	want := Default()
	want.MaxIdleConnsPerHost = 7
	want.Models = []string{"gpt-6-astra"}
	got, err := Parse(want.JSON())
	if err != nil {
		t.Fatal(err)
	}
	if !got.Equal(want) {
		t.Fatalf("round trip = %+v, want %+v", got, want)
	}
	var fields map[string]any
	if err := json.Unmarshal(want.JSON(), &fields); err != nil {
		t.Fatal(err)
	}
	if len(fields) != 14 {
		t.Fatalf("normalized JSON must contain every field, got %d: %v", len(fields), fields)
	}
	if strings.Contains(string(want.JSON()), `<`) {
		t.Fatal("unexpected HTML escaping")
	}
}

func TestCloneDoesNotShareModels(t *testing.T) {
	original := Default()
	clone := original.Clone()
	clone.Models[0] = "changed"
	if original.Models[0] == "changed" {
		t.Fatal("Clone must deep-copy Models")
	}
	defaults := Default()
	defaults.Models[0] = "changed"
	if DefaultModels[0] == "changed" {
		t.Fatal("Default must not share DefaultModels")
	}
}
