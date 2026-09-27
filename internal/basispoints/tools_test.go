package basispoints

import (
	"encoding/json"
	"strings"
	"testing"
)

func toolSource(t *testing.T, raw string) map[string]any {
	t.Helper()
	var source map[string]any
	if err := json.Unmarshal([]byte(raw), &source); err != nil {
		t.Fatal(err)
	}
	return source
}

func TestParseToolsAndCatalog(t *testing.T) {
	source := toolSource(t, `{"tools":[
		{"type":"function","name":"exec_command","description":"run","parameters":{"type":"object","properties":{"cmd":{"type":"string"}},"required":["cmd"]}},
		{"type":"custom","name":"apply_patch","format":{"type":"grammar"}},
		{"type":"namespace","name":"mcp","tools":[{"type":"function","name":"search","parameters":{"type":"object"}}]}
	]}`)
	catalog := ParseTools(source)
	if catalog.Empty() {
		t.Fatal("catalog should not be empty")
	}
	if _, ok := catalog.lookup("exec_command"); !ok {
		t.Fatal("exec_command missing")
	}
	if spec, ok := catalog.lookup("mcp.search"); !ok || spec.Namespace != "mcp" || spec.Name != "search" {
		t.Fatalf("namespaced tool wrong: %+v", spec)
	}
	messages := BuildToolCatalogMessages(catalog, true)
	if len(messages) != 2 {
		t.Fatalf("expected catalog + reminder, got %d", len(messages))
	}
	instructions := messages[0].(map[string]any)["content"].([]any)[0].(map[string]any)["text"].(string)
	if !strings.Contains(instructions, "run_officejs") || !strings.Contains(instructions, "exec_command") {
		t.Fatal("catalog instructions missing key content")
	}
	reminder := messages[1].(map[string]any)["content"].([]any)[0].(map[string]any)["text"].(string)
	if !strings.Contains(reminder, "apply_patch") || !strings.Contains(reminder, "input") {
		t.Fatalf("reminder should mention custom tool: %s", reminder)
	}
}

func TestParseToolsNoneAndEmpty(t *testing.T) {
	if !ParseTools(toolSource(t, `{"tools":[{"type":"function","name":"x"}],"tool_choice":"none"}`)).Empty() {
		t.Fatal("tool_choice none must yield empty catalog")
	}
	messages := BuildToolCatalogMessages(ParseTools(toolSource(t, `{}`)), true)
	if len(messages) != 1 {
		t.Fatalf("no tools should give the single external-client notice, got %d", len(messages))
	}
}

func nativeFunctionCall(name, arguments string) NativeToolCall {
	return NativeToolCall{Type: "function_call", Name: name, CallID: "call_1", ItemID: "fc_up", Arguments: arguments,
		Raw: map[string]any{"type": "function_call", "name": name, "call_id": "call_1", "arguments": arguments}}
}

func execCatalog(t *testing.T) *ToolCatalog {
	return ParseTools(toolSource(t, `{"tools":[
		{"type":"function","name":"exec_command","parameters":{"type":"object","properties":{"cmd":{"type":"string"}},"required":["cmd"]}},
		{"type":"custom","name":"apply_patch"}]}`))
}

func TestRestoreClientCallFromTransport(t *testing.T) {
	native := nativeFunctionCall(transportName, `{"summary":"s","code":"{\"name\":\"exec_command\",\"arguments\":{\"cmd\":\"pwd\"}}","destructive":false,"references":[]}`)
	call, ok := RestoreClientCall(native, execCatalog(t))
	if !ok {
		t.Fatal("should restore transport call")
	}
	if call.Type != "function_call" || call.Name != "exec_command" || call.CallID != "call_1" {
		t.Fatalf("restored call wrong: %+v", call)
	}
	var args map[string]any
	if err := json.Unmarshal([]byte(call.Arguments), &args); err != nil || args["cmd"] != "pwd" {
		t.Fatalf("arguments wrong: %s", call.Arguments)
	}
	if call.ID != "fc_call_1" {
		t.Fatalf("transport call must use fc_<call_id>, got %s", call.ID)
	}
}

func TestRestoreCustomToolFromTransport(t *testing.T) {
	native := nativeFunctionCall(transportName, `{"code":"{\"name\":\"apply_patch\",\"input\":\"*** patch ***\"}"}`)
	call, ok := RestoreClientCall(native, execCatalog(t))
	if !ok || call.Type != "custom_tool_call" || call.Input != "*** patch ***" || call.ID != "ctc_call_1" {
		t.Fatalf("custom restore wrong: %+v ok=%v", call, ok)
	}
}

func TestRestoreRepairsBackslashes(t *testing.T) {
	// inner shell command has an invalid JSON escape \( that must be repaired
	native := nativeFunctionCall(transportName, `{"code":"{\"name\":\"exec_command\",\"arguments\":{\"cmd\":\"grep \\(x\"}}"}`)
	call, ok := RestoreClientCall(native, execCatalog(t))
	if !ok {
		t.Fatal("should repair and restore")
	}
	var args map[string]any
	_ = json.Unmarshal([]byte(call.Arguments), &args)
	if !strings.Contains(args["cmd"].(string), "grep") {
		t.Fatalf("cmd lost: %v", args)
	}
}

func TestRestoreRejectsBadCalls(t *testing.T) {
	catalog := execCatalog(t)
	cases := map[string]NativeToolCall{
		"transport no code":  nativeFunctionCall(transportName, `{"summary":"s"}`),
		"transport bad json": nativeFunctionCall(transportName, `not json`),
		"unknown inner tool": nativeFunctionCall(transportName, `{"code":"{\"name\":\"rm\",\"arguments\":{}}"}`),
		"schema mismatch":    nativeFunctionCall(transportName, `{"code":"{\"name\":\"exec_command\",\"arguments\":{\"cmd\":123}}"}`),
		"missing required":   nativeFunctionCall(transportName, `{"code":"{\"name\":\"exec_command\",\"arguments\":{}}"}`),
		"nested transport":   nativeFunctionCall(transportName, `{"code":"{\"name\":\"run_officejs\",\"arguments\":{\"code\":\"{}\"}}"}`),
	}
	for name, native := range cases {
		if _, ok := RestoreClientCall(native, catalog); ok {
			t.Errorf("%s: expected rejection", name)
		}
	}
}

func TestRestoreDirectNativeCall(t *testing.T) {
	// model called the client tool by name directly (no run_officejs wrapper)
	native := nativeFunctionCall("exec_command", `{"cmd":"ls"}`)
	call, ok := RestoreClientCall(native, execCatalog(t))
	if !ok || call.Name != "exec_command" || call.ID != "fc_up" {
		t.Fatalf("direct native call: %+v ok=%v", call, ok)
	}
}

func TestRestoreUnwrapsDoubleNested(t *testing.T) {
	inner := `{\"name\":\"exec_command\",\"arguments\":{\"cmd\":\"pwd\"}}`
	middle := `{\"name\":\"run_officejs\",\"arguments\":{\"code\":\"` + strings.ReplaceAll(inner, `\"`, `\\\"`) + `\"}}`
	native := nativeFunctionCall(transportName, `{"code":"`+middle+`"}`)
	call, ok := RestoreClientCall(native, execCatalog(t))
	if !ok || call.Name != "exec_command" {
		t.Fatalf("double nested unwrap: %+v ok=%v", call, ok)
	}
}

// codexPlanTool 是 Codex 声明的 update_plan（顶层 tools 形态）。
const codexPlanTool = `{"tools":[{"type":"function","name":"update_plan","parameters":{"type":"object","properties":{"explanation":{"type":"string"},"plan":{"type":"array","items":{"type":"object","properties":{"step":{"type":"string"},"status":{"type":"string","enum":["pending","in_progress","completed"]}},"required":["step","status"],"additionalProperties":false}}},"required":["plan"],"additionalProperties":false}}]}`

// nativePlanArgs 是 basispoints 自带 update_plan 的真实参数（2026-09-27 抓包）。
const nativePlanArgs = `{"summary":"规划并统计 Python 文件","plan":[{"id":"step1","description":"扫描 Python 文件","status":"in_progress","result":""},{"id":"step2","description":"统计行数","status":"pending","result":""},{"id":"step3","description":"汇报","status":"done","result":"x"}]}`

func TestUpdatePlanRestore(t *testing.T) {
	catalog := ParseTools(toolSource(t, codexPlanTool))
	call, ok := RestoreClientCall(nativeFunctionCall("update_plan", nativePlanArgs), catalog)
	if !ok {
		t.Fatal("native update_plan must restore into the client schema")
	}
	var args map[string]any
	_ = json.Unmarshal([]byte(call.Arguments), &args)
	plan := args["plan"].([]any)
	if len(plan) != 3 || args["explanation"] != "规划并统计 Python 文件" || args["summary"] != nil {
		t.Fatalf("plan not converted to client schema: %v", args)
	}
	if first := plan[0].(map[string]any); first["step"] != "扫描 Python 文件" || first["status"] != "in_progress" || len(first) != 2 {
		t.Fatalf("first step wrong: %v", first)
	}
	if last := plan[2].(map[string]any); last["status"] != "completed" {
		t.Fatalf("status alias not normalized: %v", last)
	}
}

func TestClientPlanArguments(t *testing.T) {
	client := `{"explanation":"go","plan":[{"step":"a","status":"pending"}]}`
	if got := ClientPlanArgumentsJSON(client); got != client {
		t.Fatalf("client-shaped arguments must pass through: %s", got)
	}
	if got := ClientPlanArgumentsJSON("not json"); got != "not json" {
		t.Fatalf("bad JSON must pass through: %s", got)
	}
	got := ClientPlanArguments(decodeObject(`{"summary":"","plan":[{"description":"a","status":"weird"},{"id":"x"}]}`))
	if _, has := got["explanation"]; has || len(got["plan"].([]any)) != 1 || got["plan"].([]any)[0].(map[string]any)["status"] != "pending" {
		t.Fatalf("got %v", got)
	}
}

// 原生工具模式下，Codex 回放的 update_plan（客户端格式）要转回 basispoints 原生格式，结果换成 {"status":"ok"}。
func TestAdditionalToolsReplaysUpdatePlanNatively(t *testing.T) {
	body := `{"model":"gpt-5.6-sol","input":[
		{"type":"additional_tools","role":"developer","tools":[{"type":"namespace","name":"functions","tools":[{"type":"custom","name":"exec"}]}]},
		{"type":"message","role":"user","content":"plan it"},
		{"type":"function_call","id":"fc_1","call_id":"call_p","name":"update_plan","arguments":"{\"explanation\":\"go\",\"plan\":[{\"step\":\"a\",\"status\":\"in_progress\"}]}"},
		{"type":"function_call_output","call_id":"call_p","output":"Plan updated"},
		{"type":"custom_tool_call","id":"ctc_1","call_id":"call_e","name":"exec","input":"text(1)"},
		{"type":"custom_tool_call_output","call_id":"call_e","output":"1"}]}`
	input := build(t, body)["input"].([]any)
	call := input[2].(map[string]any)
	var args map[string]any
	_ = json.Unmarshal([]byte(call["arguments"].(string)), &args)
	step := args["plan"].([]any)[0].(map[string]any)
	if args["summary"] != "go" || step["description"] != "a" || step["id"] != "step1" {
		t.Fatalf("update_plan not converted back to native schema: %v", args)
	}
	if output := input[3].(map[string]any); output["output"] != `{"status":"ok"}` {
		t.Fatalf("update_plan output not normalized: %v", output)
	}
	if exec := input[4].(map[string]any); exec["input"] != "text(1)" || exec["id"] != "ctc_1" {
		t.Fatalf("other native calls must stay verbatim: %v", exec)
	}
	if output := input[5].(map[string]any); output["output"] != "1" {
		t.Fatalf("other outputs must stay verbatim: %v", output)
	}
}

func TestSchemaValidation(t *testing.T) {
	schema := map[string]any{"type": "object", "properties": map[string]any{
		"n": map[string]any{"type": "integer"}, "s": map[string]any{"type": "string"},
		"e": map[string]any{"enum": []any{"a", "b"}},
	}, "required": []any{"n"}}
	good := map[string]any{"n": float64(3), "s": "x", "e": "a"}
	if !valueMatchesSchema(good, schema) {
		t.Fatal("valid value rejected")
	}
	bad := []map[string]any{
		{"s": "x"},                  // missing required n
		{"n": "notint"},             // wrong type
		{"n": float64(1), "e": "z"}, // enum violation
		{"n": 1.5},                  // integer must be whole
	}
	for i, value := range bad {
		if valueMatchesSchema(value, schema) {
			t.Errorf("case %d: invalid value accepted: %v", i, value)
		}
	}
	if !valueMatchesSchema(map[string]any{"anything": 1}, map[string]any{}) {
		t.Fatal("empty schema must accept anything")
	}
}

func TestFunctionItemIDLength(t *testing.T) {
	short := functionItemID("call_1")
	if short != "fc_call_1" {
		t.Fatalf("short id = %s", short)
	}
	long := functionItemID(strings.Repeat("x", 200))
	if len(long) > maxItemIDLen || !strings.HasPrefix(long, "fc_") {
		t.Fatalf("long id not clamped: %s (%d)", long, len(long))
	}
}

// codexAdditionalToolsBody 是新版 Codex 的请求形态：没有顶层 tools，工具声明在 input 的
// additional_tools 里；第二轮起历史里是上游原生的 custom_tool_call（结构取自 2026-09-27 抓包）。
const codexAdditionalToolsBody = `{"model":"gpt-5.6-sol","stream":true,"tool_choice":"auto","parallel_tool_calls":false,
	"input":[
		{"type":"additional_tools","role":"developer","tools":[{"type":"namespace","name":"functions","description":"","tools":[
			{"type":"custom","name":"exec","description":"Run JavaScript","format":{"type":"grammar","syntax":"lark","definition":"start: SOURCE"}},
			{"type":"function","name":"wait","parameters":{"type":"object","properties":{"cell_id":{"type":"string"}}}}]}]},
		{"type":"message","role":"user","content":[{"type":"input_text","text":"读一下当前目录的文件"}]},
		{"type":"reasoning","id":"rs_1","summary":[],"encrypted_content":"ENC"},
		{"type":"custom_tool_call","id":"ctc_1","status":"completed","call_id":"call_1","name":"exec","input":"text(1)"},
		{"type":"custom_tool_call_output","id":"ctco_1","call_id":"call_1","output":[{"type":"input_text","text":"hello\n"}]}]}`

func TestAdditionalToolsPassThroughNatively(t *testing.T) {
	decision := Decide(enabledConfig(), identityHeader(), []byte(codexAdditionalToolsBody))
	if !decision.Route || !decision.NativeTools || decision.HasToolContext {
		t.Fatalf("route=%v native=%v toolctx=%v reason=%q", decision.Route, decision.NativeTools, decision.HasToolContext, decision.Reason)
	}
	out := build(t, codexAdditionalToolsBody)
	input := out["input"].([]any)
	if len(input) != 5 {
		t.Fatalf("input = %v", input)
	}
	if kind := input[0].(map[string]any)["type"]; kind != "additional_tools" {
		t.Fatalf("additional_tools must stay first (no injected instructions): %v", input[0])
	}
	raw, _ := json.Marshal(input)
	if strings.Contains(string(raw), "run_officejs") || strings.Contains(string(raw), "Return the answer as assistant text") {
		t.Fatalf("native tools must not be wrapped or suppressed: %s", raw)
	}
	call := input[3].(map[string]any)
	if call["type"] != "custom_tool_call" || call["name"] != "exec" || call["id"] != "ctc_1" || call["input"] != "text(1)" {
		t.Fatalf("native call not kept verbatim: %v", call)
	}
	if output := input[4].(map[string]any); output["type"] != "custom_tool_call_output" || output["id"] != "ctco_1" {
		t.Fatalf("native output not kept verbatim: %v", output)
	}
}

func TestAdditionalToolsRespectsToolRelaySwitch(t *testing.T) {
	decision := Decide(relayOffConfig(), identityHeader(), []byte(codexAdditionalToolsBody))
	if decision.Route || decision.Reason != ReasonHasTools {
		t.Fatalf("route=%v reason=%q", decision.Route, decision.Reason)
	}
}

func TestEmptyAdditionalToolsIsNotNative(t *testing.T) {
	decision := Decide(enabledConfig(), identityHeader(), []byte(`{"model":"gpt-5.6-sol","input":[{"type":"additional_tools","tools":[]},{"type":"message","role":"user","content":"hi"}]}`))
	if !decision.Route || decision.NativeTools {
		t.Fatalf("route=%v native=%v", decision.Route, decision.NativeTools)
	}
}
