package anthropic

import (
	"encoding/json"
	"strings"
)

// ErrorToMessages 把上游（Chat 形态）错误响应体转换为 Anthropic 错误形状。
//
// Anthropic 错误规范：
//
//	{
//	  "type": "error",
//	  "error": {"type": "invalid_request_error", "message": "..."}
//	}
//
// error.type 取值：invalid_request_error / authentication_error / permission_error /
// not_found_error / request_too_large / rate_limit_error / api_error / overloaded_error。
func ErrorToMessages(body []byte) map[string]any {
	var upstream map[string]any
	if json.Unmarshal(body, &upstream) != nil {
		return errorMessage("api_error", string(body))
	}
	// 上游已是 OpenAI 错误形状 {"error":{...}} → 提取后重包。
	if errObj, ok := upstream["error"].(map[string]any); ok {
		msg := rawString(errObj, "message")
		typ := rawString(errObj, "type")
		// chatCompletions 的错误透传路径会把上游原文 JSON 序列化后塞进 message
		// （string(body)）。message 本身可解析出 {"error":{...}} 时解包一层，
		// 取内层的 message/type（保留最原始的错误语义）。
		if strings.TrimSpace(msg) != "" && strings.HasPrefix(strings.TrimSpace(msg), "{") {
			var nested map[string]any
			if json.Unmarshal([]byte(msg), &nested) == nil {
				if inner, ok := nested["error"].(map[string]any); ok {
					if im := rawString(inner, "message"); im != "" {
						msg = im
					}
					if it := rawString(inner, "type"); it != "" {
						typ = it
					}
				}
			}
		}
		if msg == "" {
			msg = "upstream error"
		}
		return errorMessage(openAIErrorTypeToAnthropic(strVal(errObj["code"]), typ), msg)
	}
	return errorMessage("api_error", string(body))
}

// openAIErrorTypeToAnthropic 把 OpenAI 错误 code/type 粗映射到 Anthropic 错误类。
func openAIErrorTypeToAnthropic(code, typ string) string {
	switch typ {
	case "invalid_request_error":
		return "invalid_request_error"
	case "authentication_error":
		return "authentication_error"
	case "permission_error":
		return "permission_error"
	case "rate_limit_error":
		return "rate_limit_error"
	case "api_error", "server_error":
		return "api_error"
	}
	switch code {
	case "invalid_request", "not_found", "model_not_found":
		return "invalid_request_error"
	case "authentication", "unauthorized":
		return "authentication_error"
	case "rate_limit":
		return "rate_limit_error"
	}
	return "api_error"
}

// errorMessage 构造 Anthropic 错误体。
func errorMessage(typ, msg string) map[string]any {
	return map[string]any{
		"type":  "error",
		"error": map[string]any{"type": typ, "message": msg},
	}
}

// ErrorMessage 从 Anthropic 错误体提取 message（供日志与聚合路径复用）。
func ErrorMessage(resp map[string]any) string {
	if errObj, ok := resp["error"].(map[string]any); ok {
		return rawString(errObj, "message")
	}
	return ""
}
