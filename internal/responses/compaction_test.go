package responses

import (
	"strings"
	"testing"
)

// messageText 提取 Chat 消息的文本（字符串或 content parts）。
func messageText(msg map[string]any) string {
	if text, ok := msg["content"].(string); ok {
		return text
	}
	var parts []string
	raw, _ := msg["content"].([]any)
	for _, item := range raw {
		part, _ := item.(map[string]any)
		if text := rawString(part, "text"); text != "" {
			parts = append(parts, text)
		}
	}
	return strings.Join(parts, "\n")
}

func compactionTestBody() []byte {
	return []byte(`{
	  "model":"gpt-5.5",
	  "stream":true,
	  "tools":[{"type":"function","name":"exec_command","parameters":{"type":"object"}}],
	  "tool_choice":"auto",
	  "parallel_tool_calls":true,
	  "response_format":{"type":"json_object"},
	  "input":[
	    {"type":"message","role":"user","content":"do the work"},
	    {"type":"compaction_trigger"}
	  ]}`)
}

// 压缩回合：触发条目变成压缩提示词；不下发工具/结构化疗输出字段。
func TestCompactionRequestConversion(t *testing.T) {
	raw, ctx, err := ToChat(compactionTestBody())
	if err != nil {
		t.Fatal(err)
	}
	if !ctx.IsCompaction() {
		t.Fatal("ctx.IsCompaction=false")
	}
	out := decode(t, raw)
	if _, has := out["tools"]; has {
		t.Errorf("压缩回合不应下发 tools: %v", out["tools"])
	}
	for _, key := range []string{"tool_choice", "parallel_tool_calls", "response_format"} {
		if _, has := out[key]; has {
			t.Errorf("压缩回合不应下发 %s: %v", key, out[key])
		}
	}
	msgs, _ := out["messages"].([]any)
	last, _ := msgs[len(msgs)-1].(map[string]any)
	if !strings.Contains(messageText(last), "CONTEXT CHECKPOINT COMPACTION") {
		t.Errorf("触发条目未换成压缩提示词: %v", last)
	}
}

// 流式压缩回合：交回唯一 compaction item（encrypted_content 可回放解出摘要）。
func TestCompactionStreamOutput(t *testing.T) {
	_, ctx, err := ToChat(compactionTestBody())
	if err != nil {
		t.Fatal(err)
	}
	s := NewStreamState(ctx)
	var buf strings.Builder
	buf.Write(s.HandleChunk(chunk(map[string]any{"content": "handoff summary"}, "", nil)))
	buf.Write(s.HandleChunk(chunk(map[string]any{}, "stop", nil)))
	s.MarkDone()
	buf.Write(s.Finalize())
	out := buf.String()
	if !strings.Contains(out, `"type":"compaction"`) {
		t.Fatalf("缺少 compaction item:\n%s", out)
	}
	if !strings.Contains(out, compactionEnvelopePrefix) {
		t.Errorf("encrypted_content 未使用本项目信封:\n%s", out)
	}
	if strings.Contains(out, `"type":"function_call"`) {
		t.Errorf("压缩回合不应产出工具调用")
	}

	// 回放：下一次请求 history 里的压缩条目解回摘要正文（用户消息）。
	enc := extractEncryptedContent(t, s.OutputItems())
	body := []byte(`{"model":"gpt-5.5","input":[
	  {"type":"compaction","encrypted_content":"` + enc + `"},
	  {"type":"message","role":"user","content":"continue"}]}`)
	raw, _, err := ToChat(body)
	if err != nil {
		t.Fatal(err)
	}
	replay := decode(t, raw)
	msgs, _ := replay["messages"].([]any)
	if msgs == nil || len(msgs) < 2 {
		t.Fatalf("messages=%v", msgs)
	}
	first, _ := msgs[0].(map[string]any)
	if first["role"] != "user" || !strings.Contains(messageText(first), "handoff summary") {
		t.Errorf("历史压缩条目未解回摘要: %v", first)
	}
	if !strings.Contains(messageText(first), "Another language model started") {
		t.Errorf("缺少 SUMMARY_PREFIX: %v", first)
	}
}

func extractEncryptedContent(t *testing.T, items []any) string {
	t.Helper()
	for _, raw := range items {
		item, _ := raw.(map[string]any)
		if stringField(item, "type") == "compaction" {
			return rawString(item, "encrypted_content")
		}
	}
	t.Fatal("no compaction item")
	return ""
}

// 非流式压缩回合同样产出 compaction item。
func TestCompactionNonStreamOutput(t *testing.T) {
	_, ctx, err := ToChat(compactionTestBody())
	if err != nil {
		t.Fatal(err)
	}
	resp := map[string]any{
		"id": "chatcmpl-1", "model": "glm-5.2", "created": float64(1),
		"choices": []any{map[string]any{
			"message":       map[string]any{"role": "assistant", "content": "summary body"},
			"finish_reason": "stop",
		}},
	}
	out, err := FromChat(resp, ctx)
	if err != nil {
		t.Fatal(err)
	}
	items, _ := out["output"].([]any)
	found := false
	for _, raw := range items {
		item, _ := raw.(map[string]any)
		if stringField(item, "type") == "compaction" {
			found = true
		}
	}
	if !found {
		t.Fatalf("non-stream compaction missing item: %v", items)
	}
}

// 非压缩回合保持原有响应（防回归）。
func TestNonCompactionUnaffected(t *testing.T) {
	ctx := NewToolContext()
	resp := map[string]any{
		"choices": []any{map[string]any{
			"message":       map[string]any{"role": "assistant", "content": "hello"},
			"finish_reason": "stop",
		}},
	}
	out, err := FromChat(resp, ctx)
	if err != nil {
		t.Fatal(err)
	}
	if out["status"] != "completed" {
		t.Errorf("status=%v", out["status"])
	}
}
