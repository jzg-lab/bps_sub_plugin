package config

import (
	"encoding/json"
	"testing"
)

func TestParseEmptyNormalizesToDefaults(t *testing.T) {
	for _, raw := range []string{"", "  ", "{}"} {
		cfg, err := Parse([]byte(raw))
		if err != nil {
			t.Fatalf("Parse(%q): %v", raw, err)
		}
		if cfg != Default() {
			t.Fatalf("Parse(%q) = %+v, want defaults", raw, cfg)
		}
	}
}

func TestParseKeepsExplicitValues(t *testing.T) {
	cfg, err := Parse([]byte(`{"enable_http2":false,"use_account_proxy":false,"response_header_timeout_seconds":0}`))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.EnableHTTP2 || cfg.UseAccountProxy || cfg.ResponseHeaderTimeoutSeconds != 0 {
		t.Fatalf("explicit values lost: %+v", cfg)
	}
	if cfg.MaxIdleConnsPerHost != Default().MaxIdleConnsPerHost {
		t.Fatalf("unset field should keep default: %+v", cfg)
	}
}

func TestParseRejectsInvalidInput(t *testing.T) {
	cases := map[string]string{
		"unknown field":      `{"extra":1}`,
		"trailing object":    `{} {}`,
		"array root":         `[]`,
		"null root":          `null`,
		"wrong type":         `{"enable_http2":"yes"}`,
		"negative timeout":   `{"response_header_timeout_seconds":-1}`,
		"timeout too large":  `{"response_header_timeout_seconds":3601}`,
		"zero idle timeout":  `{"idle_conn_timeout_seconds":0}`,
		"zero idle conns":    `{"max_idle_conns_per_host":0}`,
		"too many idle conn": `{"max_idle_conns_per_host":1025}`,
	}
	for name, raw := range cases {
		if _, err := Parse([]byte(raw)); err == nil {
			t.Errorf("%s: Parse(%s) succeeded, want error", name, raw)
		}
	}
}

func TestJSONRoundTrip(t *testing.T) {
	want := Default()
	want.MaxIdleConnsPerHost = 7
	got, err := Parse(want.JSON())
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("round trip = %+v, want %+v", got, want)
	}
	var fields map[string]any
	if err := json.Unmarshal(want.JSON(), &fields); err != nil {
		t.Fatal(err)
	}
	if len(fields) != 5 {
		t.Fatalf("normalized JSON must contain every field, got %v", fields)
	}
}
