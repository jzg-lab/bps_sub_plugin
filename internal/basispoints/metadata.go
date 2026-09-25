package basispoints

import (
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// metadataNamespace 用来派生 task_id / turn_id，改了会让进行中的会话换一个身份。
const metadataNamespace = "bps_sub_plugin/"

// namespaceURL 是 RFC 4122 的 URL 命名空间。
var namespaceURL = [16]byte{0x6b, 0xa7, 0xb8, 0x11, 0x9d, 0xad, 0x11, 0xd1, 0x80, 0xb4, 0x00, 0xc0, 0x4f, 0xd4, 0x30, 0xc8}

// buildMetadata 生成 Excel 插件的 metadata：客户端的标量字段原样保留（截断长度），
// 再补 agent_iteration、task_id、turn_id。标识都是**确定性派生**的：同一会话 task_id
// 不变，同一轮（同一条用户消息）turn_id 不变，重试时上游能识别为同一轮。
func buildMetadata(source map[string]any, history []any, rawInput any) map[string]string {
	metadata := map[string]string{}
	if raw, ok := source["metadata"].(map[string]any); ok {
		for key, value := range raw {
			text, ok := scalarString(value)
			if !ok {
				continue
			}
			metadata[truncate(key, 64)] = truncate(text, 512)
		}
	}
	conversation := conversationKey(source, history)
	fingerprint, iteration := turnState(rawInput)
	setDefault(metadata, "agent_iteration", iteration)
	setDefault(metadata, "task_id", uuid5(metadataNamespace+conversation))
	setDefault(metadata, "turn_id", uuid5(metadataNamespace+conversation+"/turn/"+fingerprint))
	return metadata
}

// conversationKey 依次取 prompt_cache_key、client_metadata.session_id、第一条历史的哈希。
// 第一条历史是会话的根，追加新轮次时不会变。
func conversationKey(source map[string]any, history []any) string {
	if key, ok := source["prompt_cache_key"].(string); ok && strings.TrimSpace(key) != "" {
		return strings.TrimSpace(key)
	}
	if clientMetadata, ok := source["client_metadata"].(map[string]any); ok {
		for _, name := range []string{"session_id", "sessionId"} {
			if value, ok := clientMetadata[name].(string); ok && strings.TrimSpace(value) != "" {
				return strings.TrimSpace(value)
			}
		}
	}
	for _, item := range history {
		if _, ok := item.(map[string]any); ok {
			return hashJSON(item)
		}
	}
	return "anonymous"
}

// turnState 返回本轮指纹（截至最后一条用户消息的输入前缀的哈希）和 agent_iteration
// （最后一条用户消息之后工具结果的轮数 + 1；本阶段不带工具，恒为 1）。
func turnState(rawInput any) (string, string) {
	if text, ok := rawInput.(string); ok {
		return hashJSON(text), "1"
	}
	items, ok := rawInput.([]any)
	if !ok || len(items) == 0 {
		return "anonymous", "1"
	}
	lastUser := -1
	for index, raw := range items {
		if item, ok := raw.(map[string]any); ok {
			if role, _ := item["role"].(string); strings.EqualFold(role, "user") {
				lastUser = index
			}
		}
	}
	prefix := items[:1]
	if lastUser >= 0 {
		prefix = items[:lastUser+1]
	}
	rounds := 0
	inResults := false
	for _, raw := range items[lastUser+1:] {
		isResult := false
		if item, ok := raw.(map[string]any); ok {
			kind, _ := item["type"].(string)
			isResult = kind == "function_call_output" || kind == "custom_tool_call_output"
		}
		if isResult && !inResults {
			rounds++
		}
		inResults = isResult
	}
	return hashJSON(prefix), strconv.Itoa(rounds + 1)
}

// hashJSON 对 JSON 序列化结果取 SHA-256。encoding/json 对 map 的键排序，结果稳定。
func hashJSON(value any) string {
	data, err := json.Marshal(value)
	if err != nil {
		data = []byte(fmt.Sprint(value))
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// uuid5 按 RFC 4122 在 URL 命名空间下生成版本 5 UUID。
func uuid5(name string) string {
	hasher := sha1.New()
	hasher.Write(namespaceURL[:])
	hasher.Write([]byte(name))
	sum := hasher.Sum(nil)
	sum[6] = (sum[6] & 0x0f) | 0x50
	sum[8] = (sum[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", sum[0:4], sum[4:6], sum[6:8], sum[8:10], sum[10:16])
}

func scalarString(value any) (string, bool) {
	switch typed := value.(type) {
	case string:
		return typed, true
	case json.Number:
		return typed.String(), true
	case float64:
		return strconv.FormatFloat(typed, 'f', -1, 64), true
	case bool:
		return strconv.FormatBool(typed), true
	default:
		return "", false
	}
}

func truncate(text string, limit int) string {
	if len(text) <= limit {
		return text
	}
	// 按字节截断后去掉不完整的 UTF-8 尾巴。
	return strings.ToValidUTF8(text[:limit], "")
}

func setDefault(values map[string]string, key, value string) {
	if _, ok := values[key]; !ok {
		values[key] = value
	}
}
