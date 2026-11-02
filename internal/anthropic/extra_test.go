package anthropic

import (
	"encoding/json"
	"strings"
	"testing"
)

// 上游用 reasoning 字段（OpenRouter/Kimi）时思维链不能丢。
func TestStreamReasoningAlias(t *testing.T) {
	st := NewStreamState()
	var raw []byte
	raw = append(raw, st.HandleChunk(chatChunk(map[string]any{"reasoning": "think-alias"}, ""))...)
	raw = append(raw, st.HandleChunk(chatChunk(map[string]any{"content": "answer"}, "stop"))...)
	raw = append(raw, st.Finalize()...)
	events := parseAnthropicSSE(t, raw)
	found := false
	for _, e := range events {
		if e.event == "content_block_delta" {
			delta, _ := e.data["delta"].(map[string]any)
			if delta["type"] == "thinking_delta" && delta["thinking"] == "think-alias" {
				found = true
			}
		}
	}
	if !found {
		t.Fatalf("reasoning 字段未转成 thinking: %s", raw)
	}
}

// 流式内联 think 不得进入正文。
func TestStreamStripsInlineThink(t *testing.T) {
	st := NewStreamState()
	var raw []byte
	for _, part := range []string{"<think>", "long thought", "</think>", "answer text"} {
		raw = append(raw, st.HandleChunk(chatChunk(map[string]any{"content": part}, ""))...)
	}
	raw = append(raw, st.Finalize()...)
	s := string(raw)
	if strings.Contains(s, "<think>") {
		t.Errorf("内联 think 泄漏: %s", s)
	}
	if !strings.Contains(s, "long thought") || !strings.Contains(s, "answer text") {
		t.Errorf("think/正文丢失: %s", s)
	}
}

// messages 里的 system 角色必须保留（Claude Code 新版本会发）。
func TestSystemRolePreserved(t *testing.T) {
	out := mustToChat(t, `{"model":"m","max_tokens":1,"messages":[
		{"role":"user","content":"hi"},
		{"role":"system","content":"be terse"},
		{"role":"assistant","content":"ok"}]}`)
	msgs, _ := out["messages"].([]any)
	found := false
	for _, raw := range msgs {
		msg, _ := raw.(map[string]any)
		if msg["role"] == "system" && msg["content"] == "be terse" {
			found = true
		}
	}
	if !found {
		t.Errorf("mid-conversation system 被改写: %v", msgs)
	}
}

// billing 头只剥开头那一行，后续同前缀的用户文本保留。
func TestBillingHeaderOnlyFirstLine(t *testing.T) {
	out := mustToChat(t, `{"model":"m","max_tokens":1,"system":
		"x-anthropic-billing-hdr: v=1\nhello\nx-anthropic-billing-hdr: user text",
		"messages":[{"role":"user","content":"hi"}]}`)
	msgs, _ := out["messages"].([]any)
	first, _ := msgs[0].(map[string]any)
	content, _ := first["content"].(string)
	if strings.HasPrefix(content, "x-anthropic-billing-hdr") {
		t.Errorf("首行未剥离: %q", content)
	}
	if !strings.Contains(content, "hello") || !strings.Contains(content, "user text") {
		t.Errorf("正文被误删: %q", content)
	}
}

// 嵌套 JSON 字符串里的媒体也要搬走，残留 base64 被裁剪。
func TestToolResultNestedMedia(t *testing.T) {
	longB64 := strings.Repeat("A", 20*1024)
	jsonContent := `{"content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"abc"}},{"type":"text","text":"` + longB64 + `"}]}`
	bodyJSON, err := json.Marshal(map[string]any{
		"model": "m", "max_tokens": 1,
		"messages": []any{
			map[string]any{"role": "assistant", "content": []any{
				map[string]any{"type": "tool_use", "id": "t1", "name": "f", "input": map[string]any{}},
			}},
			map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "tool_result", "tool_use_id": "t1", "content": jsonContent},
			}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	out := mustToChat(t, string(bodyJSON))
	msgs, _ := out["messages"].([]any)
	toolMsg, _ := msgs[1].(map[string]any)
	content, _ := toolMsg["content"].(string)
	if strings.Contains(content, longB64) {
		t.Error("残留 base64 未裁剪")
	}
	if !strings.Contains(content, "omitted") {
		t.Errorf("缺少裁剪标记: %s", content)
	}
	mediaMsg, _ := msgs[2].(map[string]any)
	parts, _ := mediaMsg["content"].([]any)
	if len(parts) != 2 {
		t.Fatalf("媒体未搬走: %v", mediaMsg)
	}
}

// 只有 thinking 的 assistant 消息整条丢弃（不产出无 content 的消息）。
func TestPureThinkingMessageDropped(t *testing.T) {
	out := mustToChat(t, `{"model":"m","max_tokens":1,"messages":[
		{"role":"assistant","content":[{"type":"thinking","thinking":"only thought"}]},
		{"role":"user","content":"hi"}]}`)
	msgs, _ := out["messages"].([]any)
	for _, raw := range msgs {
		msg, _ := raw.(map[string]any)
		if msg["role"] == "assistant" {
			t.Errorf("纯 thinking 消息应丢弃: %v", msg)
		}
	}
}

// 旧式 message.function_call 回退成 tool_use 块。
func TestLegacyFunctionCallToToolUse(t *testing.T) {
	resp := chatResp(`{"id":"c","model":"m","choices":[{"finish_reason":"function_call",
		"message":{"role":"assistant","content":"","function_call":{"name":"f","arguments":"{\"a\":1}"}}}]}`)
	msg, err := FromChat(resp)
	if err != nil {
		t.Fatal(err)
	}
	content, _ := msg["content"].([]any)
	found := false
	for _, raw := range content {
		block, _ := raw.(map[string]any)
		if block["type"] == "tool_use" && block["name"] == "f" {
			input, _ := block["input"].(map[string]any)
			if input["a"] != float64(1) {
				t.Errorf("input=%v", input)
			}
			found = true
		}
	}
	if !found {
		t.Fatalf("legacy function_call 未转 tool_use: %v", content)
	}
	if msg["stop_reason"] != "tool_use" {
		t.Errorf("stop_reason=%v", msg["stop_reason"])
	}
}

// message_start.usage 用首帧 usage 填 input/cache 桶。
func TestMessageStartUsageFromFirstChunk(t *testing.T) {
	st := NewStreamState()
	first := chatChunk(map[string]any{"content": "hi"}, "")
	first["usage"] = map[string]any{
		"prompt_tokens":         float64(100),
		"completion_tokens":     float64(0),
		"prompt_tokens_details": map[string]any{"cached_tokens": float64(40)},
	}
	var raw []byte
	raw = append(raw, st.HandleChunk(first)...)
	raw = append(raw, st.HandleChunk(chatChunk(map[string]any{}, "stop"))...)
	raw = append(raw, st.Finalize()...)
	events := parseAnthropicSSE(t, raw)
	if events[0].event != "message_start" {
		t.Fatalf("first event=%s", events[0].event)
	}
	msg, _ := events[0].data["message"].(map[string]any)
	usage, _ := msg["usage"].(map[string]any)
	if numToU64(usage["input_tokens"]) != 60 {
		t.Errorf("input_tokens=%v, want 60", usage["input_tokens"])
	}
	if numToU64(usage["cache_read_input_tokens"]) != 40 {
		t.Errorf("cache_read=%v", usage["cache_read_input_tokens"])
	}
}
