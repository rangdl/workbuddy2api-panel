package responses

import "strings"

const (
	thinkOpenTag  = "\x3cthink\x3e"
	thinkCloseTag = "\x3c/think\x3e"
)

// extractReasoningFieldText 穷举上游 reasoning 回传字段，优先级：
// reasoning_content > reasoning(字符串/对象) > reasoning_details。
// 不依赖供应商声明，对各家 Chat 兼容接口都能兜底提取。
func extractReasoningFieldText(value map[string]any) string {
	for _, key := range []string{"reasoning_content", "reasoning"} {
		if s, ok := value[key].(string); ok && s != "" {
			return s
		}
	}
	if reasoning, ok := value["reasoning"].(map[string]any); ok {
		for _, key := range []string{"content", "text", "summary"} {
			if s, ok := reasoning[key].(string); ok && s != "" {
				return s
			}
		}
	}
	if details, ok := value["reasoning_details"]; ok {
		if text := extractReasoningDetailsText(details); text != "" {
			return text
		}
	}
	return ""
}

// extractReasoningDetailsText 处理 reasoning_details 的 string/array/object 三态。
func extractReasoningDetailsText(value any) string {
	switch v := value.(type) {
	case string:
		return v
	case []any:
		parts := make([]string, 0, len(v))
		for _, raw := range v {
			if text := extractReasoningDetailPartText(raw); text != "" {
				parts = append(parts, text)
			}
		}
		return strings.Join(parts, "\n\n")
	case map[string]any:
		return extractReasoningDetailPartText(v)
	default:
		return ""
	}
}

// extractReasoningDetailPartText 从单个 detail part 提取文本。
func extractReasoningDetailPartText(value any) string {
	obj, ok := value.(map[string]any)
	if !ok {
		return ""
	}
	for _, key := range []string{"text", "content", "summary"} {
		if s, ok := obj[key].(string); ok && s != "" {
			return s
		}
	}
	if parts, ok := obj["parts"].([]any); ok {
		texts := make([]string, 0, len(parts))
		for _, raw := range parts {
			if text := extractReasoningDetailPartText(raw); text != "" {
				texts = append(texts, text)
			}
		}
		return strings.Join(texts, "\n\n")
	}
	return ""
}

// extractReasoningSummaryText 提取 reasoning item 的 summary 文本
// （string / []part / object 三态）。
func extractReasoningSummaryText(summary any) string {
	switch v := summary.(type) {
	case string:
		return v
	case []any:
		texts := make([]string, 0, len(v))
		for _, raw := range v {
			if obj, ok := raw.(map[string]any); ok {
				if text := firstNonEmpty(rawString(obj, "text"), rawString(obj, "content")); text != "" {
					texts = append(texts, text)
				}
			} else if s, ok := raw.(string); ok && s != "" {
				texts = append(texts, s)
			}
		}
		return strings.Join(texts, "\n\n")
	case map[string]any:
		return firstNonEmpty(rawString(v, "text"), rawString(v, "content"))
	default:
		return ""
	}
}

// splitLeadingThinkBlock 拆分文本开头的  thinking...<｜end▁of▁thinking｜> 块。
// 返回 (reasoning, answer, found)；无 think 块时 found=false。
func splitLeadingThinkBlock(text string) (string, string, bool) {
	afterWS := strings.TrimLeft(text, " \t\r\n")
	if !strings.HasPrefix(afterWS, thinkOpenTag) {
		return "", text, false
	}
	body := afterWS[len(thinkOpenTag):]
	closeIdx := strings.Index(body, thinkCloseTag)
	if closeIdx < 0 {
		return "", text, false
	}
	reasoning := strings.TrimSpace(body[:closeIdx])
	answer := strings.TrimLeft(body[closeIdx+len(thinkCloseTag):], " \t\r\n")
	return reasoning, answer, true
}
