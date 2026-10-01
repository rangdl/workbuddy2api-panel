package anthropic

import (
	"encoding/json"
	"testing"
)

func decode(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return out
}

func mustToChat(t *testing.T, body string) map[string]any {
	t.Helper()
	raw, err := ToChat([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	return decode(t, raw)
}

// ---- 请求侧 ----

// system 字符串形态 → 单条 system 消息。
func TestSystemString(t *testing.T) {
	out := mustToChat(t, `{"model":"m","system":"You are Claude Code.","max_tokens":100,"messages":[{"role":"user","content":"hi"}]}`)
	msgs, _ := out["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("messages len=%d", len(msgs))
	}
	first, _ := msgs[0].(map[string]any)
	if first["role"] != "system" || first["content"] != "You are Claude Code." {
		t.Errorf("system message=%v", first)
	}
	if out["max_tokens"] != float64(100) {
		t.Errorf("max_tokens=%v (Anthropic 必填字段必须透传)", out["max_tokens"])
	}
}

// system blocks 数组 → 合并为一条 system 消息（prompt cache 字节稳定）。
func TestSystemBlocksMerged(t *testing.T) {
	out := mustToChat(t, `{"model":"m","system":[
		{"type":"text","text":"Part one."},
		{"type":"text","text":"Part two."}],
		"max_tokens":1,"messages":[{"role":"user","content":"hi"}]}`)
	msgs, _ := out["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("messages len=%d", len(msgs))
	}
	first, _ := msgs[0].(map[string]any)
	if first["content"] != "Part one.\nPart two." {
		t.Errorf("merged system=%v", first["content"])
	}
}

// CC 的 billing 指纹行必须从 system 剥离（sanitize 层管不到顶层 system 字段）。
func TestSystemBillingHeaderStripped(t *testing.T) {
	body := `{"model":"m","system":"x-anthropic-billing-hdr: cc_x=1; q=2;\nReal instructions here.","max_tokens":1,"messages":[{"role":"user","content":"hi"}]}`
	out := mustToChat(t, body)
	msgs, _ := out["messages"].([]any)
	first, _ := msgs[0].(map[string]any)
	content, _ := first["content"].(string)
	if !contains(content, "Real instructions here.") {
		t.Errorf("real content lost: %q", content)
	}
	if containsFold(content, "billing-hdr") {
		t.Errorf("billing fingerprint leaked: %q", content)
	}
}

// user 文本块 → 字符串 content（prompt cache 友好）。
func TestUserTextBlocks(t *testing.T) {
	out := mustToChat(t, `{"model":"m","max_tokens":1,"messages":[
		{"role":"user","content":[{"type":"text","text":"line one"},{"type":"text","text":"line two"}]}]}`)
	msgs, _ := out["messages"].([]any)
	if len(msgs) != 1 {
		t.Fatalf("messages len=%d", len(msgs))
	}
	m, _ := msgs[0].(map[string]any)
	if m["role"] != "user" || m["content"] != "line one\nline two" {
		t.Errorf("message=%v", m)
	}
}

// base64 image 块 → image_url part（data URL），content 保留数组形态。
func TestImageBlock(t *testing.T) {
	out := mustToChat(t, `{"model":"m","max_tokens":1,"messages":[
		{"role":"user","content":[
			{"type":"text","text":"look"},
			{"type":"image","source":{"type":"base64","media_type":"image/png","data":"aWNv"}}]}]}`)
	msgs, _ := out["messages"].([]any)
	m, _ := msgs[0].(map[string]any)
	parts, ok := m["content"].([]any)
	if !ok || len(parts) != 2 {
		t.Fatalf("content should stay as array with 2 parts: %v", m["content"])
	}
	img, _ := parts[1].(map[string]any)
	iurl, _ := img["image_url"].(map[string]any)
	if iurl["url"] != "data:image/png;base64,aWNv" {
		t.Errorf("image url=%v", iurl["url"])
	}
}

// thinking 块 → reasoning_content；redacted_thinking → 非空占位。
func TestThinkingBlocksToReasoningContent(t *testing.T) {
	out := mustToChat(t, `{"model":"m","max_tokens":1,"messages":[
		{"role":"assistant","content":[
			{"type":"thinking","thinking":"I should answer politely."},
			{"type":"redacted_thinking","data":"enc"},
			{"type":"text","text":"Hello"}]},
		{"role":"user","content":"again"}]}`)
	msgs, _ := out["messages"].([]any)
	a, _ := msgs[0].(map[string]any)
	if a["role"] != "assistant" {
		t.Fatalf("first message=%v", a)
	}
	if a["content"] != "Hello" {
		t.Errorf("content=%v", a["content"])
	}
	rc, _ := a["reasoning_content"].(string)
	if !contains(rc, "I should answer politely.") || !contains(rc, "[redacted thinking]") {
		t.Errorf("reasoning_content=%q", rc)
	}
}

// thinking 配置 → reasoning_effort 分档；未开 thinking 不注入。
func TestThinkingEffortMapping(t *testing.T) {
	out := mustToChat(t, `{"model":"m","max_tokens":1,"thinking":{"type":"enabled","budget_tokens":10240},"messages":[{"role":"user","content":"hi"}]}`)
	if out["reasoning_effort"] != "high" {
		t.Errorf("budget 10240 → effort=%v, want high", out["reasoning_effort"])
	}
	out = mustToChat(t, `{"model":"m","max_tokens":1,"thinking":{"type":"enabled","budget_tokens":2048},"messages":[{"role":"user","content":"hi"}]}`)
	if out["reasoning_effort"] != "medium" {
		t.Errorf("budget 2048 → effort=%v, want medium", out["reasoning_effort"])
	}
	out = mustToChat(t, `{"model":"m","max_tokens":1,"messages":[{"role":"user","content":"hi"}]}`)
	if _, has := out["reasoning_effort"]; has {
		t.Errorf("no thinking config should not inject reasoning_effort: %v", out["reasoning_effort"])
	}
}

// stop_sequences → stop 改名透传。
func TestStopSequencesRenamed(t *testing.T) {
	out := mustToChat(t, `{"model":"m","max_tokens":1,"stop_sequences":["END","STOP"],"messages":[{"role":"user","content":"hi"}]}`)
	stop, ok := out["stop"].([]any)
	if !ok || len(stop) != 2 {
		t.Errorf("stop=%v, want renamed from stop_sequences", out["stop"])
	}
	if _, has := out["stop_sequences"]; has {
		t.Error("stop_sequences should not leak to chat body")
	}
}

// 块级 cache_control 不泄漏到 Chat 体（Chat 无对应语义）。
func TestCacheControlDropped(t *testing.T) {
	out := mustToChat(t, `{"model":"m","max_tokens":1,"system":[
		{"type":"text","text":"sys","cache_control":{"type":"ephemeral"}}],
		"messages":[{"role":"user","content":[{"type":"text","text":"u","cache_control":{"type":"ephemeral"}}]}]}`)
	raw, _ := json.Marshal(out)
	if contains(string(raw), "cache_control") {
		t.Errorf("cache_control leaked: %s", raw)
	}
}

// ---- 响应侧 ----

func chatResp(body string) map[string]any {
	var out map[string]any
	json.Unmarshal([]byte(body), &out)
	return out
}

// 纯文本响应 → text 块 + end_turn。
func TestFromChatTextOnly(t *testing.T) {
	resp := chatResp(`{"id":"chat-1","model":"m","choices":[{"finish_reason":"stop",
		"message":{"role":"assistant","content":"Hello there"}}],"usage":{"prompt_tokens":10,"completion_tokens":5}}`)
	msg, err := FromChat(resp)
	if err != nil {
		t.Fatal(err)
	}
	if msg["type"] != "message" || msg["role"] != "assistant" {
		t.Errorf("envelope=%v", msg)
	}
	if msg["stop_reason"] != "end_turn" {
		t.Errorf("stop_reason=%v", msg["stop_reason"])
	}
	if msg["id"] != "msg_chat-1" {
		t.Errorf("id=%v", msg["id"])
	}
	content, _ := msg["content"].([]any)
	if len(content) != 1 {
		t.Fatalf("content=%v", content)
	}
	block, _ := content[0].(map[string]any)
	if block["type"] != "text" || block["text"] != "Hello there" {
		t.Errorf("text block=%v", block)
	}
}

// reasoning_content → thinking 块在前；tool_calls → tool_use 块（input 反序列化为对象）。
func TestFromChatThinkingAndToolUse(t *testing.T) {
	resp := chatResp(`{"id":"chat-2","model":"m","choices":[{"finish_reason":"tool_calls",
		"message":{"role":"assistant","reasoning_content":"let me check","content":"",
		"tool_calls":[{"id":"call_9","type":"function","function":{"name":"shell","arguments":"{\"cmd\":\"ls\"}"}}]}}]}`)
	msg, err := FromChat(resp)
	if err != nil {
		t.Fatal(err)
	}
	if msg["stop_reason"] != "tool_use" {
		t.Errorf("stop_reason=%v", msg["stop_reason"])
	}
	content, _ := msg["content"].([]any)
	if len(content) != 2 {
		t.Fatalf("content=%v", content)
	}
	th, _ := content[0].(map[string]any)
	if th["type"] != "thinking" || th["thinking"] != "let me check" {
		t.Errorf("thinking block=%v", th)
	}
	tu, _ := content[1].(map[string]any)
	if tu["type"] != "tool_use" || tu["name"] != "shell" || tu["id"] != "call_9" {
		t.Errorf("tool_use block=%v", tu)
	}
	input, _ := tu["input"].(map[string]any)
	if input["cmd"] != "ls" {
		t.Errorf("input must be deserialized object: %v", input)
	}
}

// finish_reason=length → max_tokens。
func TestStopReasonLength(t *testing.T) {
	resp := chatResp(`{"id":"c","model":"m","choices":[{"finish_reason":"length","message":{"role":"assistant","content":"x"}}]}`)
	msg, _ := FromChat(resp)
	if msg["stop_reason"] != "max_tokens" {
		t.Errorf("stop_reason=%v", msg["stop_reason"])
	}
}

// 缺 finish_reason 但有 tool_calls → 强制 tool_use。
func TestStopReasonForcedToolUse(t *testing.T) {
	resp := chatResp(`{"id":"c","model":"m","choices":[{"finish_reason":"",
		"message":{"role":"assistant","content":"","tool_calls":[{"id":"t1","type":"function","function":{"name":"f","arguments":"{}"}}]}}]}`)
	msg, _ := FromChat(resp)
	if msg["stop_reason"] != "tool_use" {
		t.Errorf("stop_reason=%v", msg["stop_reason"])
	}
}

// usage 缓存桶减法：input = prompt − cached − cache_write；零桶省略。
func TestUsageCacheArithmetic(t *testing.T) {
	resp := chatResp(`{"id":"c","model":"m","choices":[{"finish_reason":"stop","message":{"role":"assistant","content":"x"}}],
		"usage":{"prompt_tokens":1000,"completion_tokens":50,
		"prompt_tokens_details":{"cached_tokens":300,"cache_write_tokens":100}}}`)
	msg, _ := FromChat(resp)
	usage, _ := msg["usage"].(map[string]any)
	if usage["input_tokens"] != uint64(600) {
		t.Errorf("input_tokens=%v, want 600 (1000-300-100)", usage["input_tokens"])
	}
	if usage["cache_read_input_tokens"] != uint64(300) {
		t.Errorf("cache_read=%v", usage["cache_read_input_tokens"])
	}
	if usage["cache_creation_input_tokens"] != uint64(100) {
		t.Errorf("cache_creation=%v", usage["cache_creation_input_tokens"])
	}
}

// 无缓存时无缓存桶字段（省略而非 0）。
func TestUsageNoCacheFieldsOmitted(t *testing.T) {
	resp := chatResp(`{"id":"c","model":"m","choices":[{"finish_reason":"stop","message":{"role":"assistant","content":"x"}}],
		"usage":{"prompt_tokens":10,"completion_tokens":5}}`)
	msg, _ := FromChat(resp)
	usage, _ := msg["usage"].(map[string]any)
	if _, has := usage["cache_read_input_tokens"]; has {
		t.Errorf("cache_read should be omitted: %v", usage)
	}
	if usage["input_tokens"] != uint64(10) {
		t.Errorf("input_tokens=%v", usage["input_tokens"])
	}
}

// 前置 think 块拆分：思维链进 thinking 块，正文进 text 块。
func TestLeadingThinkBlockSplit(t *testing.T) {
	resp := chatResp(`{"id":"c","model":"m","choices":[{"finish_reason":"stop",
		"message":{"role":"assistant","content":"<think>hmm nice</think>The answer."}}]}`)
	msg, _ := FromChat(resp)
	content, _ := msg["content"].([]any)
	if len(content) != 2 {
		t.Fatalf("content=%v", content)
	}
	th, _ := content[0].(map[string]any)
	if th["type"] != "thinking" || th["thinking"] != "hmm nice" {
		t.Errorf("thinking=%v", th)
	}
	tx, _ := content[1].(map[string]any)
	if tx["text"] != "The answer." {
		t.Errorf("text=%v", tx)
	}
}

// 坏 JSON arguments → {_raw} 保真。
func TestToolInputBadJSONFallback(t *testing.T) {
	resp := chatResp(`{"id":"c","model":"m","choices":[{"finish_reason":"tool_calls",
		"message":{"role":"assistant","content":"","tool_calls":[{"id":"t","type":"function","function":{"name":"f","arguments":"not-json"}}]}}]}`)
	msg, _ := FromChat(resp)
	content, _ := msg["content"].([]any)
	tu, _ := content[0].(map[string]any)
	input, _ := tu["input"].(map[string]any)
	if input["_raw"] != "not-json" {
		t.Errorf("input=%v, want _raw fallback", input)
	}
}

// ---- 错误形状 ----

func TestErrorToMessages(t *testing.T) {
	out := ErrorToMessages([]byte(`{"error":{"message":"boom","type":"invalid_request_error","code":"x"}}`), 0)
	if out["type"] != "error" {
		t.Errorf("envelope=%v", out)
	}
	errObj, _ := out["error"].(map[string]any)
	if errObj["type"] != "invalid_request_error" || errObj["message"] != "boom" {
		t.Errorf("error=%v", errObj)
	}
	// 非 JSON 体 → api_error 原文。
	out = ErrorToMessages([]byte(`plain gateway text`), 0)
	errObj, _ = out["error"].(map[string]any)
	if errObj["type"] != "api_error" {
		t.Errorf("plain body type=%v", errObj["type"])
	}
}

// TestErrorToMessagesStatusFirst HTTP 状态是 error.type 的第一判定键：网关自家
// 429 错误体写的是 type=api_error + code=rate_limit_exceeded，必须按状态映射为
// rate_limit_error（CC 据此做限流退避；此前只看字符串恒报 api_error）。
func TestErrorToMessagesStatusFirst(t *testing.T) {
	body := []byte(`{"error":{"message":"rate limited: all accounts are cooling down","type":"api_error","code":"rate_limit_exceeded"}}`)
	out := ErrorToMessages(body, 429)
	errObj, _ := out["error"].(map[string]any)
	if errObj["type"] != "rate_limit_error" {
		t.Errorf("429 type=%v, want rate_limit_error", errObj["type"])
	}
	// 503（no_healthy_account）→ overloaded_error。
	out = ErrorToMessages([]byte(`{"error":{"message":"no healthy account","type":"api_error","code":"no_healthy_account"}}`), 503)
	errObj, _ = out["error"].(map[string]any)
	if errObj["type"] != "overloaded_error" {
		t.Errorf("503 type=%v, want overloaded_error", errObj["type"])
	}
	// 401 → authentication_error。
	out = ErrorToMessages([]byte(`{"error":{"message":"bad key","type":"api_error","code":"invalid_api_key"}}`), 401)
	errObj, _ = out["error"].(map[string]any)
	if errObj["type"] != "authentication_error" {
		t.Errorf("401 type=%v, want authentication_error", errObj["type"])
	}
	// 状态未知（0）时保留上游自带的 Anthropic 规范 type；OpenAI 型 type 走字符串映射。
	out = ErrorToMessages([]byte(`{"error":{"message":"m","type":"invalid_request_error"}}`), 0)
	errObj, _ = out["error"].(map[string]any)
	if errObj["type"] != "invalid_request_error" {
		t.Errorf("passthrough type=%v", errObj["type"])
	}
}

// TestErrorTypeForCode Classify 风格稳定 code → Anthropic closed enum。
func TestErrorTypeForCode(t *testing.T) {
	cases := map[string]string{
		"invalid_api_key":     "authentication_error",
		"permission_denied":   "permission_error",
		"model_not_found":     "not_found_error",
		"rate_limit_exceeded": "rate_limit_error",
		"invalid_request":     "invalid_request_error",
		"upstream_error":      "api_error",
		"whatever":            "api_error",
	}
	for code, want := range cases {
		if got := ErrorTypeForCode(code); got != want {
			t.Errorf("ErrorTypeForCode(%q)=%q, want %q", code, got, want)
		}
	}
}

// ---- 测试小工具 ----

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || len(sub) == 0 || indexOf(s, sub) >= 0)
}

func containsFold(s, sub string) bool {
	return contains(toLower(s), toLower(sub))
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

func toLower(s string) string {
	b := []byte(s)
	for i := range b {
		if b[i] >= 'A' && b[i] <= 'Z' {
			b[i] += 'a' - 'A'
		}
	}
	return string(b)
}

// ---- A2：工具全链路 ----

// tools → Chat tools：input_schema→parameters、description 缺失省略（非 null）。
func TestToolsToChat(t *testing.T) {
	out := mustToChat(t, `{"model":"m","max_tokens":1,"messages":[{"role":"user","content":"hi"}],
		"tools":[
			{"name":"get_weather","description":"Get weather","input_schema":{"type":"object","properties":{"city":{"type":"string"}}}},
			{"name":"no_desc","input_schema":{"type":"object"}},
			{"type":"web_search_20250305","name":"server_search","max_uses":3}
		]}`)
	tools, _ := out["tools"].([]any)
	if len(tools) != 2 {
		t.Fatalf("tools=%v (server tool must be dropped)", tools)
	}
	first, _ := tools[0].(map[string]any)
	fn, _ := first["function"].(map[string]any)
	if fn["name"] != "get_weather" {
		t.Errorf("name=%v", fn["name"])
	}
	params, _ := fn["parameters"].(map[string]any)
	if params["type"] != "object" {
		t.Errorf("parameters=%v", params)
	}
	second, _ := tools[1].(map[string]any)
	fn2, _ := second["function"].(map[string]any)
	if _, has := fn2["description"]; has {
		t.Errorf("missing description must be omitted (not null): %v", fn2)
	}
	raw, _ := json.Marshal(out)
	if contains(string(raw), "web_search") {
		t.Error("server tool leaked to chat body")
	}
}

// tool_choice 四态映射。
func TestToolChoiceMapping(t *testing.T) {
	cases := []struct {
		in   string
		want any
	}{
		{`{"type":"auto"}`, "auto"},
		{`{"type":"any"}`, "required"},
		{`{"type":"none"}`, "none"},
	}
	for _, c := range cases {
		out := mustToChat(t, `{"model":"m","max_tokens":1,"messages":[{"role":"user","content":"hi"}],"tool_choice":`+c.in+`}`)
		if out["tool_choice"] != c.want {
			t.Errorf("%s → %v, want %v", c.in, out["tool_choice"], c.want)
		}
	}
	out := mustToChat(t, `{"model":"m","max_tokens":1,"messages":[{"role":"user","content":"hi"}],"tool_choice":{"type":"tool","name":"shell"}}`)
	tc, _ := out["tool_choice"].(map[string]any)
	fn, _ := tc["function"].(map[string]any)
	if fn["name"] != "shell" {
		t.Errorf("tool choice=%v", out["tool_choice"])
	}
}

// assistant tool_use 块 → tool_calls（input 对象→规范化字符串）；后续 user 的
// tool_result 块 → 独立 tool 消息（并行回喂形态）。
func TestToolUseResultRoundTrip(t *testing.T) {
	out := mustToChat(t, `{"model":"m","max_tokens":1,"messages":[
		{"role":"user","content":"run both"},
		{"role":"assistant","content":[
			{"type":"tool_use","id":"toolu_1","name":"shell","input":{"cmd":"ls"}},
			{"type":"tool_use","id":"toolu_2","name":"shell","input":{"cmd":"pwd"}}]},
		{"role":"user","content":[
			{"type":"tool_result","tool_use_id":"toolu_1","content":"dir-a"},
			{"type":"tool_result","tool_use_id":"toolu_2","content":"dir-b"}]}]}`)
	msgs, _ := out["messages"].([]any)
	if len(msgs) != 4 {
		t.Fatalf("messages len=%d: %v", len(msgs), msgs)
	}
	// assistant 带 tool_calls 且 content=null（规范形态）
	a, _ := msgs[1].(map[string]any)
	if a["role"] != "assistant" {
		t.Fatalf("msgs[1]=%v", a)
	}
	tcs, _ := a["tool_calls"].([]any)
	if len(tcs) != 2 {
		t.Fatalf("tool_calls=%v", a["tool_calls"])
	}
	tc1, _ := tcs[0].(map[string]any)
	if tc1["id"] != "toolu_1" {
		t.Errorf("call id=%v", tc1["id"])
	}
	fn, _ := tc1["function"].(map[string]any)
	if fn["name"] != "shell" || fn["arguments"] != `{"cmd":"ls"}` {
		t.Errorf("function=%v", fn)
	}
	if v, has := a["content"]; has && v != nil {
		t.Errorf("assistant content should be null with tool_calls, got %v", v)
	}
	// 两个独立 tool 消息
	t1, _ := msgs[2].(map[string]any)
	t2, _ := msgs[3].(map[string]any)
	if t1["role"] != "tool" || t1["tool_call_id"] != "toolu_1" || t1["content"] != "dir-a" {
		t.Errorf("tool msg1=%v", t1)
	}
	if t2["role"] != "tool" || t2["tool_call_id"] != "toolu_2" || t2["content"] != "dir-b" {
		t.Errorf("tool msg2=%v", t2)
	}
}

// tool_result 的 blocks 数组 content 提取 text 合并；image 占位。
func TestToolResultBlocksContent(t *testing.T) {
	out := mustToChat(t, `{"model":"m","max_tokens":1,"messages":[
		{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"f","input":{}}]},
		{"role":"user","content":[
			{"type":"tool_result","tool_use_id":"t1","content":[
				{"type":"text","text":"result line"},
				{"type":"image","source":{"type":"base64","media_type":"image/png","data":"x"}}]}]}]}`)
	msgs, _ := out["messages"].([]any)
	toolMsg, _ := msgs[1].(map[string]any)
	if toolMsg["content"] != "result line\n[image omitted]" {
		t.Errorf("tool content=%v", toolMsg["content"])
	}
}

// 缺 id/name 的 tool_use 丢弃；缺 tool_use_id 的 tool_result 丢弃（畸形防御）。
func TestMalformedToolBlocksDropped(t *testing.T) {
	out := mustToChat(t, `{"model":"m","max_tokens":1,"messages":[
		{"role":"assistant","content":[
			{"type":"tool_use","name":"no_id","input":{}},
			{"type":"tool_use","id":"ok1","name":"f","input":{}}]},
		{"role":"user","content":[
			{"type":"tool_result","content":"orphan"}]}]}`)
	msgs, _ := out["messages"].([]any)
	// assistant 保留（有合法调用）；orphan result 丢弃后 user 消息全空 → 整条消失。
	if len(msgs) != 1 {
		t.Fatalf("messages=%v", msgs)
	}
	a, _ := msgs[0].(map[string]any)
	if tcs, _ := a["tool_calls"].([]any); len(tcs) != 1 {
		t.Errorf("only the valid call should survive: %v", a["tool_calls"])
	}
}
