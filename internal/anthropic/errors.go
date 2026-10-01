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
//
// status 是网关将回给客户端的 HTTP 状态码（0 = 未知）。HTTP 状态是 error.type 的
// 第一判定键（closed enum，CC 按类决定重试/重新鉴权行为）；body 里的 code/type
// 字符串只做补充。只看字符串会把网关自家 429（type=api_error、code=
// rate_limit_exceeded）错报成 api_error，CC 的限流退避随之失效。
func ErrorToMessages(body []byte, status int) map[string]any {
	var upstream map[string]any
	if json.Unmarshal(body, &upstream) != nil {
		return errorMessage(statusErrorType(status, "api_error"), string(body))
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
		// 判定顺序：上游自带 Anthropic 规范 type（api_error 除外——那也是网关
		// OpenAI 形错误体的万能填充值，不携带信息）→ 原样保留；否则 HTTP 状态
		// 优先；最后才按 code/type 字符串粗映射。
		if typ != "api_error" && isAnthropicErrorType(typ) {
			return errorMessage(typ, msg)
		}
		if t := statusErrorType(status, ""); t != "" {
			return errorMessage(t, msg)
		}
		return errorMessage(openAIErrorTypeToAnthropic(strVal(errObj["code"]), typ), msg)
	}
	return errorMessage(statusErrorType(status, "api_error"), string(body))
}

// anthropicErrorTypes Anthropic error.type 的 closed enum。
var anthropicErrorTypes = map[string]bool{
	"invalid_request_error": true,
	"authentication_error":  true,
	"permission_error":      true,
	"not_found_error":       true,
	"request_too_large":     true,
	"rate_limit_error":      true,
	"api_error":             true,
	"overloaded_error":      true,
}

// isAnthropicErrorType 报告 typ 是否已是 Anthropic 规范错误类。
func isAnthropicErrorType(typ string) bool {
	return anthropicErrorTypes[typ]
}

// statusErrorType 按 HTTP 状态映射 Anthropic error.type（closed enum 之外的
// 状态返回 def）。503 → overloaded_error：网关 503 语义是「全部账号暂不可用/
// 上游过载」（no_healthy_account 等），closest Anthropic 类是 overloaded 而非
// rate_limit（后者留给 429，CC 据此做等待重试）。
func statusErrorType(status int, def string) string {
	switch status {
	case 400:
		return "invalid_request_error"
	case 401:
		return "authentication_error"
	case 403:
		return "permission_error"
	case 404:
		return "not_found_error"
	case 413:
		return "request_too_large"
	case 429:
		return "rate_limit_error"
	case 503:
		return "overloaded_error"
	case 500, 502, 504:
		return "api_error"
	}
	return def
}

// StatusErrorType 按 HTTP 状态映射 Anthropic error.type（导出给 server 层的
// /tp 错误路径复用；无匹配状态返回空串）。
func StatusErrorType(status int) string {
	return statusErrorType(status, "")
}

// ErrorTypeForCode 把网关内部稳定错误 code（thirdparty.Classify 产出的 OpenAI
// 风格码）映射为 Anthropic error.type（closed enum；未命中回落 api_error）。
// /tp/v1/messages 的错误路径用：Classify 的 code 不能直接当 error.type 发——
// 那不是 Anthropic 枚举值，CC 只能当未知类型处理。
func ErrorTypeForCode(code string) string {
	switch code {
	case "invalid_api_key":
		return "authentication_error"
	case "permission_denied":
		return "permission_error"
	case "model_not_found":
		return "not_found_error"
	case "rate_limit_exceeded":
		return "rate_limit_error"
	case "invalid_request":
		return "invalid_request_error"
	}
	return "api_error"
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
