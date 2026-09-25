package basispoints

import (
	"encoding/json"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"github.com/jzg-lab/bps_sub_plugin/internal/config"
)

func enabledConfig() config.Config {
	cfg := config.Default()
	cfg.BPSEnabled = true
	return cfg
}

func relayOffConfig() config.Config {
	cfg := enabledConfig()
	cfg.ToolRelay = false
	return cfg
}

func imageOffConfig() config.Config {
	cfg := enabledConfig()
	cfg.ImageSupport = false
	return cfg
}

func identityHeader() http.Header {
	header := http.Header{}
	header.Set("Authorization", "Bearer tok")
	header.Set("chatgpt-account-id", "acct-1")
	return header
}

func mustURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	parsed, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return parsed
}

func TestIsResponsesRequest(t *testing.T) {
	cases := map[string]bool{
		"https://chatgpt.com/backend-api/codex/responses":           true,
		"https://chatgpt.com/backend-api/codex/responses/":          true,
		"https://ChatGPT.com/backend-api/codex/responses?x=1":       true,
		"https://chatgpt.com/backend-api/codex/responses/compact":   false,
		"https://chatgpt.com/backend-api/codex/alpha/search":        false,
		"https://chatgpt.com/backend-api/codex/models":              false,
		"https://example.com/backend-api/codex/responses":           false,
		"https://chatgpt.com.evil.test/backend-api/codex/responses": false,
	}
	for raw, want := range cases {
		if got := IsResponsesRequest(http.MethodPost, mustURL(t, raw)); got != want {
			t.Errorf("IsResponsesRequest(POST %s) = %v, want %v", raw, got, want)
		}
	}
	if IsResponsesRequest(http.MethodGet, mustURL(t, "https://chatgpt.com/backend-api/codex/responses")) {
		t.Error("GET must not be rewritten")
	}
}

func TestDecideReasons(t *testing.T) {
	disabled := config.Default()
	cases := []struct {
		name   string
		cfg    config.Config
		header http.Header
		body   string
		reason string
	}{
		{"disabled", disabled, identityHeader(), `{"model":"gpt-5.6-sol"}`, ReasonDisabled},
		{"not json", enabledConfig(), identityHeader(), `not json`, ReasonBadBody},
		{"array body", enabledConfig(), identityHeader(), `[1]`, ReasonBadBody},
		{"trailing data", enabledConfig(), identityHeader(), `{"model":"gpt-5.6-sol"} {}`, ReasonBadBody},
		{"no model", enabledConfig(), identityHeader(), `{}`, ReasonModelNotAllowed},
		{"other model", enabledConfig(), identityHeader(), `{"model":"gpt-5.5"}`, ReasonModelNotAllowed},
		{"tools relay off", relayOffConfig(), identityHeader(), `{"model":"gpt-5.6-sol","tools":[{"type":"function","name":"x"}]}`, ReasonHasTools},
		{"tool history relay off", relayOffConfig(), identityHeader(), `{"model":"gpt-5.6-sol","input":[{"type":"function_call_output","call_id":"c","output":"x"}]}`, ReasonHasToolHistory},
		{"tool non-stream", enabledConfig(), identityHeader(), `{"model":"gpt-5.6-sol","stream":false,"tools":[{"type":"function","name":"x"}]}`, ReasonToolNonStream},
		{"image support off", imageOffConfig(), identityHeader(), `{"model":"gpt-5.6-sol","input":[{"role":"user","content":[{"type":"input_image","image_url":"data:image/png;base64,AAAA"}]}]}`, ReasonHasAttachment},
		{"remote image", enabledConfig(), identityHeader(), `{"model":"gpt-5.6-sol","input":[{"role":"user","content":[{"type":"input_image","image_url":"https://x/y.png"}]}]}`, ReasonHasAttachment},
		{"input file", enabledConfig(), identityHeader(), `{"model":"gpt-5.6-sol","input":[{"role":"user","content":[{"type":"input_file","file_id":"f"}]}]}`, ReasonHasAttachment},
		{"no auth", enabledConfig(), http.Header{"Chatgpt-Account-Id": {"a"}}, `{"model":"gpt-5.6-sol"}`, ReasonNoAuthorization},
		{"no account", enabledConfig(), http.Header{"Authorization": {"Bearer t"}}, `{"model":"gpt-5.6-sol"}`, ReasonNoAccountID},
	}
	for _, tc := range cases {
		decision := Decide(tc.cfg, tc.header, []byte(tc.body))
		if decision.Route || decision.Reason != tc.reason {
			t.Errorf("%s: got route=%v reason=%q, want reason %q", tc.name, decision.Route, decision.Reason, tc.reason)
		}
	}
}

func TestDecideRoutesAllowedModel(t *testing.T) {
	decision := Decide(enabledConfig(), identityHeader(), []byte(`{"model":" GPT-5.6-Sol ","tools":[],"input":"hi"}`))
	if !decision.Route || decision.Reason != "" || decision.Model != "gpt-5.6-sol" {
		t.Fatalf("decision = %+v", decision)
	}
}

func TestDecideSuffixModes(t *testing.T) {
	all := enabledConfig()
	if d := Decide(all, identityHeader(), []byte(`{"model":"gpt-6-astra-bps"}`)); !d.Route || d.Model != "gpt-6-astra" {
		t.Fatalf("all mode should strip suffix: %+v", d)
	}
	suffix := enabledConfig()
	suffix.RouteMode = config.RouteModeSuffix
	if d := Decide(suffix, identityHeader(), []byte(`{"model":"gpt-6-astra"}`)); d.Route || d.Reason != ReasonModelNotAllowed {
		t.Fatalf("suffix mode must skip unsuffixed model: %+v", d)
	}
	if d := Decide(suffix, identityHeader(), []byte(`{"model":"gpt-6-astra-bps"}`)); !d.Route || d.Model != "gpt-6-astra" {
		t.Fatalf("suffix mode should route suffixed model: %+v", d)
	}
	if d := Decide(suffix, identityHeader(), []byte(`{"model":"-bps"}`)); d.Route {
		t.Fatalf("bare suffix must not match: %+v", d)
	}
}

func TestBuildHeadersDropsCodexIdentity(t *testing.T) {
	original := identityHeader()
	for _, key := range []string{"originator", "version", "session_id", "conversation_id", "OpenAI-Beta", "x-codex-turn-state", "User-Agent", "Cookie"} {
		original.Set(key, "codex")
	}
	header := BuildHeaders(original, "UA/1", true)
	for _, key := range []string{"originator", "version", "session_id", "conversation_id", "OpenAI-Beta", "x-codex-turn-state", "Cookie"} {
		if header.Get(key) != "" {
			t.Errorf("header %s must not be forwarded", key)
		}
	}
	checks := map[string]string{
		"Authorization":                                "Bearer tok",
		"Chatgpt-Account-Id":                           "acct-1",
		"X-Openai-Account-Id":                          "acct-1",
		"X-Basispoints-Auth-Mode":                      "chatgpt",
		"X-Openai-Internal-Basispoints-Client-Product": "basispoints-excel-plugin",
		"Accept":          "text/event-stream",
		"Accept-Encoding": "identity",
		"Origin":          "https://bps.openai.com",
		"User-Agent":      "UA/1",
		"Content-Type":    "application/json",
	}
	for key, want := range checks {
		if got := header.Get(key); got != want {
			t.Errorf("%s = %q, want %q", key, got, want)
		}
	}
	if BuildHeaders(original, "UA/1", false).Get("Accept") != "application/json" {
		t.Error("non-stream Accept should be application/json")
	}
}

func build(t *testing.T, body string) map[string]any {
	t.Helper()
	decision := Decide(enabledConfig(), identityHeader(), []byte(body))
	if !decision.Route {
		t.Fatalf("expected route, got %q", decision.Reason)
	}
	raw, err := BuildBody(decision, nil)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestBuildBodyWhitelistsFields(t *testing.T) {
	out := build(t, `{
		"model":"gpt-5.6-sol-bps","stream":true,"store":true,"instructions":"Be terse.",
		"reasoning":{"effort":"high","summary":"auto"},"include":["reasoning.encrypted_content"],
		"tools":[],"tool_choice":"auto","parallel_tool_calls":true,"text":{"verbosity":"low"},
		"prompt_cache_key":" pck ","client_metadata":{"x":"y"},"service_tier":"priority",
		"metadata":{"a":"b","n":3,"nested":{"x":1}},
		"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}]}`)
	allowed := map[string]bool{
		"model": true, "model_selection": true, "stream": true, "store": true, "input": true,
		"reasoning_effort": true, "context_management": true, "metadata": true, "prompt_cache_key": true,
	}
	for key := range out {
		if !allowed[key] {
			t.Errorf("field %q must not be sent to basispoints", key)
		}
	}
	if out["model"] != "gpt-5.6-sol" || out["model_selection"] != "explicit" || out["store"] != false || out["stream"] != true {
		t.Fatalf("core fields wrong: %v", out)
	}
	if out["reasoning_effort"] != "high" || out["prompt_cache_key"] != "pck" {
		t.Fatalf("effort/cache key wrong: %v", out)
	}
	input := out["input"].([]any)
	if len(input) != 3 {
		t.Fatalf("input = %v", input)
	}
	first := input[0].(map[string]any)
	if first["role"] != "developer" || !strings.Contains(first["content"].([]any)[0].(map[string]any)["text"].(string), "Be terse.") {
		t.Fatalf("instructions must become the first developer message: %v", first)
	}
	second := input[1].(map[string]any)
	if second["content"].([]any)[0].(map[string]any)["text"] != ExternalClientInstructions {
		t.Fatalf("external client notice missing: %v", second)
	}
	metadata := out["metadata"].(map[string]any)
	if metadata["a"] != "b" || metadata["n"] != "3" || metadata["nested"] != nil {
		t.Fatalf("metadata scalars not preserved correctly: %v", metadata)
	}
	cm := out["context_management"].([]any)[0].(map[string]any)
	if cm["type"] != "compaction" || cm["compact_threshold"] != float64(200000) {
		t.Fatalf("context_management default wrong: %v", cm)
	}
}

func TestBuildBodyStringInputAndDefaults(t *testing.T) {
	out := build(t, `{"model":"gpt-6-astra","input":"hello","stream":false}`)
	if out["stream"] != false || out["reasoning_effort"] != "medium" {
		t.Fatalf("defaults wrong: %v", out)
	}
	if _, ok := out["prompt_cache_key"]; ok {
		t.Fatal("empty prompt_cache_key must be omitted")
	}
	input := out["input"].([]any)
	if len(input) != 2 {
		t.Fatalf("without instructions only the notice is prepended: %v", input)
	}
	user := input[1].(map[string]any)
	if user["role"] != "user" || user["content"].([]any)[0].(map[string]any)["text"] != "hello" {
		t.Fatalf("string input not wrapped: %v", user)
	}
}

func TestBuildBodyMissingStreamDefaultsToTrue(t *testing.T) {
	if out := build(t, `{"model":"gpt-6-astra","input":"x"}`); out["stream"] != true {
		t.Fatalf("stream should default to true like the host forces: %v", out["stream"])
	}
}

func TestTranslateInputFiltersItems(t *testing.T) {
	out := build(t, `{"model":"gpt-5.6-sol","input":[
		{"type":"message","role":"user","content":"a","internal_chat_message_metadata_passthrough":{"turn_id":"x"}},
		{"type":"reasoning","summary":[{"type":"summary_text","text":"s"}],"id":"rs_1"},
		{"type":"reasoning","id":"rs_2","summary":[],"encrypted_content":"ENC"},
		{"type":"item_reference","id":"msg_1"},
		{"type":"message","role":"assistant","content":[{"type":"output_text","text":"b"}]}]}`)
	input := out["input"].([]any)[1:] // 跳过外部客户端提示
	if len(input) != 3 {
		t.Fatalf("input = %v", input)
	}
	if _, has := input[0].(map[string]any)["internal_chat_message_metadata_passthrough"]; has {
		t.Fatal("codex private metadata must be stripped")
	}
	reasoning := input[1].(map[string]any)
	if reasoning["encrypted_content"] != "ENC" || reasoning["id"] != nil || len(reasoning["summary"].([]any)) != 0 {
		t.Fatalf("encrypted reasoning not normalized: %v", reasoning)
	}
	if input[2].(map[string]any)["role"] != "assistant" {
		t.Fatalf("assistant message lost: %v", input[2])
	}
}

func TestReasoningEffortMapping(t *testing.T) {
	cases := map[string]string{
		`{"reasoning":{"effort":"none"}}`:    "low",
		`{"reasoning":{"effort":"minimal"}}`: "low",
		`{"reasoning":{"effort":"LOW"}}`:     "low",
		`{"reasoning":{"effort":"medium"}}`:  "medium",
		`{"reasoning":{"effort":"high"}}`:    "high",
		`{"reasoning":{"effort":"xhigh"}}`:   "xhigh",
		`{"reasoning":{"effort":"max"}}`:     "xhigh",
		`{"reasoning_effort":"x-high"}`:      "xhigh",
		`{"reasoning":{"effort":"weird"}}`:   "medium",
		`{}`:                                 "medium",
	}
	for raw, want := range cases {
		var source map[string]any
		_ = json.Unmarshal([]byte(raw), &source)
		if got := ReasoningEffort(source); got != want {
			t.Errorf("ReasoningEffort(%s) = %q, want %q", raw, got, want)
		}
	}
}

var uuidPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-5[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

func metadataOf(t *testing.T, body string) map[string]any {
	t.Helper()
	return build(t, body)["metadata"].(map[string]any)
}

func TestMetadataIsDeterministic(t *testing.T) {
	body := `{"model":"gpt-5.6-sol","input":[{"type":"message","role":"user","content":"first"}]}`
	a, b := metadataOf(t, body), metadataOf(t, body)
	if a["task_id"] != b["task_id"] || a["turn_id"] != b["turn_id"] {
		t.Fatal("same request must produce the same ids")
	}
	for _, key := range []string{"task_id", "turn_id"} {
		if !uuidPattern.MatchString(a[key].(string)) {
			t.Errorf("%s = %v is not a v5 UUID", key, a[key])
		}
	}
	if a["agent_iteration"] != "1" {
		t.Fatalf("agent_iteration = %v", a["agent_iteration"])
	}
}

func TestMetadataTaskStableAcrossTurns(t *testing.T) {
	turn1 := metadataOf(t, `{"model":"gpt-5.6-sol","input":[{"type":"message","role":"user","content":"first"}]}`)
	turn2 := metadataOf(t, `{"model":"gpt-5.6-sol","input":[
		{"type":"message","role":"user","content":"first"},
		{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok"}]},
		{"type":"message","role":"user","content":"second"}]}`)
	if turn1["task_id"] != turn2["task_id"] {
		t.Fatal("task_id must stay the same within a conversation")
	}
	if turn1["turn_id"] == turn2["turn_id"] {
		t.Fatal("a new user message must start a new turn")
	}
	other := metadataOf(t, `{"model":"gpt-5.6-sol","input":[{"type":"message","role":"user","content":"different"}]}`)
	if other["task_id"] == turn1["task_id"] {
		t.Fatal("different conversations must get different task_id")
	}
}

func TestMetadataPrefersCacheKeyAndClientValues(t *testing.T) {
	a := metadataOf(t, `{"model":"gpt-5.6-sol","prompt_cache_key":"k","input":"x"}`)
	b := metadataOf(t, `{"model":"gpt-5.6-sol","prompt_cache_key":"k","input":"y"}`)
	if a["task_id"] != b["task_id"] {
		t.Fatal("prompt_cache_key should define the conversation")
	}
	c := metadataOf(t, `{"model":"gpt-5.6-sol","client_metadata":{"session_id":"s"},"input":"x"}`)
	d := metadataOf(t, `{"model":"gpt-5.6-sol","client_metadata":{"session_id":"s"},"input":"y"}`)
	if c["task_id"] != d["task_id"] {
		t.Fatal("client_metadata.session_id should define the conversation")
	}
	explicit := metadataOf(t, `{"model":"gpt-5.6-sol","input":"x","metadata":{"task_id":"mine","agent_iteration":"7"}}`)
	if explicit["task_id"] != "mine" || explicit["agent_iteration"] != "7" {
		t.Fatalf("client-provided values must win: %v", explicit)
	}
}

func TestMetadataTruncation(t *testing.T) {
	long := strings.Repeat("界", 300) // 900 字节
	meta := metadataOf(t, `{"model":"gpt-5.6-sol","input":"x","metadata":{"`+strings.Repeat("k", 80)+`":"`+long+`"}}`)
	for key, value := range meta {
		if len(key) > 64 || len(value.(string)) > 512 {
			t.Fatalf("metadata not truncated: %d/%d", len(key), len(value.(string)))
		}
		if !strings.HasPrefix(key, "kkk") {
			continue
		}
		if !json.Valid([]byte(`"` + value.(string) + `"`)) {
			t.Fatal("truncation produced invalid UTF-8")
		}
	}
}

func TestUUID5KnownVector(t *testing.T) {
	// Python: uuid.uuid5(uuid.NAMESPACE_URL, "https://example.com")
	if got := uuid5("https://example.com"); got != "4fd35a71-71ef-5a55-a9d9-aa75c889a6d0" {
		t.Fatalf("uuid5 = %s", got)
	}
}

func TestTurnStateCountsToolRounds(t *testing.T) {
	var input []any
	_ = json.Unmarshal([]byte(`[
		{"role":"user","content":"x"},
		{"type":"function_call","call_id":"a"},{"type":"function_call_output","call_id":"a"},
		{"type":"function_call","call_id":"b"},{"type":"function_call_output","call_id":"b"},{"type":"function_call_output","call_id":"c"}]`), &input)
	if _, iteration := turnState(input); iteration != "3" {
		t.Fatalf("agent_iteration = %s, want 3", iteration)
	}
}
