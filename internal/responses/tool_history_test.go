package responses

import (
	"encoding/json"
	"strings"
	"testing"
)

func historyCall(id, name, args string) map[string]any {
	return map[string]any{
		"role":    "assistant",
		"content": nil,
		"tool_calls": []any{map[string]any{
			"id":   id,
			"type": "function",
			"function": map[string]any{
				"name":      name,
				"arguments": args,
			},
		}},
	}
}

func historyTool(id, content string) map[string]any {
	return map[string]any{"role": "tool", "tool_call_id": id, "content": content}
}

func summarizeHistory(msgs []any) []string {
	out := make([]string, 0, len(msgs))
	for _, raw := range msgs {
		msg, _ := raw.(map[string]any)
		role, _ := msg["role"].(string)
		s := role
		if tcs, ok := msg["tool_calls"].([]any); ok {
			for _, tc := range tcs {
				call, _ := tc.(map[string]any)
				id, _ := call["id"].(string)
				s += "[call:" + id + "]"
			}
		}
		if id, ok := msg["tool_call_id"].(string); ok {
			s += "[" + id + "]"
		}
		out = append(out, s)
	}
	return out
}

// 半截 JSON 的历史调用降级为文本，配对结果折回同一条 assistant：
// 严格上游不会再因这条坏历史对之后每次重放都 400。
func TestPreserveInvalidToolCallsAsText(t *testing.T) {
	in := []any{
		historyCall("call_broken", "lookup", `{"path":"/tmp`),
		historyTool("call_broken", `{"error":"parse failed"}`),
	}
	out, n := preserveInvalidToolCallsAsText(in)
	if n != 1 {
		t.Fatalf("recovered=%d", n)
	}
	if got := summarizeHistory(out); len(got) != 1 || got[0] != "assistant" {
		t.Fatalf("messages=%v", got)
	}
	msg := out[0].(map[string]any)
	if _, has := msg["tool_calls"]; has {
		t.Error("坏调用应删除 tool_calls")
	}
	content, _ := msg["content"].(string)
	for _, want := range []string{
		"name: lookup", "call_id: call_broken", `{"path":"/tmp`,
		"Historical tool result for invalid call call_broken", "parse failed",
	} {
		if !strings.Contains(content, want) {
			t.Errorf("content 缺少 %q: %q", want, content)
		}
	}

	// 结构化 content（数组）保留原有块，降级文本作为新 text 块追加。
	arr := historyCall("c2", "lookup", "[]")
	arr["content"] = []any{map[string]any{"type": "text", "text": "Existing commentary."}}
	out2, n2 := preserveInvalidToolCallsAsText([]any{arr, historyTool("c2", "x")})
	if n2 != 1 {
		t.Fatalf("array: recovered=%d", n2)
	}
	parts, ok := out2[0].(map[string]any)["content"].([]any)
	if !ok || len(parts) != 3 {
		t.Fatalf("content 数组形态丢失（原块 + 调用说明 + 结果说明应为 3 块）: %v", out2[0])
	}
	first, _ := parts[0].(map[string]any)
	if first["text"] != "Existing commentary." {
		t.Errorf("原有块被改写: %v", parts[0])
	}
}

// 合法历史（对象/可解析为对象的字符串/"{}"）零改动。
func TestPreserveInvalidToolCallsKeepsValidHistory(t *testing.T) {
	in := []any{
		historyCall("c1", "lookup", `{"path":"/tmp"}`),
		historyTool("c1", "ok"),
		historyCall("c2", "lookup", `{}`),
		historyTool("c2", "ok"),
	}
	out, n := preserveInvalidToolCallsAsText(in)
	if n != 0 {
		t.Fatalf("recovered=%d", n)
	}
	got, _ := json.Marshal(out)
	want, _ := json.Marshal(in)
	if string(got) != string(want) {
		t.Errorf("合法历史被改动:\n got=%s\nwant=%s", got, want)
	}
}

// 后续合法调用复用同一 id 时，结果必须留给合法调用，不能被更早的坏调用吞掉。
func TestPreserveInvalidToolCallsIdReuse(t *testing.T) {
	in := []any{
		historyCall("reused", "lookup", "{broken"),
		historyCall("reused", "lookup", `{"ok":true}`),
		historyTool("reused", "sunny"),
	}
	out, n := preserveInvalidToolCallsAsText(in)
	if n != 1 {
		t.Fatalf("recovered=%d", n)
	}
	got := summarizeHistory(out)
	if len(got) != 3 || got[0] != "assistant" || got[1] != "assistant[call:reused]" || got[2] != "tool[reused]" {
		t.Fatalf("messages=%v", got)
	}
	if toolContent, _ := out[2].(map[string]any)["content"].(string); toolContent != "sunny" {
		t.Errorf("合法调用的结果应原样保留: %v", out[2])
	}
	if strings.Contains(out[0].(map[string]any)["content"].(string), "sunny") {
		t.Error("合法调用的结果不应被折进坏调用的文本")
	}
	if !strings.Contains(out[0].(map[string]any)["content"].(string), "Historical tool call with invalid arguments") {
		t.Error("坏调用应留下历史上下文文本")
	}
}

// 孤儿工具结果（历史里没有对应调用）转成 user 文本，保留上下文且不会触发
// 严格上游的 role:tool 配对 400。
func TestOrphanToolOutputBecomesUserText(t *testing.T) {
	raw, _, err := ToChat([]byte(`{
	  "model":"gpt-5.5",
	  "input":[
	    {"type":"message","role":"user","content":"hi"},
	    {"type":"function_call_output","call_id":"missing_call","output":"{\"ok\":true}"},
	    {"type":"function_call","call_id":"c1","name":"lookup","arguments":"{}"},
	    {"type":"function_call_output","call_id":"c1","output":"real result"}
	  ]}`))
	if err != nil {
		t.Fatal(err)
	}
	out := decode(t, raw)
	roles := rolesOf(t, out)
	// user, user(孤儿结果), assistant(tool_calls), tool
	if len(roles) != 4 || roles[1] != "user" || roles[2] != "assistant" || roles[3] != "tool" {
		t.Fatalf("roles=%v", roles)
	}
	orphan, _ := out["messages"].([]any)[1].(map[string]any)
	content, _ := orphan["content"].(string)
	for _, want := range []string{"tool_result without a matching tool_call", "missing_call", "ok"} {
		if !strings.Contains(content, want) {
			t.Errorf("孤儿结果文本缺少 %q: %q", want, content)
		}
	}
	// 正常配对不受影响。
	toolMsg, _ := out["messages"].([]any)[3].(map[string]any)
	if toolMsg["role"] != "tool" || toolMsg["tool_call_id"] != "c1" {
		t.Errorf("正常结果被误改: %v", toolMsg)
	}
}
