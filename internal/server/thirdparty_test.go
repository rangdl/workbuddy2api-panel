package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

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
