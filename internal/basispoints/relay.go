package basispoints

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
)

// maxItemIDLen 是 Responses item id 的长度上限。
const maxItemIDLen = 64

// NativeToolCall 是上游返回的一个原生调用（可能是 run_officejs 中转，或 update_plan 之类）。
type NativeToolCall struct {
	Type      string // "function_call" 或 "custom_tool_call"
	Name      string
	CallID    string
	ItemID    string
	Arguments string // function_call 的 arguments（字符串化 JSON）
	Input     string // custom_tool_call 的 input
	Raw       map[string]any
}

// ClientToolCall 是还原成客户端声明形态后的调用，直接发给宿主/写进历史。
type ClientToolCall struct {
	Type      string         // "function_call" / "custom_tool_call"
	ID        string         // item id
	CallID    string         // 与工具结果关联
	Name      string         // 客户端工具本名
	Namespace string         // 可空
	Arguments string         // function_call
	Input     string         // custom_tool_call
	Native    map[string]any // 对应的原生调用项（存 KV 供回放）
}

// asMap 把还原后的调用转成 output item（用于合成 SSE / 非流式）。
func (c ClientToolCall) asMap() map[string]any {
	m := map[string]any{"type": c.Type, "id": c.ID, "call_id": c.CallID, "name": c.Name}
	if c.Type == "custom_tool_call" {
		m["input"] = c.Input
	} else {
		m["arguments"] = c.Arguments
	}
	if c.Namespace != "" {
		m["namespace"] = c.Namespace
	}
	return m
}

// functionItemID 返回 fc_<call_id>，超长则哈希。
func functionItemID(callID string) string {
	candidate := "fc_" + callID
	if len(candidate) <= maxItemIDLen {
		return candidate
	}
	sum := sha256.Sum256([]byte(callID))
	return "fc_" + hex.EncodeToString(sum[:])[:maxItemIDLen-3]
}

// parseNativeCall 从 output item map 读出原生调用。
func parseNativeCall(item map[string]any) (NativeToolCall, bool) {
	kind, _ := item["type"].(string)
	if kind != "function_call" && kind != "custom_tool_call" {
		return NativeToolCall{}, false
	}
	call := NativeToolCall{
		Type:   kind,
		Name:   stringField(item, "name"),
		CallID: stringField(item, "call_id"),
		ItemID: stringField(item, "id"),
		Raw:    item,
	}
	call.Arguments, _ = item["arguments"].(string)
	call.Input, _ = item["input"].(string)
	return call, true
}

// RestoreClientCall 把一个原生调用还原成客户端声明的调用。
// 无法还原（不是本代理认识的中转/工具、schema 不符、JSON 坏）返回 ok=false。
func RestoreClientCall(native NativeToolCall, catalog *ToolCatalog) (ClientToolCall, bool) {
	envelope, isTransport := transportEnvelope(native)
	if isTransportName(native.Name) && envelope == nil {
		return ClientToolCall{}, false // 声称是中转但解不出内层
	}

	var innerName string
	if envelope != nil {
		innerName, _ = envelope["name"].(string)
	} else {
		innerName = native.Name
	}
	spec, ok := catalog.lookup(strings.TrimSpace(innerName))
	if !ok {
		return ClientToolCall{}, false
	}

	callID := native.CallID
	if callID == "" {
		callID = fallbackCallIDPrefix + shortHash(native.ItemID+innerName)
	}

	if spec.Type == "function" {
		var arguments map[string]any
		if envelope != nil {
			arguments = coerceObject(envelope["arguments"])
		} else {
			if native.Type != "function_call" {
				return ClientToolCall{}, false
			}
			arguments = decodeObject(native.Arguments)
			arguments = restoreNativeFunctionArguments(spec.Name, arguments)
		}
		if arguments == nil {
			return ClientToolCall{}, false
		}
		if !valueMatchesSchema(arguments, toolParameters(spec)) {
			return ClientToolCall{}, false
		}
		encoded, err := json.Marshal(arguments)
		if err != nil {
			return ClientToolCall{}, false
		}
		itemID := native.ItemID
		if isTransport || itemID == "" {
			itemID = functionItemID(callID)
		}
		return ClientToolCall{
			Type: "function_call", ID: itemID, CallID: callID, Name: spec.Name,
			Namespace: spec.Namespace, Arguments: string(encoded), Native: native.Raw,
		}, true
	}

	// custom
	var input string
	if envelope != nil {
		input, ok = envelope["input"].(string)
		if !ok {
			return ClientToolCall{}, false
		}
	} else {
		if native.Type != "custom_tool_call" {
			return ClientToolCall{}, false
		}
		input = native.Input
	}
	itemID := native.ItemID
	if isTransport || itemID == "" {
		itemID = "ctc_" + callID
	}
	return ClientToolCall{
		Type: "custom_tool_call", ID: itemID, CallID: callID, Name: spec.Name,
		Namespace: spec.Namespace, Input: input, Native: native.Raw,
	}, true
}

// transportEnvelope 解析 run_officejs 的 code 字段，返回内层 {name, arguments/input}。
// 第二个返回值表示这个原生调用是不是 run_officejs 中转。
func transportEnvelope(native NativeToolCall) (map[string]any, bool) {
	if native.Type != "function_call" || !isTransportName(native.Name) {
		return nil, false
	}
	arguments := decodeObject(native.Arguments)
	if arguments == nil {
		return nil, true
	}
	envelope := decodeTransportCode(arguments["code"])
	// 最多剥两层嵌套的 run_officejs（模型偶尔会套娃）。
	for i := 0; i < 2; i++ {
		if envelope == nil || !isTransportName(envelope["name"]) {
			break
		}
		nested := coerceObject(envelope["arguments"])
		if nested == nil {
			return nil, true
		}
		envelope = decodeTransportCode(nested["code"])
	}
	if envelope != nil && isTransportName(envelope["name"]) {
		return nil, true // 还是中转名，放弃
	}
	return envelope, true
}

// decodeTransportCode 把 code 解成对象：可能是对象、字符串化 JSON、或夹在文本里的 JSON。
func decodeTransportCode(code any) map[string]any {
	if obj, ok := code.(map[string]any); ok {
		return obj
	}
	text, ok := code.(string)
	if !ok {
		return nil
	}
	candidates := []string{text}
	if repaired := repairInvalidJSONBackslashes(text); repaired != text {
		candidates = append(candidates, repaired)
	}
	for _, candidate := range candidates {
		if obj := decodeObject(candidate); obj != nil {
			return obj
		}
		if obj := firstJSONObject(candidate); obj != nil {
			return obj
		}
	}
	return nil
}

// decodeObject 把字符串解成 JSON 对象；失败返回 nil。
func decodeObject(raw string) map[string]any {
	trimmed := strings.TrimSpace(raw)
	if !strings.HasPrefix(trimmed, "{") {
		return nil
	}
	var obj map[string]any
	if json.Unmarshal([]byte(trimmed), &obj) != nil {
		return nil
	}
	return obj
}

// coerceObject 把 any 变成对象：已经是 map 直接用，是字符串就解码。
func coerceObject(value any) map[string]any {
	switch typed := value.(type) {
	case map[string]any:
		return typed
	case string:
		return decodeObject(typed)
	default:
		return nil
	}
}

// firstJSONObject 从文本里找出第一个完整 JSON 对象（模型有时用代码块包起来）。
func firstJSONObject(text string) map[string]any {
	for i := 0; i < len(text); i++ {
		if text[i] != '{' {
			continue
		}
		decoder := json.NewDecoder(strings.NewReader(text[i:]))
		var obj map[string]any
		if decoder.Decode(&obj) == nil && obj != nil {
			return obj
		}
	}
	return nil
}

var jsonEscapeChars = map[byte]bool{'"': true, '\\': true, '/': true, 'b': true, 'f': true, 'n': true, 'r': true, 't': true}

// repairInvalidJSONBackslashes 把字符串值里的非法反斜杠翻倍（shell 命令里的 \( 之类）。
func repairInvalidJSONBackslashes(text string) string {
	var out strings.Builder
	inString := false
	for i := 0; i < len(text); i++ {
		c := text[i]
		if !inString {
			out.WriteByte(c)
			if c == '"' {
				inString = true
			}
			continue
		}
		if c == '"' {
			out.WriteByte(c)
			inString = false
			continue
		}
		if c != '\\' {
			out.WriteByte(c)
			continue
		}
		var next byte
		if i+1 < len(text) {
			next = text[i+1]
		}
		valid := jsonEscapeChars[next]
		if next == 'u' {
			valid = i+5 < len(text) && isHex(text[i+2:i+6])
		}
		if valid {
			out.WriteByte(c)
			out.WriteByte(next)
			i++
		} else {
			out.WriteByte(c)
			out.WriteByte(c)
		}
	}
	return out.String()
}

func isHex(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F') {
			return false
		}
	}
	return len(s) == 4
}

func shortHash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])[:32]
}
