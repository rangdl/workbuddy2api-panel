package responses

import (
	"encoding/json"
	"testing"
)

func rolesOf(t *testing.T, out map[string]any) []string {
	t.Helper()
	msgs, _ := out["messages"].([]any)
	roles := make([]string, 0, len(msgs))
	for _, raw := range msgs {
		msg, _ := raw.(map[string]any)
		role, _ := msg["role"].(string)
		roles = append(roles, role)
	}
	return roles
}

func firstByRole(t *testing.T, out map[string]any, role string) map[string]any {
	t.Helper()
	msgs, _ := out["messages"].([]any)
	for _, raw := range msgs {
		msg, _ := raw.(map[string]any)
		if msg["role"] == role {
			return msg
		}
	}
	t.Fatalf("no %s message: %v", role, out["messages"])
	return nil
}

// DeepSeek：function_call 与输出之间回放的 assistant 正文/思考合并回发起调用的
// assistant，wire 上 tool 结果紧跟其后。
func TestDeepSeekCoalesceReplayedAssistant(t *testing.T) {
	raw, _, err := ToChat([]byte(`{
	  "model":"cn:deepseek-v4.1-flash",
	  "input":[
	    {"type":"function_call","call_id":"c1","name":"view_image","arguments":"{}"},
	    {"type":"message","role":"assistant","content":"Replay text.","reasoning_content":"replay thought"},
	    {"type":"function_call_output","call_id":"c1","output":"ok"}
	  ]}`))
	if err != nil {
		t.Fatal(err)
	}
	out := decode(t, raw)
	if got := rolesOf(t, out); len(got) != 2 || got[0] != "assistant" || got[1] != "tool" {
		t.Fatalf("roles=%v", got)
	}
	a := firstByRole(t, out, "assistant")
	if a["content"] != "Replay text." {
		t.Errorf("回放正文未合并: %v", a["content"])
	}
	if a["reasoning_content"] != "replay thought" {
		t.Errorf("回放思考未合并: %v", a["reasoning_content"])
	}
	if tcs, _ := a["tool_calls"].([]any); len(tcs) != 1 {
		t.Errorf("tool_calls=%v", a["tool_calls"])
	}
}

// 非 DeepSeek 模型保持原序（不因重写历史破坏上游 prompt cache）。
func TestNonDeepSeekKeepsReplayOrder(t *testing.T) {
	raw, _, err := ToChat([]byte(`{
	  "model":"cn:glm-5.3-flash",
	  "input":[
	    {"type":"function_call","call_id":"c1","name":"view_image","arguments":"{}"},
	    {"type":"message","role":"assistant","content":"Replay text."},
	    {"type":"function_call_output","call_id":"c1","output":"ok"}
	  ]}`))
	if err != nil {
		t.Fatal(err)
	}
	got := rolesOf(t, decode(t, raw))
	if len(got) != 3 || got[0] != "assistant" || got[1] != "assistant" || got[2] != "tool" {
		t.Fatalf("非 DeepSeek 不应合并: %v", got)
	}
}

// 并行调用被拆到多条 assistant、结果乱序返回：合并成一条 assistant，tool 连续。
func TestDeepSeekCoalesceSplitParallelCalls(t *testing.T) {
	raw, _, err := ToChat([]byte(`{
	  "model":"global:deepseek-v4.1-flash",
	  "input":[
	    {"type":"function_call","call_id":"c1","name":"a","arguments":"{}"},
	    {"type":"message","role":"assistant","content":"mid text"},
	    {"type":"function_call","call_id":"c2","name":"b","arguments":"{}"},
	    {"type":"function_call_output","call_id":"c2","output":"r2"},
	    {"type":"function_call_output","call_id":"c1","output":"r1"}
	  ]}`))
	if err != nil {
		t.Fatal(err)
	}
	out := decode(t, raw)
	got := rolesOf(t, out)
	if len(got) != 3 || got[0] != "assistant" || got[1] != "tool" || got[2] != "tool" {
		t.Fatalf("roles=%v", got)
	}
	a := firstByRole(t, out, "assistant")
	if a["content"] != "mid text" {
		t.Errorf("正文未合并: %v", a["content"])
	}
	if tcs, _ := a["tool_calls"].([]any); len(tcs) != 2 {
		t.Errorf("tool_calls=%v", a["tool_calls"])
	}
}

// 规范历史（调用后立即有结果）零改动，缓存敏感的序列化保持稳定。
func TestDeepSeekKeepsWellFormedHistory(t *testing.T) {
	raw, _, err := ToChat([]byte(`{
	  "model":"deepseek-v4.1-flash",
	  "input":[
	    {"type":"function_call","call_id":"c1","name":"a","arguments":"{}"},
	    {"type":"function_call_output","call_id":"c1","output":"r1"},
	    {"type":"message","role":"assistant","content":"done"},
	    {"type":"message","role":"user","content":"next"}
	  ]}`))
	if err != nil {
		t.Fatal(err)
	}
	got := rolesOf(t, decode(t, raw))
	if len(got) != 4 || got[0] != "assistant" || got[1] != "tool" || got[2] != "assistant" || got[3] != "user" {
		t.Fatalf("roles=%v", got)
	}
}

// 模型映射发生在 ToChat 之后：映射后必须按最终上游模型补跑 DeepSeek 合并。
func TestApplyUpstreamModelFixesAfterModelMap(t *testing.T) {
	body := []byte(`{"model":"cn:deepseek-v4.1-flash","messages":[
	  {"role":"assistant","content":null,"tool_calls":[{"id":"c1","type":"function","function":{"name":"a","arguments":"{}"}}]},
	  {"role":"assistant","content":"Replay text."},
	  {"role":"tool","tool_call_id":"c1","content":"ok"}
	]}`)
	fixed := ApplyUpstreamModelFixes(body)
	var out map[string]any
	if err := json.Unmarshal(fixed, &out); err != nil {
		t.Fatal(err)
	}
	msgs, _ := out["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("映射后未合并: %v", msgs)
	}
	if a := msgs[0].(map[string]any); a["content"] != "Replay text." {
		t.Errorf("回放正文未并入调用消息: %v", a["content"])
	}

	// 非 DeepSeek 与规范历史：原样返回（字节不变，保护 prompt cache）。
	glm := []byte(`{"model":"cn:glm-5.3-flash","messages":[
	  {"role":"assistant","content":null,"tool_calls":[{"id":"c1","type":"function","function":{"name":"a","arguments":"{}"}}]},
	  {"role":"assistant","content":"Replay text."},
	  {"role":"tool","tool_call_id":"c1","content":"ok"}
	]}`)
	if got := ApplyUpstreamModelFixes(glm); string(got) != string(glm) {
		t.Error("非 DeepSeek 不应改写请求体")
	}
	valid := []byte(`{"model":"deepseek-v4.1-flash","messages":[
	  {"role":"assistant","content":null,"tool_calls":[{"id":"c1","type":"function","function":{"name":"a","arguments":"{}"}}]},
	  {"role":"tool","tool_call_id":"c1","content":"ok"}
	]}`)
	if got := ApplyUpstreamModelFixes(valid); string(got) != string(valid) {
		t.Error("规范历史应原样返回")
	}
}
