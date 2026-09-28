package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/trae"
)

// traeSSE 假上游返回的 SOLO SSE（含内容、用量、结束）。
const traeSSE = "event: metadata\ndata: {\"session_id\":\"s1\"}\n\n" +
	"event: output\ndata: {\"response\":\"你好\"}\n\n" +
	"event: output\ndata: {\"response\":\"世界\"}\n\n" +
	"event: token_usage\ndata: {\"prompt_tokens\":3,\"completion_tokens\":5,\"total_tokens\":8}\n\n" +
	"event: done\ndata: {\"finish_reason\":\"stop\"}\n\n"

// newTraeHandler 构造只带 Trae 上游的 handler（不涉及 CodeBuddy 账号池）。
func newTraeHandler(t *testing.T, upstreamURL string) *Handler {
	t.Helper()
	cfg := &trae.Config{
		Enabled:        true,
		ChatBase:       upstreamURL,
		IDEVersion:     trae.DefaultIDEVersion,
		IDEVersionCode: trae.DefaultIDEVersionCode,
		AppID:          trae.DefaultAppID,
		Accounts: []trae.Account{{
			UID: "u1", Name: "trae-1", AccessToken: "tok-1",
			DeviceID: "dev-1", MachineID: "machine-1",
		}},
	}
	return NewHandler(Config{Trae: cfg, TraeClient: trae.NewClient()})
}

// TestTraeChatNonStream 非流式：SOLO SSE 聚合成 OpenAI chat.completion。
func TestTraeChatNonStream(t *testing.T) {
	var gotPath string
	var gotBody map[string]any
	var gotAuth, gotDevice string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		gotDevice = r.Header.Get("X-Device-Id")
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &gotBody)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(traeSSE))
	}))
	defer srv.Close()

	h := newTraeHandler(t, srv.URL)
	req := httptest.NewRequest(http.MethodPost, "/trae/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body)
	}
	// 上游收到的请求已改写成 llm_utils_chat 形态
	if gotPath != trae.EndpointLLMChat {
		t.Errorf("上游路径 = %q, want %q", gotPath, trae.EndpointLLMChat)
	}
	if gotBody["config_name"] != "glm-5.2" || gotBody["model_name"] != "glm-5.2__dev" {
		t.Errorf("模型字段未改写: %v / %v", gotBody["config_name"], gotBody["model_name"])
	}
	if gotBody["stream"] != true {
		t.Errorf("stream 必须为 true: %v", gotBody["stream"])
	}
	if gotBody["function"] != trae.FunctionSoloWorkLite {
		t.Errorf("function = %v", gotBody["function"])
	}
	if gotAuth != "Cloud-IDE-JWT tok-1" {
		t.Errorf("Authorization = %q", gotAuth)
	}
	if gotDevice != "dev-1" {
		t.Errorf("X-Device-Id = %q", gotDevice)
	}

	// 回程是标准 OpenAI completion
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("响应非 JSON: %s", rec.Body)
	}
	if resp["object"] != "chat.completion" {
		t.Errorf("object = %v", resp["object"])
	}
	choices, _ := resp["choices"].([]any)
	c, _ := choices[0].(map[string]any)
	msg, _ := c["message"].(map[string]any)
	if msg["content"] != "你好世界" {
		t.Errorf("content = %v, want 你好世界", msg["content"])
	}
	if c["finish_reason"] != "stop" {
		t.Errorf("finish_reason = %v", c["finish_reason"])
	}
	usage, _ := resp["usage"].(map[string]any)
	if usage["total_tokens"] != float64(8) {
		t.Errorf("usage = %v", usage)
	}
}

// TestTraeChatStream 流式：SOLO SSE 转成 OpenAI SSE 并补 [DONE]。
func TestTraeChatStream(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(traeSSE))
	}))
	defer srv.Close()

	h := newTraeHandler(t, srv.URL)
	req := httptest.NewRequest(http.MethodPost, "/trae/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		t.Fatalf("Content-Type = %q", ct)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"object":"chat.completion.chunk"`) {
		t.Errorf("缺少 OpenAI chunk 形状: %s", body)
	}
	if !strings.Contains(body, "你好") || !strings.Contains(body, "世界") {
		t.Errorf("内容未透出: %s", body)
	}
	if !strings.Contains(body, "data: [DONE]") {
		t.Errorf("缺少 [DONE]: %s", body)
	}
	if !strings.Contains(body, `"finish_reason":"stop"`) {
		t.Errorf("缺少 finish_reason: %s", body)
	}
	// SOLO 专属事件名不应泄漏给客户端
	if strings.Contains(body, "event: output") {
		t.Errorf("上游事件名泄漏: %s", body)
	}
}

// TestTraeResponsesNonStream Responses 路径：请求转 Chat 再转 SOLO，回程投影为 Responses。
func TestTraeResponsesNonStream(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(traeSSE))
	}))
	defer srv.Close()

	h := newTraeHandler(t, srv.URL)
	req := httptest.NewRequest(http.MethodPost, "/trae/v1/responses",
		strings.NewReader(`{"model":"glm-5.2","input":"hi"}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body)
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("响应非 JSON: %s", rec.Body)
	}
	if resp["object"] != "response" {
		t.Fatalf("object = %v, want response（body=%s）", resp["object"], rec.Body)
	}
	if _, ok := resp["output"]; !ok {
		t.Fatalf("缺 output 字段: %s", rec.Body)
	}
}

// TestTraeModels 模型列表来自内置表。
func TestTraeModels(t *testing.T) {
	h := newTraeHandler(t, "http://127.0.0.1:1")
	req := httptest.NewRequest(http.MethodGet, "/trae/v1/models", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var out struct {
		Object string `json:"object"`
		Data   []struct {
			ID      string `json:"id"`
			OwnedBy string `json:"owned_by"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Object != "list" || len(out.Data) == 0 {
		t.Fatalf("模型列表不符: %s", rec.Body)
	}
	if out.Data[0].OwnedBy != "trae" {
		t.Errorf("owned_by = %q", out.Data[0].OwnedBy)
	}
}

// TestTraeDisabled 未注入配置时路由不注册 → 404（且不影响 /v1/* 与 /tp/v1/*）。
func TestTraeDisabled(t *testing.T) {
	h := NewHandler(Config{})
	req := httptest.NewRequest(http.MethodGet, "/trae/v1/models", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("未启用时 status = %d, want 404", rec.Code)
	}
}

// TestTraeNoAccount 配置了但无可用账号 → 503 no_healthy_account。
func TestTraeNoAccount(t *testing.T) {
	cfg := &trae.Config{Enabled: true, ChatBase: "http://127.0.0.1:1", Accounts: nil}
	h := NewHandler(Config{Trae: cfg, TraeClient: trae.NewClient()})
	req := httptest.NewRequest(http.MethodPost, "/trae/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","messages":[]}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "no_healthy_account") {
		t.Errorf("body = %s", rec.Body.String())
	}
}

// TestTraeUpstreamError 上游 4xx 原文透传 + code 归一。
func TestTraeUpstreamError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"code":20405,"message":"Device proof required"}`))
	}))
	defer srv.Close()

	h := newTraeHandler(t, srv.URL)
	req := httptest.NewRequest(http.MethodPost, "/trae/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","messages":[]}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "Device proof required") {
		t.Errorf("上游原文未透传: %s", body)
	}
	if !strings.Contains(body, "invalid_api_key") {
		t.Errorf("code 未归一: %s", body)
	}
}
