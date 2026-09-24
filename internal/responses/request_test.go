package responses

import (
	"encoding/json"
	"testing"
)

func decode(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("unmarshal: %v raw=%s", err, raw)
	}
	return out
}

func TestToChatInstructionsAndInput(t *testing.T) {
	body := []byte(`{
		"model": "gpt-5-codex",
		"instructions": "You are a coding agent.",
		"input": [
			{"type":"message","role":"user","content":[{"type":"input_text","text":"list files"}]}
		],
		"max_output_tokens": 1024,
		"stream": true,
		"reasoning": {"effort":"high"}
	}`)
	raw, _, err := ToChat(body)
	if err != nil {
		t.Fatal(err)
	}
	out := decode(t, raw)

	if out["model"] != "gpt-5-codex" {
		t.Errorf("model=%v", out["model"])
	}
	if out["max_tokens"] != float64(1024) {
		t.Errorf("max_tokens=%v", out["max_tokens"])
	}
	if out["stream"] != true {
		t.Errorf("stream=%v", out["stream"])
	}
	if out["reasoning_effort"] != "high" {
		t.Errorf("reasoning_effort=%v", out["reasoning_effort"])
	}
	msgs, _ := out["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("messages len=%d", len(msgs))
	}
	sys, _ := msgs[0].(map[string]any)
	if sys["role"] != "system" || sys["content"] != "You are a coding agent." {
		t.Errorf("system=%v", sys)
	}
	user, _ := msgs[1].(map[string]any)
	if user["role"] != "user" || user["content"] != "list files" {
		t.Errorf("user=%v", user)
	}
}

func TestFunctionCallOutputBecomesToolMessage(t *testing.T) {
	body := []byte(`{
		"model":"m",
		"input":[
			{"type":"function_call","call_id":"call_1","name":"shell","arguments":"{\"cmd\":\"ls\"}"},
			{"type":"function_call_output","call_id":"call_1","output":"file1\nfile2"}
		]
	}`)
	raw, _, err := ToChat(body)
	if err != nil {
		t.Fatal(err)
	}
	out := decode(t, raw)
	msgs, _ := out["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("messages len=%d (%v)", len(msgs), msgs)
	}
	assistant, _ := msgs[0].(map[string]any)
	if assistant["role"] != "assistant" {
		t.Errorf("assistant role=%v", assistant["role"])
	}
	tcs, _ := assistant["tool_calls"].([]any)
	if len(tcs) != 1 {
		t.Fatalf("tool_calls len=%d", len(tcs))
	}
	tc, _ := tcs[0].(map[string]any)
	if tc["id"] != "call_1" {
		t.Errorf("call id=%v", tc["id"])
	}
	fn, _ := tc["function"].(map[string]any)
	if fn["name"] != "shell" || fn["arguments"] != `{"cmd":"ls"}` {
		t.Errorf("function=%v", fn)
	}
	tool, _ := msgs[1].(map[string]any)
	if tool["role"] != "tool" || tool["tool_call_id"] != "call_1" || tool["content"] != "file1\nfile2" {
		t.Errorf("tool=%v", tool)
	}
}

func TestReasoningAttachedToPendingToolCalls(t *testing.T) {
	body := []byte(`{
		"model":"m",
		"input":[
			{"type":"reasoning","summary":[{"type":"summary_text","text":"need to list"}]},
			{"type":"function_call","call_id":"c1","name":"shell","arguments":"{}"}
		]
	}`)
	raw, _, err := ToChat(body)
	if err != nil {
		t.Fatal(err)
	}
	out := decode(t, raw)
	msgs, _ := out["messages"].([]any)
	if len(msgs) != 1 {
		t.Fatalf("messages len=%d", len(msgs))
	}
	assistant, _ := msgs[0].(map[string]any)
	if assistant["reasoning_content"] != "need to list" {
		t.Errorf("reasoning_content=%v", assistant["reasoning_content"])
	}
}

func TestFunctionToolMapping(t *testing.T) {
	body := []byte(`{
		"model":"m",
		"input":"hi",
		"tools":[{"type":"function","name":"shell","description":"run shell",
			"parameters":{"type":"object","properties":{"cmd":{"type":"string"}}}}],
		"tool_choice":{"type":"function","name":"shell"}
	}`)
	raw, ctx, err := ToChat(body)
	if err != nil {
		t.Fatal(err)
	}
	out := decode(t, raw)
	tools, _ := out["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("tools len=%d", len(tools))
	}
	tool, _ := tools[0].(map[string]any)
	fn, _ := tool["function"].(map[string]any)
	if fn["name"] != "shell" || fn["description"] != "run shell" {
		t.Errorf("function=%v", fn)
	}
	params, _ := fn["parameters"].(map[string]any)
	if params["type"] != "object" {
		t.Errorf("params=%v", params)
	}
	tc, _ := out["tool_choice"].(map[string]any)
	if tc["type"] != "function" {
		t.Errorf("tool_choice=%v", tc)
	}
	if _, ok := ctx.Lookup("shell"); !ok {
		t.Errorf("tool context missing shell")
	}
}

func TestCustomToolMappingAndRoundTrip(t *testing.T) {
	body := []byte(`{
		"model":"m",
		"input":"hi",
		"tools":[{"type":"custom","name":"apply_patch","description":"apply a patch"}]
	}`)
	raw, ctx, err := ToChat(body)
	if err != nil {
		t.Fatal(err)
	}
	out := decode(t, raw)
	tools, _ := out["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("tools len=%d", len(tools))
	}
	fn, _ := tools[0].(map[string]any)["function"].(map[string]any)
	if fn["name"] != "apply_patch" {
		t.Errorf("function name=%v", fn["name"])
	}
	params, _ := fn["parameters"].(map[string]any)
	props, _ := params["properties"].(map[string]any)
	if _, ok := props[customToolInputField]; !ok {
		t.Errorf("custom tool params missing input field: %v", params)
	}
	if !ctx.isCustom("apply_patch") {
		t.Errorf("context should mark apply_patch custom")
	}

	// Chat 回程：arguments 里的 {input: ...} 应还原为 custom_tool_call.input。
	item := chatToolCallToResponseItem("call_9", "apply_patch", `{"input":"*** Begin Patch"}`, "", ctx)
	if item["type"] != "custom_tool_call" {
		t.Errorf("type=%v", item["type"])
	}
	if item["input"] != "*** Begin Patch" {
		t.Errorf("input=%v", item["input"])
	}
	if item["id"] != "ctc_call_9" {
		t.Errorf("id=%v", item["id"])
	}
}

func TestNamespaceToolFlattening(t *testing.T) {
	body := []byte(`{
		"model":"m",
		"input":"hi",
		"tools":[{"type":"namespace","name":"fs","tools":[
			{"type":"function","name":"read","parameters":{"type":"object"}}
		]}]
	}`)
	raw, ctx, err := ToChat(body)
	if err != nil {
		t.Fatal(err)
	}
	out := decode(t, raw)
	tools, _ := out["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("tools len=%d", len(tools))
	}
	fn, _ := tools[0].(map[string]any)["function"].(map[string]any)
	if fn["name"] != "fs__read" {
		t.Errorf("flattened name=%v", fn["name"])
	}
	// 回程应还原 namespace + name。
	item := chatToolCallToResponseItem("c1", "fs__read", "{}", "", ctx)
	if item["type"] != "function_call" || item["name"] != "read" || item["namespace"] != "fs" {
		t.Errorf("restored item=%v", item)
	}
}

func TestReasoningEffortDisabled(t *testing.T) {
	body := []byte(`{"model":"m","input":"hi","reasoning":{"effort":"none"}}`)
	raw, _, err := ToChat(body)
	if err != nil {
		t.Fatal(err)
	}
	out := decode(t, raw)
	if _, ok := out["reasoning_effort"]; ok {
		t.Errorf("disabled effort should not emit reasoning_effort: %v", out["reasoning_effort"])
	}
}

// TestWebSearchToolIgnored 验证服务端工具（web_search）被有意忽略：不产生 Chat 工具、
// 不报错（对齐 cc-switch 的 Chat 路径）。
func TestWebSearchToolIgnored(t *testing.T) {
	body := []byte(`{"model":"m","input":"hi","tools":[
		{"type":"function","name":"shell","parameters":{"type":"object"}},
		{"type":"web_search","external_web_access":false}
	]}`)
	raw, _, err := ToChat(body)
	if err != nil {
		t.Fatalf("web_search must not cause error: %v", err)
	}
	out := decode(t, raw)
	tools, _ := out["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("tools=%d, web_search should be dropped", len(tools))
	}
	fn, _ := tools[0].(map[string]any)["function"].(map[string]any)
	if fn["name"] != "shell" {
		t.Errorf("remaining tool=%v", fn)
	}
}

// TestResponsesOnlyFieldsIgnored 验证 include / client_metadata / prompt_cache_key
// 等 Responses 专有字段不进入 Chat 请求体。
func TestResponsesOnlyFieldsIgnored(t *testing.T) {
	body := []byte(`{"model":"m","input":"hi",
		"include":["reasoning.encrypted_content"],
		"prompt_cache_key":"abc",
		"client_metadata":{"turn_id":"t1"}}`)
	raw, _, err := ToChat(body)
	if err != nil {
		t.Fatal(err)
	}
	out := decode(t, raw)
	for _, k := range []string{"include", "prompt_cache_key", "client_metadata"} {
		if _, ok := out[k]; ok {
			t.Errorf("%s should not be forwarded to Chat", k)
		}
	}
}
