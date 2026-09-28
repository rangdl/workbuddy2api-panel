package thirdparty

import (
	"encoding/json"
	"net/http"
	"strings"
)

// ErrorShape 归一化后的错误形状（OpenAI 兼容 error 对象）。
type ErrorShape struct {
	Status  int
	Code    string
	Message string
}

// Classify 把第三方上游的错误响应归一化为稳定形状。
//
// 设计取舍（与 CodeBuddy 侧 upstream.Classify 的根本差异）：第三方上游没有
// CodeBuddy 那套业务码（6004/11101/11102…），也不该套用其冷却语义——这里只做
// 「HTTP 状态 → 稳定 code」的粗分类，**message 一律透传上游原文**：
// 上游原文（含真实原因与 request id）是排查的唯一依据，网关编造文案只会帮倒忙。
// 仅在 body 为空/非 JSON 时补一句可读兜底。
func Classify(status int, body []byte) ErrorShape {
	msg := extractMessage(body)
	code := codeForStatus(status)
	if msg == "" {
		msg = fallbackMessage(status, code)
	}
	return ErrorShape{Status: status, Code: code, Message: msg}
}

// codeForStatus 按 HTTP 状态映射稳定 code。
func codeForStatus(status int) string {
	switch {
	case status == http.StatusUnauthorized:
		return "invalid_api_key"
	case status == http.StatusForbidden:
		return "permission_denied"
	case status == http.StatusNotFound:
		return "model_not_found"
	case status == http.StatusTooManyRequests:
		return "rate_limit_exceeded"
	case status == http.StatusRequestTimeout || status == http.StatusGatewayTimeout:
		return "upstream_timeout"
	case status >= 500:
		return "upstream_error"
	case status >= 400:
		return "invalid_request"
	default:
		return "upstream_error"
	}
}

// extractMessage 从上游错误体提取可读 message。
// 依次尝试 OpenAI 形态（error.message / error.code 对象或字符串）、
// 火山/通用信封（message / msg / error 字符串），全不命中返回空串。
func extractMessage(body []byte) string {
	if len(body) == 0 {
		return ""
	}
	var v map[string]any
	if json.Unmarshal(body, &v) != nil {
		// 非 JSON：原文即信息（部分中转返回纯文本/HTML 错误页）。
		return strings.TrimSpace(string(body))
	}
	// OpenAI 形态：{"error": {"message": "...", "code": "..."}}
	if errObj, ok := v["error"].(map[string]any); ok {
		if m, _ := errObj["message"].(string); strings.TrimSpace(m) != "" {
			return strings.TrimSpace(m)
		}
		if c, _ := errObj["code"].(string); strings.TrimSpace(c) != "" {
			return strings.TrimSpace(c)
		}
	}
	// {"error": "..."} 字符串形态
	if s, ok := v["error"].(string); ok && strings.TrimSpace(s) != "" {
		return strings.TrimSpace(s)
	}
	for _, k := range []string{"message", "msg", "detail"} {
		if s, ok := v[k].(string); ok && strings.TrimSpace(s) != "" {
			return strings.TrimSpace(s)
		}
	}
	// 无法识别但确实是 JSON：回原文，保证信息不丢。
	return strings.TrimSpace(string(body))
}

// fallbackMessage 上游无响应体时的可读兜底（不编造上游原文，只说明网关观测到的事实）。
func fallbackMessage(status int, code string) string {
	switch code {
	case "invalid_api_key":
		return "第三方上游拒绝了 API Key（请检查 third_party.json 的 api_key）"
	case "model_not_found":
		return "第三方上游不存在该模型（请检查 providers[].models 配置）"
	case "rate_limit_exceeded":
		return "第三方上游限流（上游未返回响应体）"
	case "upstream_timeout":
		return "第三方上游超时（上游未返回响应体）"
	default:
		return "第三方上游返回空错误体"
	}
}
