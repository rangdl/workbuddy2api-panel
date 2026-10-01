package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/livecfg"
	"github.com/linguo2625469/workbuddy2api-panel/internal/thirdparty"
)

// newThirdPartyHandler 构造只带第三方上游的 handler（不涉及账号池）。
// upstreamURL 是假第三方上游的根地址，provider base 用 <url>/v1。
//
// 刻意不经 Load()/normalize()：这条构造路径（测试、面板热改）最容易漏填
// ChatPath/TimeoutSeconds，正好用来锁定 ChatURL/Chat 的缺省兜底行为。
func newThirdPartyHandler(t *testing.T, upstreamURL string) *Handler {
	t.Helper()
	cfg := &thirdparty.Config{
		Enabled: true,
		Providers: []thirdparty.Provider{{
			Name:     "fake",
			Protocol: thirdparty.ProtocolOpenAI,
			BaseURL:  upstreamURL + "/v1",
			APIKey:   "sk-test",
			Models:   []string{"fake-model"},
		}},
	}
	return NewHandler(Config{
		ThirdParty:       cfg,
		ThirdPartyClient: thirdparty.NewClient(),
	})
}

// TestThirdPartyChatNonStream 非流式转发：请求落到正确端点、带上 API Key、
// 响应原样透传。
func TestThirdPartyChatNonStream(t *testing.T) {
	var gotPath, gotAuth string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &gotBody)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl-1","object":"chat.completion","model":"fake-model",
			"choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],
			"usage":{"prompt_tokens":3,"completion_tokens":5,"total_tokens":8}}`))
	}))
	defer srv.Close()

	h := newThirdPartyHandler(t, srv.URL)
	req := httptest.NewRequest(http.MethodPost, "/tp/v1/chat/completions",
		strings.NewReader(`{"model":"fake-model","messages":[{"role":"user","content":"hi"}]}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if gotPath != "/v1/chat/completions" {
		t.Fatalf("上游路径 = %q, want /v1/chat/completions", gotPath)
	}
	if gotAuth != "Bearer sk-test" {
		t.Fatalf("上游 Authorization = %q, want Bearer sk-test", gotAuth)
	}
	if gotBody["model"] != "fake-model" {
		t.Fatalf("上游收到的 model = %v", gotBody["model"])
	}
	if !strings.Contains(rec.Body.String(), `"content":"hi"`) {
		t.Fatalf("响应未原样透传: %s", rec.Body.String())
	}
}

// TestThirdPartyChatStream 流式转发：SSE 帧透传且 Content-Type 正确。
func TestThirdPartyChatStream(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		for _, frame := range []string{
			"data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"你\"}}]}\n\n",
			"data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"好\"}}]}\n\n",
			"data: [DONE]\n\n",
		} {
			_, _ = w.Write([]byte(frame))
			if flusher != nil {
				flusher.Flush()
			}
		}
	}))
	defer srv.Close()

	h := newThirdPartyHandler(t, srv.URL)
	req := httptest.NewRequest(http.MethodPost, "/tp/v1/chat/completions",
		strings.NewReader(`{"model":"fake-model","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		t.Fatalf("Content-Type = %q", ct)
	}
	body := rec.Body.String()
	for _, want := range []string{"你", "好", "[DONE]"} {
		if !strings.Contains(body, want) {
			t.Fatalf("SSE 透传缺 %q: %s", want, body)
		}
	}
}

// TestThirdPartyResponsesNonStream Responses 路径端到端：Responses 请求体 →
// 内部转 Chat → 转发第三方 → 回程投影为 Responses 对象。
func TestThirdPartyResponsesNonStream(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &gotBody)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl-2","object":"chat.completion","model":"fake-model",
			"choices":[{"index":0,"message":{"role":"assistant","content":"42"},"finish_reason":"stop"}],
			"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}))
	defer srv.Close()

	h := newThirdPartyHandler(t, srv.URL)
	req := httptest.NewRequest(http.MethodPost, "/tp/v1/responses",
		strings.NewReader(`{"model":"fake-model","input":"the answer?","instructions":"be brief"}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	// 转换层把 Responses 的 input/instructions 翻成了 Chat 的 messages。
	msgs, _ := gotBody["messages"].([]any)
	if len(msgs) == 0 {
		t.Fatalf("上游未收到 messages: %v", gotBody)
	}
	// 回程是 Responses 对象而非 Chat 对象。
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("响应非 JSON: %s", rec.Body.String())
	}
	if resp["object"] != "response" {
		t.Fatalf("object = %v, want response（body=%s）", resp["object"], rec.Body.String())
	}
	if _, ok := resp["output"]; !ok {
		t.Fatalf("Responses 响应缺 output 字段: %s", rec.Body.String())
	}
}

// TestThirdPartyModels 模型列表来自配置声明。
func TestThirdPartyModels(t *testing.T) {
	h := newThirdPartyHandler(t, "http://127.0.0.1:1") // 不触网
	req := httptest.NewRequest(http.MethodGet, "/tp/v1/models", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var out struct {
		Object string `json:"object"`
		Data   []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Object != "list" || len(out.Data) != 1 || out.Data[0].ID != "fake-model" {
		t.Fatalf("模型列表不符: %s", rec.Body.String())
	}
}

// TestThirdPartyModelNotFound 未命中任何 provider → 404 model_not_found。
func TestThirdPartyModelNotFound(t *testing.T) {
	h := newThirdPartyHandler(t, "http://127.0.0.1:1")
	req := httptest.NewRequest(http.MethodPost, "/tp/v1/chat/completions",
		strings.NewReader(`{"model":"unknown-model","messages":[]}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "model_not_found") {
		t.Fatalf("body = %s", rec.Body.String())
	}
}

// TestThirdPartyDisabled 未注入第三方配置时：路由未注册 → 404（且 /v1/* 不受影响）。
func TestThirdPartyDisabled(t *testing.T) {
	h := NewHandler(Config{})
	req := httptest.NewRequest(http.MethodGet, "/tp/v1/models", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("未启用时 /tp/v1/models status = %d, want 404", rec.Code)
	}
}

// TestThirdPartyUpstreamError 上游 4xx 错误体原文透传（不编造文案）。
func TestThirdPartyUpstreamError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"message":"Incorrect API key provided: sk-test"}}`))
	}))
	defer srv.Close()

	h := newThirdPartyHandler(t, srv.URL)
	req := httptest.NewRequest(http.MethodPost, "/tp/v1/chat/completions",
		strings.NewReader(`{"model":"fake-model","messages":[]}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "Incorrect API key provided") {
		t.Fatalf("上游错误原文未透传: %s", body)
	}
	if !strings.Contains(body, "invalid_api_key") {
		t.Fatalf("错误 code 未归一: %s", body)
	}
}

// ---- /tp/v1/messages（Anthropic 协议接入第三方上游）----
// 此前该端点零测试覆盖；model_map 跳过与错误枚举映射的回归都在这里锁定。

// newTPAnthropicHandler 构造带第三方上游的 handler（沿用 newThirdPartyHandler，
// 另设错误的 anthropic.json 映射，验证 /tp 不套用该映射）。
func newTPAnthropicHandler(t *testing.T, upstreamURL string) *Handler {
	t.Helper()
	h := newThirdPartyHandler(t, upstreamURL)
	h.SetAnthropicConfig(&AnthropicConfig{
		Enabled:      true,
		DefaultModel: "mapped-should-not-apply",
		ModelMap:     map[string]string{"claude-sonnet-4-5": "mapped-should-not-apply"},
	})
	return h
}

// TestTPAnthropicNonStream 端到端：Anthropic 请求 → Chat → 第三方上游 → 回程
// Anthropic message；model 原样透传（/tp 有意不套用 anthropicCfg 的模型映射）。
func TestTPAnthropicNonStream(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &gotBody)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl-tp","object":"chat.completion","model":"fake-model",
			"choices":[{"index":0,"message":{"role":"assistant","content":"hello from tp"},"finish_reason":"stop"}],
			"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`))
	}))
	defer srv.Close()

	h := newTPAnthropicHandler(t, srv.URL)
	req := httptest.NewRequest(http.MethodPost, "/tp/v1/messages",
		strings.NewReader(`{"model":"fake-model","max_tokens":128,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("x-api-key", "k")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	// 上游收到的必须是 Chat 请求体，且 model 未被映射改写。
	if gotBody["model"] != "fake-model" {
		t.Errorf("上游 model = %v, want fake-model（/tp 不套用 anthropicCfg 映射）", gotBody["model"])
	}
	if _, ok := gotBody["max_tokens"]; !ok {
		t.Errorf("max_tokens 未透传: %v", gotBody)
	}
	// 回程是 Anthropic message 形状。
	var msg map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &msg); err != nil {
		t.Fatalf("响应非 JSON: %s", rec.Body.String())
	}
	if msg["type"] != "message" || msg["role"] != "assistant" {
		t.Fatalf("非 Anthropic message: %v", msg)
	}
	content, _ := msg["content"].([]any)
	if len(content) == 0 {
		t.Fatalf("空 content: %v", msg)
	}
	block, _ := content[0].(map[string]any)
	if block["type"] != "text" || block["text"] != "hello from tp" {
		t.Errorf("text block = %v", block)
	}
}

// TestTPAnthropicStream 流式：Chat SSE → Anthropic SSE 六事件族收尾。
func TestTPAnthropicStream(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		frames := []string{
			"data: {\"id\":\"c1\",\"model\":\"fake-model\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"好\"}}]}\n\n",
			"data: {\"id\":\"c1\",\"model\":\"fake-model\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":2,\"total_tokens\":5}}\n\n",
			"data: [DONE]\n\n",
		}
		for _, f := range frames {
			_, _ = w.Write([]byte(f))
			if flusher != nil {
				flusher.Flush()
			}
		}
	}))
	defer srv.Close()

	h := newTPAnthropicHandler(t, srv.URL)
	req := httptest.NewRequest(http.MethodPost, "/tp/v1/messages",
		strings.NewReader(`{"model":"fake-model","max_tokens":64,"stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("x-api-key", "k")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{"message_start", "content_block_start", "content_block_delta", "content_block_stop", "message_delta", "message_stop"} {
		if !strings.Contains(body, "event: "+want) {
			t.Errorf("缺事件 %s\n%s", want, body)
		}
	}
	if !strings.Contains(body, `"text":"好"`) {
		t.Errorf("缺文本增量\n%s", body)
	}
}

// TestTPAnthropicUpstreamError 上游 4xx → Anthropic closed-enum error.type
// （Classify 的 OpenAI 风格码不得直接当 error.type 发）。本测试同时走真实
// x-api-key 鉴权（APIKey 非空时 VerifyAnthropic 不再短路放行），锁定中间件接线。
func TestTPAnthropicUpstreamError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"message":"bad upstream key"}}`))
	}))
	defer srv.Close()

	h := newTPAnthropicHandler(t, srv.URL)
	// 真实鉴权：Live 快照设 APIKey 后 VerifyAnthropic 不再空 key 短路放行，
	// 顺带锁定 x-api-key 中间件接线（错误 key 必须 401）。
	h.cfg.Live = livecfg.New(livecfg.Snapshot{APIKey: "sk-live-key"})
	req := httptest.NewRequest(http.MethodPost, "/tp/v1/messages",
		strings.NewReader(`{"model":"fake-model","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("x-api-key", "sk-live-key")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"authentication_error"`) {
		t.Errorf("error.type 应为 Anthropic 枚举 authentication_error: %s", body)
	}
	if strings.Contains(body, "invalid_api_key") {
		t.Errorf("OpenAI 风格码不得出现在 error.type: %s", body)
	}
	if !strings.Contains(body, "bad upstream key") {
		t.Errorf("上游原文应透传: %s", body)
	}

	// 错误 key → 401（鉴权中间件真实生效）。
	reqBad := httptest.NewRequest(http.MethodPost, "/tp/v1/messages",
		strings.NewReader(`{"model":"fake-model","max_tokens":16,"messages":[]}`))
	reqBad.Header.Set("x-api-key", "wrong-key")
	recBad := httptest.NewRecorder()
	h.ServeHTTP(recBad, reqBad)
	if recBad.Code != http.StatusUnauthorized {
		t.Errorf("错误 x-api-key 应 401, got %d", recBad.Code)
	}
	if !strings.Contains(recBad.Body.String(), "authentication_error") {
		t.Errorf("鉴权错误形状应为 Anthropic 枚举: %s", recBad.Body.String())
	}
}

// TestTPAnthropicUpstream429 429 → rate_limit_error（CC 据此做限流退避）。
func TestTPAnthropicUpstream429(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"message":"quota"}}`))
	}))
	defer srv.Close()

	h := newTPAnthropicHandler(t, srv.URL)
	req := httptest.NewRequest(http.MethodPost, "/tp/v1/messages",
		strings.NewReader(`{"model":"fake-model","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("x-api-key", "k")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"rate_limit_error"`) {
		t.Errorf("error.type 应为 rate_limit_error: %s", rec.Body.String())
	}
}

// TestTPAnthropicUnknownModel 未命中 provider → 404 not_found_error。
func TestTPAnthropicUnknownModel(t *testing.T) {
	h := newThirdPartyHandler(t, "http://127.0.0.1:1")
	req := httptest.NewRequest(http.MethodPost, "/tp/v1/messages",
		strings.NewReader(`{"model":"unknown","max_tokens":16,"messages":[]}`))
	req.Header.Set("x-api-key", "k")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"not_found_error"`) {
		t.Errorf("error.type 应为 not_found_error: %s", rec.Body.String())
	}
}
