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
	if !cfg.ImageSupport || cfg.MaxImageBytes != 10<<20 {
		t.Fatalf("image defaults wrong: %+v", cfg)
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
		"image too small":       `{"max_image_bytes":100}`,
		"image too large":       `{"max_image_bytes":134217728}`,
		"zero account id":       `{"account_ids":[0]}`,
		"negative account id":   `{"account_ids":[-3]}`,
		"string account id":     `{"account_ids":["12"]}`,
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
	if len(fields) != 21 {
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

func TestAccountIDsNormalizedAndSelected(t *testing.T) {
	cfg, err := Parse([]byte(`{"account_ids":[12,7,12]}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.AccountIDs) != 2 || cfg.AccountIDs[0] != 12 || cfg.AccountIDs[1] != 7 {
		t.Fatalf("account_ids not deduplicated: %v", cfg.AccountIDs)
	}
	if !cfg.AccountSelected(7) || cfg.AccountSelected(8) {
		t.Fatal("AccountSelected must follow the list")
	}
	if !Default().AccountSelected(8) {
		t.Fatal("empty account_ids must select every account")
	}
	if !strings.Contains(string(Default().JSON()), `"account_ids":[]`) {
		t.Fatalf("default account_ids must serialize as [] not null: %s", Default().JSON())
	}
	clone := cfg.Clone()
	clone.AccountIDs[0] = 99
	if cfg.AccountIDs[0] == 99 {
		t.Fatal("Clone must deep-copy AccountIDs")
	}
}

func TestTooManyAccountIDs(t *testing.T) {
	cfg := Default()
	for i := int64(1); i <= 1001; i++ {
		cfg.AccountIDs = append(cfg.AccountIDs, i)
	}
	if _, err := Parse(cfg.JSON()); err == nil {
		t.Fatal("1001 account ids must be rejected")
	}
}

func TestExcludePlanTypes(t *testing.T) {
	if !Default().PlanExcluded("free") || Default().PlanExcluded("self_serve_business_prolite") || Default().PlanExcluded("") {
		t.Fatal("default must exclude only free, and never an unknown plan")
	}
	cfg, err := Parse([]byte(`{"exclude_plan_types":[" Free ","team","free",""]}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.ExcludePlanTypes) != 2 || cfg.ExcludePlanTypes[0] != "free" || cfg.ExcludePlanTypes[1] != "team" {
		t.Fatalf("exclude_plan_types not normalized: %v", cfg.ExcludePlanTypes)
	}
	if !cfg.PlanExcluded("TEAM") {
		t.Fatal("plan match must ignore case")
	}
	empty, err := Parse([]byte(`{"exclude_plan_types":[]}`))
	if err != nil || empty.PlanExcluded("free") || !strings.Contains(string(empty.JSON()), `"exclude_plan_types":[]`) {
		t.Fatalf("empty list must exclude nothing and serialize as []: %v %s", err, empty.JSON())
	}
	if _, err := Parse([]byte(`{"exclude_plan_types":["a b"]}`)); err == nil {
		t.Fatal("plan with spaces must be rejected")
	}
	clone := cfg.Clone()
	clone.ExcludePlanTypes[0] = "x"
	if cfg.ExcludePlanTypes[0] != "free" {
		t.Fatal("Clone must deep-copy ExcludePlanTypes")
	}
}
