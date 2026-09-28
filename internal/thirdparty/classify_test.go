package thirdparty

import (
	"net/http"
	"strings"
	"testing"
)

// TestCodeForStatus 覆盖状态码 → 稳定 code 的映射（第三方上游没有 CodeBuddy 那套
// 业务码，只做粗分类）。
func TestCodeForStatus(t *testing.T) {
	cases := []struct {
		status int
		want   string
	}{
		{http.StatusUnauthorized, "invalid_api_key"},
		{http.StatusForbidden, "permission_denied"},
		{http.StatusNotFound, "model_not_found"},
		{http.StatusTooManyRequests, "rate_limit_exceeded"},
		{http.StatusBadRequest, "invalid_request"},
		{http.StatusUnprocessableEntity, "invalid_request"},
		{http.StatusGatewayTimeout, "upstream_timeout"},
		{http.StatusRequestTimeout, "upstream_timeout"},
		{http.StatusInternalServerError, "upstream_error"},
		{http.StatusBadGateway, "upstream_error"},
	}
	for _, c := range cases {
		if got := codeForStatus(c.status); got != c.want {
			t.Fatalf("codeForStatus(%d) = %q, want %q", c.status, got, c.want)
		}
	}
}

// TestExtractMessage 覆盖上游错误体的各种形态：OpenAI 对象/字符串、
// 通用信封、非 JSON 纯文本。核心红线是**不丢上游原文**。
func TestExtractMessage(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{"OpenAI error.message", `{"error":{"message":"Incorrect API key","type":"invalid_request_error"}}`,
			"Incorrect API key"},
		{"OpenAI error.code 兜底", `{"error":{"code":"invalid_api_key"}}`, "invalid_api_key"},
		{"error 字符串形态", `{"error":"rate limited"}`, "rate limited"},
		{"message 键", `{"message":"bad model"}`, "bad model"},
		{"msg 键", `{"msg":"bad model"}`, "bad model"},
		{"detail 键", `{"detail":"not found"}`, "not found"},
		{"非 JSON 纯文本", `upstream is down`, "upstream is down"},
		{"空体", ``, ""},
		{"无法识别但仍是 JSON", `{"foo":"bar"}`, `{"foo":"bar"}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := extractMessage([]byte(c.body)); got != c.want {
				t.Fatalf("extractMessage(%q) = %q, want %q", c.body, got, c.want)
			}
		})
	}
}

// TestClassify 组合行为：message 透传上游原文，仅在空体时用可读兜底。
func TestClassify(t *testing.T) {
	// 有响应体：原文优先，code 按状态映射。
	got := Classify(http.StatusUnauthorized, []byte(`{"error":{"message":"Incorrect API key provided"}}`))
	if got.Status != http.StatusUnauthorized || got.Code != "invalid_api_key" {
		t.Fatalf("Classify 状态/code 不符: %+v", got)
	}
	if got.Message != "Incorrect API key provided" {
		t.Fatalf("message 应透传上游原文，得到 %q", got.Message)
	}

	// 空响应体：给可读兜底（说明网关观测到的事实，不编造上游原文）。
	empty := Classify(http.StatusNotFound, nil)
	if empty.Message == "" {
		t.Fatal("空体应有兜底 message")
	}
	if !strings.Contains(empty.Message, "模型") {
		t.Fatalf("404 兜底文案应提示模型配置，得到 %q", empty.Message)
	}
}
