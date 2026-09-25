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

func TestUpdatePlanRestore(t *testing.T) {
	catalog := ParseTools(toolSource(t, `{"tools":[{"type":"function","name":"update_plan","parameters":{"type":"object","properties":{"plan":{"type":"array"}}}}]}`))
	native := nativeFunctionCall("update_plan", `{"explanation":"go","plan":[{"step":"a","status":"done"},{"step":"b","status":"doing"}]}`)
	call, ok := RestoreClientCall(native, catalog)
	if !ok {
		t.Fatal("update_plan should restore")
	}
	var args map[string]any
	_ = json.Unmarshal([]byte(call.Arguments), &args)
	plan := args["plan"].([]any)
	first := plan[0].(map[string]any)
	if first["description"] != "a" || first["status"] != "completed" || first["id"] != "step1" {
		t.Fatalf("plan not converted to native schema: %v", first)
	}
	if args["summary"] != "go" {
		t.Fatalf("summary wrong: %v", args["summary"])
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
