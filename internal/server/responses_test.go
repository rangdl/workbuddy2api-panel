package server

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/responsesstore"
)

func responsesTestHandler(t *testing.T, behavior func(authz string) (int, string, bool)) *Handler {
	t.Helper()
	up := newFakeUpstream(t, behavior)
	return NewHandler(Config{
		Pool:      testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream:  up,
		Responses: &ResponsesConfig{Enabled: true, Store: responsesstore.NewMemoryStore(0)},
	})
}

func TestResponsesNonStream(t *testing.T) {
	h := responsesTestHandler(t, func(authz string) (int, string, bool) {
		return 200, sseOK, true
	})
	req := httptest.NewRequest("POST", "/v1/responses", strings.NewReader(`{"model":"glm-5.2","input":"hi"}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("not json: %v body=%s", err, rec.Body)
	}
	if resp["object"] != "response" {
		t.Errorf("object=%v", resp["object"])
	}
	if resp["status"] != "completed" {
		t.Errorf("status=%v", resp["status"])
	}
	output, _ := resp["output"].([]any)
	if len(output) == 0 {
		t.Fatalf("empty output: %v", resp)
	}
	msg, _ := output[0].(map[string]any)
	if msg["type"] != "message" {
		t.Fatalf("first item=%v", msg)
	}
	content, _ := msg["content"].([]any)
	part, _ := content[0].(map[string]any)
	if part["text"] != "你好" {
		t.Errorf("text=%v", part["text"])
	}
}

func TestResponsesStream(t *testing.T) {
	h := responsesTestHandler(t, func(authz string) (int, string, bool) {
		return 200, sseOK, true
	})
	req := httptest.NewRequest("POST", "/v1/responses", strings.NewReader(`{"model":"glm-5.2","stream":true,"input":"hi"}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	body := rec.Body.String()
	for _, ev := range []string{
		"event: response.created",
		"event: response.output_text.delta",
		"event: response.completed",
	} {
		if !strings.Contains(body, ev) {
			t.Errorf("missing %q\n%s", ev, body)
		}
	}
	if !strings.Contains(body, "你好") {
		t.Errorf("missing text: %s", body)
	}
	// Responses 流不应透传 Chat 的 [DONE] 行。
	if strings.Contains(body, "data: [DONE]") {
		t.Errorf("should not contain chat [DONE]: %s", body)
	}
}

func TestResponsesModelMap(t *testing.T) {
	var seenModel string
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 200, sseOK, true
	})
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: up,
		Responses: &ResponsesConfig{
			Enabled:      true,
			Store:        responsesstore.NewMemoryStore(0),
			ModelMap:     map[string]string{"gpt-5-codex": "glm-5.2"},
			DefaultModel: "",
		},
	})
	_ = seenModel
	req := httptest.NewRequest("POST", "/v1/responses", strings.NewReader(`{"model":"gpt-5-codex","input":"hi"}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	var resp map[string]any
	json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp["model"] != "glm-5.2" {
		t.Errorf("model=%v (should be mapped)", resp["model"])
	}
}

func TestResponsesIncrementalFill(t *testing.T) {
	// 第一次响应返回 function_call，第二次请求带 previous_response_id + call_output。
	store := responsesstore.NewMemoryStore(0)
	toolSSE := `data: {"id":"chatcmpl-tool","object":"chat.completion.chunk","created":1753600000,"model":"glm-5.2","choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"shell","arguments":"{\"cmd\":\"ls\"}"}}]}}]}` + "\n\n" +
		`data: {"id":"chatcmpl-tool","object":"chat.completion.chunk","created":1753600000,"model":"glm-5.2","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}` + "\n\n" +
		"data: [DONE]\n\n"

	var lastBody string
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 200, toolSSE, true
	})
	h := NewHandler(Config{
		Pool:      testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream:  up,
		Responses: &ResponsesConfig{Enabled: true, Store: store},
	})

	// 第一次
	req1 := httptest.NewRequest("POST", "/v1/responses", strings.NewReader(`{"model":"glm-5.2","input":"list files","tools":[{"type":"function","name":"shell","parameters":{"type":"object"}}]}`))
	rec1 := httptest.NewRecorder()
	h.ServeHTTP(rec1, req1)
	var resp1 map[string]any
	json.Unmarshal(rec1.Body.Bytes(), &resp1)
	respID, _ := resp1["id"].(string)
	if respID == "" {
		t.Fatalf("no response id: %s", rec1.Body)
	}
	output, _ := resp1["output"].([]any)
	foundCall := false
	for _, raw := range output {
		if item, ok := raw.(map[string]any); ok && item["type"] == "function_call" {
			foundCall = true
		}
	}
	if !foundCall {
		t.Fatalf("first response has no function_call: %s", rec1.Body)
	}

	// 第二次：增量
	body2 := `{"model":"glm-5.2","previous_response_id":"` + respID + `","input":[{"type":"function_call_output","call_id":"call_1","output":"file1"}]}`
	req2 := httptest.NewRequest("POST", "/v1/responses", strings.NewReader(body2))
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, req2)
	if rec2.Code != 200 {
		t.Fatalf("code=%d body=%s", rec2.Code, rec2.Body)
	}
	_ = lastBody
}

// TestResponsesHotReload 验证面板保存后热重载：启用/禁用开关与模型映射立即生效，
// 无需重启（SetResponsesConfig 原子替换）。
func TestResponsesHotReload(t *testing.T) {
	up := newFakeUpstream(t, func(authz string) (int, string, bool) { return 200, sseOK, true })
	h := NewHandler(Config{
		Pool:      testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream:  up,
		Responses: &ResponsesConfig{Enabled: true, Store: responsesstore.NewMemoryStore(0)},
	})
	post := func(model string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("POST", "/v1/responses", strings.NewReader(`{"model":"`+model+`","input":"hi"}`))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}

	// 热禁用 → 404（路由仍在，handler 运行期判断）。
	h.SetResponsesConfig(&ResponsesConfig{Enabled: false})
	if rec := post("glm-5.2"); rec.Code != 404 {
		t.Fatalf("disabled should be 404, got %d", rec.Code)
	}

	// 热启用 + 模型映射 → 200，且请求 model 被映射（上游 fake 固定返回 glm-5.2）。
	h.SetResponsesConfig(&ResponsesConfig{Enabled: true, ModelMap: map[string]string{"gpt-5-codex": "glm-5.2"}})
	rec := post("gpt-5-codex")
	if rec.Code != 200 {
		t.Fatalf("enabled should be 200, got %d body=%s", rec.Code, rec.Body)
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("not json: %v", err)
	}
	if resp["model"] != "glm-5.2" {
		t.Errorf("mapped model=%v", resp["model"])
	}
}
