package responses

import (
	"encoding/json"
	"strings"
)

// ErrorToResponses 把 Chat 错误响应体规整为 Responses 错误形状
// {"error":{message,type,code,param}}，兼容标准 OpenAI 形式与最小 {message} 形式。
func ErrorToResponses(body []byte) map[string]any {
	var parsed map[string]any
	if err := json.Unmarshal(body, &parsed); err == nil {
		if errObj, ok := parsed["error"].(map[string]any); ok {
			return responsesError(
				firstNonEmpty(rawString(errObj, "message"), "upstream returned an error"),
				firstNonEmpty(rawString(errObj, "type"), "upstream_error"),
				errObj["code"], errObj["param"],
			)
		}
		if msg := firstNonEmpty(rawString(parsed, "message"), rawString(parsed, "detail")); msg != "" {
			return responsesError(msg, "upstream_error", nil, nil)
		}
	}
	msg := strings.TrimSpace(string(body))
	if msg == "" {
		msg = "upstream returned an error"
	}
	return responsesError(msg, "upstream_error", nil, nil)
}

// responsesError 构造 Responses 错误对象。
func responsesError(message, typ string, code, param any) map[string]any {
	return map[string]any{"error": map[string]any{
		"message": message,
		"type":    typ,
		"code":    code,
		"param":   param,
	}}
}

// ErrorMessage 返回 Responses 错误对象的 message（供 server 层日志）。
func ErrorMessage(resp map[string]any) string {
	if errObj, ok := resp["error"].(map[string]any); ok {
		return rawString(errObj, "message")
	}
	return ""
}
