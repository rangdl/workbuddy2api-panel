package server

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/responsesstore"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
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

// newCapturingFakeUpstream 在 fake upstream 上捕获每个请求的转发 body（按到达
// 顺序），供断言「网关实际发给上游的 model / 恢复后的 function_call」——
// 之前的版本只断言响应体，断言形同虚设（响应里的 model 来自 fake 写死的 body）。
type capturingFake struct {
	*upstream.Client
	mu       sync.Mutex
	bodies   []map[string]any
	behavior func(call int) (int, string, bool) // call 从 0 起；nil = 恒 200/sseOK 流
}

func newCapturingFakeUpstream(t *testing.T, behavior func(call int) (int, string, bool)) *capturingFake {
	t.Helper()
	cf := &capturingFake{behavior: behavior}
	cf.Client = &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			raw, _ := io.ReadAll(r.Body)
			var body map[string]any
			_ = json.Unmarshal(raw, &body)
			cf.mu.Lock()
			idx := len(cf.bodies)
			cf.bodies = append(cf.bodies, body)
			cf.mu.Unlock()
			status, respBody, isStream := 200, sseOK, true
			if cf.behavior != nil {
				status, respBody, isStream = cf.behavior(idx)
			}
			ct := "application/json"
			if isStream {
				ct = "text/event-stream"
			}
			return &http.Response{
				StatusCode: status,
				Header:     http.Header{"Content-Type": []string{ct}},
				Body:       io.NopCloser(strings.NewReader(respBody)),
			}, nil
		})},
		ChatBaseCN:    "https://fake.example",
		BillingBaseCN: "https://fake.example",
	}
	return cf
}

// lastBody 返回第 i 个（从 0 起）转发请求体。
func (cf *capturingFake) bodyAt(i int) map[string]any {
	cf.mu.Lock()
	defer cf.mu.Unlock()
	if i < 0 || i >= len(cf.bodies) {
		return nil
	}
	return cf.bodies[i]
}

func TestResponsesModelMap(t *testing.T) {
	up := newCapturingFakeUpstream(t, nil)
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: up.Client,
		Responses: &ResponsesConfig{
			Enabled:      true,
			Store:        responsesstore.NewMemoryStore(0),
			ModelMap:     map[string]string{"gpt-5-codex": "glm-5.2"},
			DefaultModel: "",
		},
	})
	req := httptest.NewRequest("POST", "/v1/responses", strings.NewReader(`{"model":"gpt-5-codex","input":"hi"}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	// 断言落在「上游实际收到的请求体」上：model 必须已被映射。
	sent := up.bodyAt(0)
	if sent == nil || sent["model"] != "glm-5.2" {
		t.Errorf("upstream received model=%v, want glm-5.2 (mapped)", sent["model"])
	}
}

func TestResponsesIncrementalFill(t *testing.T) {
	// 第一次响应返回 function_call，第二次请求带 previous_response_id + call_output。
	store := responsesstore.NewMemoryStore(0)
	toolSSE := `data: {"id":"chatcmpl-tool","object":"chat.completion.chunk","created":1753600000,"model":"glm-5.2","choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"shell","arguments":"{\"cmd\":\"ls\"}"}}]}}]}` + "\n\n" +
		`data: {"id":"chatcmpl-tool","object":"chat.completion.chunk","created":1753600000,"model":"glm-5.2","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}` + "\n\n" +
		"data: [DONE]\n\n"

	up := newCapturingFakeUpstream(t, func(call int) (int, string, bool) {
		if call == 0 {
			return 200, toolSSE, true // 第一次：返回带 function_call 的工具流
		}
		return 200, sseOK, true
	})
	h := NewHandler(Config{
		Pool:      testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream:  up.Client,
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

	// 第二次：增量。断言上游收到的请求体里被恢复出 assistant.tool_calls
	// （这是 store 存在的全部意义——此前该测试从不检查这一点）。
	body2 := `{"model":"glm-5.2","previous_response_id":"` + respID + `","input":[{"type":"function_call_output","call_id":"call_1","output":"file1"}]}`
	req2 := httptest.NewRequest("POST", "/v1/responses", strings.NewReader(body2))
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, req2)
	if rec2.Code != 200 {
		t.Fatalf("code=%d body=%s", rec2.Code, rec2.Body)
	}
	sent2 := up.bodyAt(1)
	if sent2 == nil {
		t.Fatal("second request never reached upstream")
	}
	// 补全会把恢复的 assistant function_call 以 pending → flush 方式并进
	// messages：上游 body 必含 name=shell 的 tool_call，且紧邻 tool 结果。
	msgs, _ := sent2["messages"].([]any)
	sawToolCall := false
	for _, m := range msgs {
		msg, ok := m.(map[string]any)
		if !ok {
			continue
		}
		if tcs, ok := msg["tool_calls"].([]any); ok && len(tcs) > 0 {
			sawToolCall = true
			tc, _ := tcs[0].(map[string]any)
			fn, _ := tc["function"].(map[string]any)
			if fn["name"] != "shell" {
				t.Errorf("restored tool call name=%v, want shell", fn["name"])
			}
		}
	}
	if !sawToolCall {
		t.Errorf("restored function_call missing in forwarded body: %v", sent2)
	}
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

// TestTakeSSEBlockCRLF SSE 分帧必须同时支持 LF（\n\n）与 CRLF（\r\n\r\n）——
// SSE 规范两者皆合法，第三方中转常见 CRLF；只按 \n\n 切会让整条流凑不出块、
// finish() 时整体静默丢弃（该修复曾无回归测试，此处锁定）。
func TestTakeSSEBlockCRLF(t *testing.T) {
	lfBlock := "data: {\"a\":1}\n\nevent: x\ndata: {\"b\":2}\n\n"
	crlfBlock := "data: {\"a\":1}\r\n\r\nevent: x\r\ndata: {\"b\":2}\r\n\r\n"
	mixed := "data: {\"a\":1}\r\n\r\ndata: {\"b\":2}\n\n"

	for name, body := range map[string]string{"LF": lfBlock, "CRLF": crlfBlock, "mixed": mixed} {
		buf := bytes.NewBufferString(body)
		var blocks []string
		for {
			blk, ok := takeSSEBlock(buf)
			if !ok {
				break
			}
			blocks = append(blocks, blk)
		}
		if len(blocks) != 2 {
			t.Errorf("%s: blocks=%d (%v), want 2", name, len(blocks), blocks)
			continue
		}
		for i, want := range []string{"{\"a\":1}", "{\"b\":2}"} {
			if got := extractDataLine(blocks[i]); !strings.Contains(got, want) {
				t.Errorf("%s: block%d data=%q, want containing %s", name, i, got, want)
			}
		}
	}
}

// TestAnthropicWriterCRLFStream 端到端：CRLF 分帧的上游 SSE 流经 anthropicWriter
// 仍完整产出 Anthropic 事件（此前会整流丢弃、发空消息）。
func TestAnthropicWriterCRLFStream(t *testing.T) {
	dst := httptest.NewRecorder()
	w := newAnthropicWriter(dst)
	dst.Header().Set("Content-Type", "text/event-stream")

	crlf := "data: {\"id\":\"c1\",\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"好\"}}]}\r\n\r\n" +
		"data: {\"id\":\"c1\",\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":1,\"total_tokens\":2}}\r\n\r\n" +
		"data: [DONE]\r\n\r\n"
	if _, err := w.Write([]byte(crlf)); err != nil {
		t.Fatalf("write: %v", err)
	}
	w.finish()

	body := dst.Body.String()
	for _, want := range []string{"event: message_start", "event: content_block_delta", "event: message_stop", `"text":"好"`} {
		if !strings.Contains(body, want) {
			t.Errorf("CRLF 流缺 %q\n%s", want, body)
		}
	}
}
