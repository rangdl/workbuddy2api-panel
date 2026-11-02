package responses

import (
	"encoding/json"
	"strings"
)

// ErrorToResponses 把 Chat 错误响应体规整为 Responses 错误形状
// {"error":{message,type,code,param}}（对齐参考 chat_error_to_response_error）。
//
// 兼容：
//  1. 标准 OpenAI 形式 {"error":{message,type,code,param}}；
//  2. MiniMax 等非标 {"base_resp":{status_code,status_msg}}；
//  3. 顶层只有 message/detail/status_msg 或裸字符串的最小错误。
func ErrorToResponses(body []byte) map[string]any {
	var parsed map[string]any
	if err := json.Unmarshal(body, &parsed); err != nil {
		msg := strings.TrimSpace(string(body))
		if msg == "" {
			msg = "upstream returned an error"
		}
		return responsesError(msg, "upstream_error", nil, nil)
	}
	source := parsed
	if errObj, ok := parsed["error"].(map[string]any); ok {
		source = errObj
	}
	message := firstNonEmpty(rawString(source, "message"), rawString(source, "detail"),
		rawString(source, "status_msg"))
	if message == "" {
		if br, ok := source["base_resp"].(map[string]any); ok {
			message = rawString(br, "status_msg")
		}
	}
	if message == "" {
		raw, err := json.Marshal(source)
		if err != nil || len(raw) == 0 {
			message = "upstream returned an error"
		} else {
			message = string(raw)
		}
	}
	typ := firstNonEmpty(rawString(source, "type"), "upstream_error")
	var code any
	if v, ok := source["code"]; ok {
		code = v
	} else if br, ok := source["base_resp"].(map[string]any); ok {
		code = br["status_code"]
	}
	var param any
	if v, ok := source["param"]; ok {
		param = v
	}
	// Gemini 的上下文超限用数字 code 或没有 code：Codex 只认
	// context_length_exceeded 才会触发压缩恢复。
	if strings.Contains(strings.ToLower(message), "input token count exceeds the maximum number of tokens allowed") {
		code = "context_length_exceeded"
	}
	return responsesError(message, typ, code, param)
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

// LooksLikeError 报告解析后的 Chat 响应体是否是错误形状（而非 completion）：
// 没有 choices，且带错误类字段（error/base_resp/message/detail/status_msg）。
func LooksLikeError(v map[string]any) bool {
	if _, ok := v["choices"]; ok {
		return false
	}
	for _, key := range []string{"error", "base_resp", "message", "detail", "status_msg"} {
		if _, ok := v[key]; ok {
			return true
		}
	}
	return false
}
