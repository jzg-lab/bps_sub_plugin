package tracelog

import (
	"strings"
	"unicode"
)

// nudgeWords 是用户催模型接着干时常发的短消息（小写、去掉标点空白后比较）。
var nudgeWords = []string{
	"继续", "接着", "接着做", "继续做", "继续啊", "继续呀", "继续吧", "继续执行", "继续修改", "继续干",
	"怎么不动了", "咋不动了", "动啊", "干活", "然后呢", "还没完", "没完成", "卡住了", "停了",
	"continue", "goon", "keepgoing", "proceed", "next", "carryon", "resume",
}

// maxNudgeRunes 是催促消息的最大长度（去掉标点空白后）；更长的当成正常提问。
const maxNudgeRunes = 20

// IsNudge 判断一条用户消息是不是在催模型继续（"继续"、"？？？"、"continue" 之类）。
func IsNudge(text string) bool {
	text = strings.TrimSpace(text)
	if text == "" {
		return false
	}
	var core strings.Builder
	onlyMarks := true
	for _, r := range strings.ToLower(text) {
		switch {
		case r == '?' || r == '？' || r == '.' || r == '。' || r == '…' || r == '!' || r == '！':
			// 只有问号、省略号、感叹号：用户在表达"怎么没反应"
		case unicode.IsSpace(r) || unicode.IsPunct(r) || unicode.IsSymbol(r):
		default:
			onlyMarks = false
			core.WriteRune(r)
		}
	}
	if onlyMarks {
		return true
	}
	word := core.String()
	if len([]rune(word)) > maxNudgeRunes {
		return false
	}
	for _, nudge := range nudgeWords {
		if word == nudge || (len([]rune(nudge)) >= 2 && strings.HasPrefix(word, nudge) && len([]rune(word))-len([]rune(nudge)) <= 3) {
			return true
		}
	}
	return false
}

// LastUserText 返回 Codex 请求体里最后一条用户消息的文字；最后一条 user 之后如果还有工具结果，
// 说明这是模型自己的后续轮次（不是用户刚发的），返回空串。
func LastUserText(body map[string]any) string {
	if text, ok := body["input"].(string); ok {
		return text
	}
	items, _ := body["input"].([]any)
	for i := len(items) - 1; i >= 0; i-- {
		item, ok := items[i].(map[string]any)
		if !ok {
			continue
		}
		kind, _ := item["type"].(string)
		if strings.HasSuffix(kind, "_output") {
			return ""
		}
		if role, _ := item["role"].(string); role != "user" {
			continue
		}
		switch content := item["content"].(type) {
		case string:
			return content
		case []any:
			var b strings.Builder
			for _, raw := range content {
				if part, ok := raw.(map[string]any); ok {
					if text, ok := part["text"].(string); ok {
						b.WriteString(text)
					}
				}
			}
			return b.String()
		}
		return ""
	}
	return ""
}
