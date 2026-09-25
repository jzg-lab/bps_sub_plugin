package basispoints

import (
	"encoding/json"
	"strconv"
	"strings"
)

// valueMatchesSchema 做一层轻量 JSON Schema 校验，判断还原出的参数是否符合工具声明。
// 目的是拦掉明显不对的调用（模型把 schema 写错），不追求完整实现。空 schema 一律放行。
func valueMatchesSchema(value any, schema any) bool {
	obj, ok := schema.(map[string]any)
	if !ok || len(obj) == 0 {
		return true
	}
	if types, ok := obj["type"].([]any); ok {
		for _, candidate := range types {
			merged := map[string]any{}
			for k, v := range obj {
				merged[k] = v
			}
			merged["type"] = candidate
			if valueMatchesSchema(value, merged) {
				return matchesEnum(value, obj)
			}
		}
		return false
	}
	expected, _ := obj["type"].(string)
	switch expected {
	case "object":
		m, ok := value.(map[string]any)
		if !ok {
			return false
		}
		if required, ok := obj["required"].([]any); ok {
			for _, r := range required {
				if key, ok := r.(string); ok {
					if _, present := m[key]; !present {
						return false
					}
				}
			}
		}
		properties, _ := obj["properties"].(map[string]any)
		if properties != nil {
			if obj["additionalProperties"] == false {
				for key := range m {
					if _, ok := properties[key]; !ok {
						return false
					}
				}
			}
			for key, nested := range m {
				if nestedSchema, ok := properties[key]; ok {
					if !valueMatchesSchema(nested, nestedSchema) {
						return false
					}
				}
			}
		}
	case "array":
		arr, ok := value.([]any)
		if !ok {
			return false
		}
		if items, ok := obj["items"]; ok {
			for _, elem := range arr {
				if !valueMatchesSchema(elem, items) {
					return false
				}
			}
		}
	case "string":
		if _, ok := value.(string); !ok {
			return false
		}
	case "integer":
		if !isJSONInteger(value) {
			return false
		}
	case "number":
		if !isJSONNumber(value) {
			return false
		}
	case "boolean":
		if _, ok := value.(bool); !ok {
			return false
		}
	case "null":
		if value != nil {
			return false
		}
	}
	return matchesEnum(value, obj)
}

func matchesEnum(value any, schema map[string]any) bool {
	enum, ok := schema["enum"].([]any)
	if !ok {
		return true
	}
	target, _ := json.Marshal(value)
	for _, candidate := range enum {
		if other, _ := json.Marshal(candidate); string(other) == string(target) {
			return true
		}
	}
	return false
}

func isJSONNumber(value any) bool {
	switch typed := value.(type) {
	case float64:
		return true
	case json.Number:
		_, err := typed.Float64()
		return err == nil
	default:
		return false
	}
}

func isJSONInteger(value any) bool {
	switch typed := value.(type) {
	case float64:
		return typed == float64(int64(typed))
	case json.Number:
		if _, err := typed.Int64(); err == nil {
			return true
		}
		if f, err := typed.Float64(); err == nil {
			return f == float64(int64(f))
		}
		return false
	default:
		return false
	}
}

// planStatusAliases 把各种计划状态别名归一到 basispoints 认识的三种。
var planStatusAliases = map[string]string{
	"pending": "pending", "not_started": "pending", "todo": "pending", "planned": "pending",
	"queued": "pending", "blocked": "pending",
	"in_progress": "in_progress", "active": "in_progress", "started": "in_progress",
	"doing": "in_progress", "current": "in_progress",
	"completed": "completed", "complete": "completed", "done": "completed", "finished": "completed",
}

// restoreNativeFunctionArguments 把 update_plan 的 Codex 参数转回 basispoints 原生 schema。
// 其它工具原样返回。
func restoreNativeFunctionArguments(name string, arguments map[string]any) map[string]any {
	if name != "update_plan" || arguments == nil {
		return arguments
	}
	rawPlan, ok := arguments["plan"].([]any)
	if !ok {
		return arguments
	}
	nativePlan := make([]any, 0, len(rawPlan))
	for i, raw := range rawPlan {
		item, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		step, _ := item["step"].(string)
		status, _ := item["status"].(string)
		if step == "" || status == "" {
			continue
		}
		nativePlan = append(nativePlan, map[string]any{
			"id": "step" + strconv.Itoa(i+1), "description": step, "status": normalizePlanStatus(status), "result": "",
		})
	}
	summary, _ := arguments["explanation"].(string)
	if summary == "" {
		summary = "Update task plan"
	}
	return map[string]any{"summary": summary, "plan": nativePlan}
}

// normalizePlanStatus 归一化计划状态别名。
func normalizePlanStatus(status string) string {
	key := strings.ReplaceAll(strings.ReplaceAll(strings.ToLower(strings.TrimSpace(status)), "-", "_"), " ", "_")
	if mapped, ok := planStatusAliases[key]; ok {
		return mapped
	}
	return status
}
