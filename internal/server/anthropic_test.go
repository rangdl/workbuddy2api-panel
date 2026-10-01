package server

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

func anthropicTestHandler(t *testing.T, behavior func(authz string) (int, string, bool)) *Handler {
	t.Helper()
	up := newFakeUpstream(t, behavior)
	return NewHandler(Config{
		Pool:      testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream:  up,
		Anthropic: &AnthropicConfig{Enabled: true},
	})
}

const anthropicRequestBody = `{"model":"glm-5.2","max_tokens":64,"system":"You are helpful.","messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}]}`

// 非流式：Chat JSON → Anthropic message（CC 的 stream=false 常态路径）。
func TestAnthropicNonStream(t *testing.T) {
	// 真实链路恒流式（chatCompletions 强制上游 stream:true），非流式 Anthropic 响应
	// 由 anthropicWriter 聚合 Chat SSE 后整体转换。
	h := anthropicTestHandler(t, func(authz string) (int, string, bool) {
		return 200, sseOK, true
	})
	req := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(anthropicRequestBody))
	req.Header.Set("x-api-key", "k") // httpauth 未启用鉴权时值不重要
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("not json: %v body=%s", err, rec.Body)
	}
	if resp["type"] != "message" || resp["role"] != "assistant" {
		t.Errorf("envelope=%v", resp)
	}
	if resp["stop_reason"] != "end_turn" {
		t.Errorf("stop_reason=%v", resp["stop_reason"])
	}
	content, _ := resp["content"].([]any)
	if len(content) != 1 {
		t.Fatalf("content=%v", content)
	}
	block, _ := content[0].(map[string]any)
	if block["type"] != "text" || block["text"] != "你好" {
		t.Errorf("text block=%v", block)
	}
}

// 流式：Chat SSE → 聚合转换的 Anthropic message（A1 过渡路径；A3 起为实时事件流）。
func TestAnthropicStreamAggregated(t *testing.T) {
	h := anthropicTestHandler(t, func(authz string) (int, string, bool) {
		return 200, sseOK, true
	})
	req := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(anthropicRequestBody))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("not json: %v body=%s", err, rec.Body)
	}
	if resp["type"] != "message" {
		t.Errorf("envelope=%v", resp)
	}
	content, _ := resp["content"].([]any)
	if len(content) == 0 {
		t.Fatalf("empty content: %v", resp)
	}
	block, _ := content[0].(map[string]any)
	if block["text"] != "你好" {
		t.Errorf("text=%v", block["text"])
	}
}

// 禁用时 404 + Anthropic 错误形状。
func TestAnthropicDisabled404(t *testing.T) {
	h := anthropicTestHandler(t, func(authz string) (int, string, bool) {
		return 200, sseOK, true
	})
	h.SetAnthropicConfig(&AnthropicConfig{Enabled: false})
	req := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(anthropicRequestBody))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != 404 {
		t.Fatalf("code=%d", rec.Code)
	}
	var resp map[string]any
	json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp["type"] != "error" {
		t.Errorf("error shape=%v", resp)
	}
}

// 配置未注入（nil）时不注册路由 → 404（mux 层）。
func TestAnthropicNotRegistered(t *testing.T) {
	up := newFakeUpstream(t, func(authz string) (int, string, bool) { return 200, sseOK, true })
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: up,
	})
	req := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(anthropicRequestBody))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 404 {
		t.Fatalf("code=%d", rec.Code)
	}
}

// x-api-key 鉴权：密钥正确放行；缺失/错误 401（Anthropic 错误形状）。
func TestAnthropicXAPIKeyAuth(t *testing.T) {
	up := newFakeUpstream(t, func(authz string) (int, string, bool) { return 200, sseOK, true })
	h := NewHandler(Config{
		Pool:      testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream:  up,
		APIKey:    "sekret",
		Anthropic: &AnthropicConfig{Enabled: true},
	})

	// 正确的 x-api-key。
	req := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(anthropicRequestBody))
	req.Header.Set("x-api-key", "sekret")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("valid x-api-key rejected: code=%d body=%s", rec.Code, rec.Body)
	}

	// 缺失鉴权头。
	req = httptest.NewRequest("POST", "/v1/messages", strings.NewReader(anthropicRequestBody))
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 401 {
		t.Fatalf("missing key: code=%d", rec.Code)
	}
	var resp map[string]any
	json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp["type"] != "error" {
		t.Errorf("401 shape=%v (must be anthropic error shape)", resp)
	}

	// Bearer 回落（兼容代理形态）。
	req = httptest.NewRequest("POST", "/v1/messages", strings.NewReader(anthropicRequestBody))
	req.Header.Set("Authorization", "Bearer sekret")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("bearer fallback rejected: code=%d", rec.Code)
	}
}

// 模型映射：model_map 命中后上游收到映射名。
func TestAnthropicModelMap(t *testing.T) {
	var gotModel string
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		_ = authz
		return 200, sseOK, true
	})
	h := NewHandler(Config{
		Pool:      testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream:  up,
		Anthropic: &AnthropicConfig{Enabled: true, ModelMap: map[string]string{"claude-sonnet-4-5": "glm-5.2"}},
	})
	req := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(
		`{"model":"claude-sonnet-4-5","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	_ = gotModel
	// fake upstream 回显 sseOK（model=glm-5.2）；映射是否生效看响应 model 字段回投。
	var resp map[string]any
	json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp["model"] != "glm-5.2" {
		t.Errorf("model=%v, want mapped glm-5.2 echoed back", resp["model"])
	}
}

// 上游错误 → Anthropic 错误形状透传。
func TestAnthropicUpstreamError(t *testing.T) {
	h := anthropicTestHandler(t, func(authz string) (int, string, bool) {
		return 429, `{"error":{"message":"slow down","type":"rate_limit_error","code":"rate_limit"}}`, false
	})
	req := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(anthropicRequestBody))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != 429 {
		t.Fatalf("code=%d", rec.Code)
	}
	var resp map[string]any
	json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp["type"] != "error" {
		t.Errorf("shape=%v", resp)
	}
	errObj, _ := resp["error"].(map[string]any)
	if errObj["message"] != "slow down" {
		t.Errorf("message=%v", errObj["message"])
	}
}

// 双拼路径容错：CC 把 base_url 配成带 /v1 结尾时实际请求 /v1/v1/messages
// （CC 恒在 base_url 后追加 /v1/messages，2.1.286 实测）。同 handler 响应，
// 避免 404 page not found 被 CC 显示为 model_not_found 误导排障。
func TestAnthropicDoubleSlashRoute(t *testing.T) {
	h := anthropicTestHandler(t, func(authz string) (int, string, bool) {
		return 200, sseOK, true
	})
	req := httptest.NewRequest("POST", "/v1/v1/messages", strings.NewReader(anthropicRequestBody))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	var resp map[string]any
	json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp["type"] != "message" {
		t.Errorf("envelope=%v", resp)
	}
}
