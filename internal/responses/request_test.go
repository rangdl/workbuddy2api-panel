package responses

import (
	"bytes"
	"encoding/json"
	"sort"
	"strings"
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

// TestInputDeclaredToolsLifted 验证 input 里的 additional_tools 声明也被提升。
func TestInputDeclaredToolsLifted(t *testing.T) {
	body := []byte(`{"model":"m","input":[
		{"type":"additional_tools","tools":[{"type":"function","name":"extra_tool","parameters":{"type":"object"}}]},
		{"type":"message","role":"user","content":"hi"}
	]}`)
	raw, ctx, err := ToChat(body)
	if err != nil {
		t.Fatal(err)
	}
	out := decode(t, raw)
	tools, _ := out["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("expected 1 lifted tool, got %d (%v)", len(tools), tools)
	}
	if _, ok := ctx.Lookup("extra_tool"); !ok {
		t.Errorf("extra_tool not registered in context")
	}
}

// TestReasoningDedupAcrossPendingCalls 验证同一段 reasoning 在合并 tool_calls 时只出现一次。
func TestReasoningDedupAcrossPendingCalls(t *testing.T) {
	body := []byte(`{"model":"m","input":[
		{"type":"function_call","call_id":"c1","name":"a","arguments":"{}","reasoning_content":"same thought"},
		{"type":"function_call","call_id":"c2","name":"b","arguments":"{}","reasoning_content":"same thought"}
	]}`)
	raw, _, err := ToChat(body)
	if err != nil {
		t.Fatal(err)
	}
	out := decode(t, raw)
	msgs, _ := out["messages"].([]any)
	if len(msgs) != 1 {
		t.Fatalf("two adjacent calls should merge into one assistant, got %d", len(msgs))
	}
	msg, _ := msgs[0].(map[string]any)
	if msg["reasoning_content"] != "same thought" {
		t.Errorf("reasoning should be deduped, got %v", msg["reasoning_content"])
	}
}

// TestPendingToolCallsMergeIntoAssistant 验证 pending tool_calls 并入相邻的空 assistant。
func TestPendingToolCallsMergeIntoAssistant(t *testing.T) {
	body := []byte(`{"model":"m","input":[
		{"type":"message","role":"assistant","content":"let me check"},
		{"type":"function_call","call_id":"c1","name":"shell","arguments":"{}"}
	]}`)
	raw, _, err := ToChat(body)
	if err != nil {
		t.Fatal(err)
	}
	out := decode(t, raw)
	msgs, _ := out["messages"].([]any)
	if len(msgs) != 1 {
		t.Fatalf("should merge into the existing assistant, got %d (%v)", len(msgs), msgs)
	}
	msg, _ := msgs[0].(map[string]any)
	if msg["role"] != "assistant" {
		t.Errorf("role=%v", msg["role"])
	}
	if tcs, _ := msg["tool_calls"].([]any); len(tcs) != 1 {
		t.Errorf("tool_calls not merged: %v", msg["tool_calls"])
	}
}

func TestToolRosterHintAppendedAtTail(t *testing.T) {
	body := []byte(`{
		"model":"m",
		"instructions":"You are a coding agent.",
		"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}],
		"tools":[
			{"type":"function","name":"exec_command","description":"Runs a command in a PTY. Returns output.","parameters":{"type":"object"}},
			{"type":"function","name":"write_stdin","description":"Write to a running session.","parameters":{"type":"object"}}
		]
	}`)
	raw, _, err := ToChat(body)
	if err != nil {
		t.Fatal(err)
	}
	out := decode(t, raw)
	msgs, _ := out["messages"].([]any)
	if len(msgs) < 3 {
		t.Fatalf("messages len=%d", len(msgs))
	}
	// 名单提示位于头部 system 区：instructions 之后、首条对话消息之前
	//（对齐 TWA wb_toolexec 注入范式：约束行为 + prompt cache 前缀稳定）。
	first, _ := msgs[0].(map[string]any)
	if first["role"] != "system" || !strings.Contains(rawString(first, "content"), "You are a coding agent.") {
		t.Fatalf("first message should be instructions system, got %v", first)
	}
	hint, _ := msgs[1].(map[string]any)
	if hint["role"] != "system" {
		t.Fatalf("second message role=%v, want system roster hint", hint["role"])
	}
	content := rawString(hint, "content")
	for _, want := range []string{"exec_command", "write_stdin", "shell", "apply_patch"} {
		if !strings.Contains(content, want) {
			t.Errorf("roster hint missing %q: %q", want, content)
		}
	}
	if u, _ := msgs[2].(map[string]any); u["role"] != "user" {
		t.Errorf("third message should be the first conversation message, got %v", u)
	}
}

func TestNoToolsNoRosterHint(t *testing.T) {
	body := []byte(`{"model":"m","input":"hi"}`)
	raw, _, err := ToChat(body)
	if err != nil {
		t.Fatal(err)
	}
	out := decode(t, raw)
	msgs, _ := out["messages"].([]any)
	for _, m := range msgs {
		msg, _ := m.(map[string]any)
		if c, _ := msg["content"].(string); strings.Contains(c, "authoritative list") {
			t.Errorf("unexpected roster hint without tools: %v", msgs)
		}
	}
}

func TestAliasToolInjectedWhenTargetDeclared(t *testing.T) {
	body := []byte(`{
		"model":"m",
		"input":"hi",
		"tools":[{"type":"function","name":"exec_command","description":"run",
			"parameters":{"type":"object","properties":{"cmd":{"type":"string"}}}}]
	}`)
	raw, ctx, err := ToChat(body)
	if err != nil {
		t.Fatal(err)
	}
	out := decode(t, raw)
	tools, _ := out["tools"].([]any)
	if len(tools) != 2 {
		t.Fatalf("tools len=%d, want exec_command + shell alias", len(tools))
	}
	// 别名工具以旧版 schema 声明（command 数组）
	var aliasFn map[string]any
	for _, raw := range tools {
		tl, _ := raw.(map[string]any)
		fn, _ := tl["function"].(map[string]any)
		if fn["name"] == "shell" {
			aliasFn = fn
		}
	}
	if aliasFn == nil {
		t.Fatal("shell alias tool not injected")
	}
	params, _ := aliasFn["parameters"].(map[string]any)
	props, _ := params["properties"].(map[string]any)
	if _, ok := props["command"]; !ok {
		t.Errorf("alias schema missing legacy command param: %v", props)
	}
	// ToolContext 记录别名关系
	spec, ok := ctx.Lookup("shell")
	if !ok || spec.Kind != toolKindAlias || spec.AliasFor != "exec_command" {
		t.Errorf("Lookup(shell) = %+v ok=%v", spec, ok)
	}
}

func TestNoAliasWhenTargetNotDeclared(t *testing.T) {
	body := []byte(`{
		"model":"m",
		"input":"hi",
		"tools":[{"type":"function","name":"write_stdin","description":"w","parameters":{"type":"object"}}]
	}`)
	raw, _, err := ToChat(body)
	if err != nil {
		t.Fatal(err)
	}
	out := decode(t, raw)
	tools, _ := out["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("tools len=%d, want only declared tool", len(tools))
	}
}

func TestAliasRoundTripShellToExecCommand(t *testing.T) {
	// 请求侧：codex 声明 exec_command
	body := []byte(`{
		"model":"m",
		"input":"hi",
		"tools":[{"type":"function","name":"exec_command","description":"run",
			"parameters":{"type":"object","properties":{"cmd":{"type":"string"}}}}]
	}`)
	_, ctx, err := ToChat(body)
	if err != nil {
		t.Fatal(err)
	}
	// 回程：模型输出了旧名 shell + 旧版 command 数组参数
	chatResp := map[string]any{
		"id": "chat-1", "model": "m", "created": float64(1),
		"choices": []any{map[string]any{
			"finish_reason": "tool_calls",
			"message": map[string]any{
				"role": "assistant",
				"tool_calls": []any{map[string]any{
					"id": "call_1", "type": "function",
					"function": map[string]any{
						"name":      "shell",
						"arguments": `{"command":["bash","-lc","echo hi"],"workdir":"/tmp"}`,
					},
				}},
			},
		}},
	}
	resp, err := FromChat(chatResp, ctx)
	if err != nil {
		t.Fatal(err)
	}
	output, _ := resp["output"].([]any)
	if len(output) != 1 {
		t.Fatalf("output=%v", output)
	}
	item, _ := output[0].(map[string]any)
	if item["type"] != "function_call" || item["name"] != "exec_command" {
		t.Fatalf("item=%v, want function_call exec_command", item)
	}
	args, _ := item["arguments"].(string)
	if !strings.Contains(args, `"cmd"`) || strings.Contains(args, `"command"`) {
		t.Errorf("arguments not remapped: %s", args)
	}
	if !strings.Contains(args, `echo hi`) || !strings.Contains(args, `/tmp`) {
		t.Errorf("arguments lost data: %s", args)
	}
}

// namespace 下的 custom 子工具（codex 0.154+ 的 functions.exec JS 编排工具）必须
// 提升——cc-switch PR #7454 将其列为已知边界（模型视野中工具消失 → 盲调 →
// unsupported call），本实现修复该边界。
func TestNamespaceCustomChildLifted(t *testing.T) {
	body := []byte(`{
		"model":"m",
		"input":[
			{"type":"additional_tools","role":"developer","tools":[
				{"type":"namespace","name":"functions","tools":[
					{"type":"custom","name":"exec","description":"Run JavaScript code to orchestrate tool calls"},
					{"type":"function","name":"wait","description":"wait","parameters":{"type":"object"}}
				]},
				{"type":"namespace","name":"clock","tools":[
					{"type":"function","name":"sleep","description":"sleep","parameters":{"type":"object"}}
				]}
			]},
			{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}
		]
	}`)
	raw, ctx, err := ToChat(body)
	if err != nil {
		t.Fatal(err)
	}
	out := decode(t, raw)
	tools, _ := out["tools"].([]any)
	names := map[string]map[string]any{}
	for _, raw := range tools {
		tl, _ := raw.(map[string]any)
		fn, _ := tl["function"].(map[string]any)
		names[fn["name"].(string)] = fn
	}
	// custom 子工具 functions.exec → 扁平名 functions__exec（{input:string} 包装）
	execFn, ok := names["functions__exec"]
	if !ok {
		t.Fatalf("functions__exec not lifted, got tools %v", keysOf(names))
	}
	if !strings.Contains(execFn["description"].(string), "Run JavaScript code") {
		t.Errorf("exec description should embed original definition: %v", execFn["description"])
	}
	// function 子工具照常提升
	if _, ok := names["functions__wait"]; !ok {
		t.Errorf("functions__wait missing, got %v", keysOf(names))
	}
	if _, ok := names["clock__sleep"]; !ok {
		t.Errorf("clock__sleep missing, got %v", keysOf(names))
	}
	// ToolContext：functions__exec 按 custom 通道登记，Name 为裸名
	spec, ok := ctx.Lookup("functions__exec")
	if !ok || spec.Kind != toolKindCustom || spec.Name != "exec" || spec.Namespace != "functions" {
		t.Errorf("Lookup(functions__exec) = %+v ok=%v", spec, ok)
	}
}

// 回程：模型调用 functions__exec → custom_tool_call item（name=exec）。
func TestNamespaceCustomChildRoundTrip(t *testing.T) {
	body := []byte(`{
		"model":"m",
		"input":[{"type":"additional_tools","role":"developer","tools":[
			{"type":"namespace","name":"functions","tools":[
				{"type":"custom","name":"exec","description":"run js"}
			]}
		]}]
	}`)
	_, ctx, err := ToChat(body)
	if err != nil {
		t.Fatal(err)
	}
	chatResp := map[string]any{
		"id": "chat-1", "model": "m", "created": float64(1),
		"choices": []any{map[string]any{
			"finish_reason": "tool_calls",
			"message": map[string]any{
				"role": "assistant",
				"tool_calls": []any{map[string]any{
					"id": "call_e1", "type": "function",
					"function": map[string]any{
						"name":      "functions__exec",
						"arguments": `{"input":"await functions.wait()"}`,
					},
				}},
			},
		}},
	}
	resp, err := FromChat(chatResp, ctx)
	if err != nil {
		t.Fatal(err)
	}
	output, _ := resp["output"].([]any)
	if len(output) != 1 {
		t.Fatalf("output=%v", output)
	}
	item, _ := output[0].(map[string]any)
	if item["type"] != "custom_tool_call" {
		t.Fatalf("type=%v, want custom_tool_call", item["type"])
	}
	if item["name"] != "exec" {
		t.Errorf("name=%v, want bare exec", item["name"])
	}
	if item["input"] != "await functions.wait()" {
		t.Errorf("input=%v, want unwrapped raw input", item["input"])
	}
}

func keysOf(m map[string]map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// 裸名纠偏（实测 deepseek 上游故障）：模型不按声明输出 functions__exec 而是裸名
// exec 时，回程必须还原为 custom_tool_call（而非 function_call），否则 codex 端
// 校验失败、工具执行被中止。
func TestBareNameCustomRoundTrip(t *testing.T) {
	body := []byte(`{
		"model":"m",
		"input":[{"type":"additional_tools","role":"developer","tools":[
			{"type":"namespace","name":"functions","tools":[
				{"type":"custom","name":"exec","description":"run js"},
				{"type":"function","name":"wait","parameters":{"type":"object"}}
			]},
			{"type":"namespace","name":"clock","tools":[
				{"type":"function","name":"sleep","parameters":{"type":"object"}}
			]}
		]}]
	}`)
	_, ctx, err := ToChat(body)
	if err != nil {
		t.Fatal(err)
	}
	// 模型输出了裸名 exec
	chatResp := map[string]any{
		"id": "chat-1", "model": "m", "created": float64(1),
		"choices": []any{map[string]any{
			"finish_reason": "tool_calls",
			"message": map[string]any{
				"role": "assistant",
				"tool_calls": []any{map[string]any{
					"id": "call_b1", "type": "function",
					"function": map[string]any{"name": "exec", "arguments": `{"input":"text('hi')"}`},
				}},
			},
		}},
	}
	resp, err := FromChat(chatResp, ctx)
	if err != nil {
		t.Fatal(err)
	}
	output, _ := resp["output"].([]any)
	if len(output) != 1 {
		t.Fatalf("output=%v", output)
	}
	item, _ := output[0].(map[string]any)
	if item["type"] != "custom_tool_call" {
		t.Fatalf("type=%v, want custom_tool_call (bare name must be re-associated)", item["type"])
	}
	if item["name"] != "exec" {
		t.Errorf("name=%v", item["name"])
	}
	if item["input"] != "text('hi')" {
		t.Errorf("input=%v", item["input"])
	}
	// item_id 前缀同步为 ctc_
	if id := toolCallItemID("call_b1", "exec", ctx); id != "ctc_call_b1" {
		t.Errorf("itemID=%v, want ctc_call_b1", id)
	}
}

// 歧义不猜：两个 namespace 有同名子工具时裸名不纠偏（保持 function_call default）。
func TestBareNameAmbiguousNotGuessed(t *testing.T) {
	body := []byte(`{
		"model":"m",
		"input":[{"type":"additional_tools","role":"developer","tools":[
			{"type":"namespace","name":"functions","tools":[
				{"type":"custom","name":"run","description":"a"}
			]},
			{"type":"namespace","name":"tools","tools":[
				{"type":"custom","name":"run","description":"b"}
			]}
		]}]
	}`)
	_, ctx, err := ToChat(body)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := ctx.LookupLoose("run"); ok {
		t.Error("ambiguous bare name should not match")
	}
	chatResp := map[string]any{
		"id": "chat-1", "model": "m", "created": float64(1),
		"choices": []any{map[string]any{
			"finish_reason": "tool_calls",
			"message": map[string]any{
				"role": "assistant",
				"tool_calls": []any{map[string]any{
					"id": "call_a1", "type": "function",
					"function": map[string]any{"name": "run", "arguments": "{}"},
				}},
			},
		}},
	}
	resp, err := FromChat(chatResp, ctx)
	if err != nil {
		t.Fatal(err)
	}
	output, _ := resp["output"].([]any)
	item, _ := output[0].(map[string]any)
	if item["type"] != "function_call" {
		t.Errorf("ambiguous case type=%v, want function_call (unchanged default)", item["type"])
	}
}

// 组合回归（对照真实会话 #103 的调用形态）：custom exec（裸名）与 namespace
// function 子工具 clock__sleep（裸名 sleep）在同一轮并行发起，且 arguments 分片
// 到达。三个工具调用必须各自还原为正确的 item 类型，互不串扰。
func TestMixedCustomAndNamespaceFunctionParallel(t *testing.T) {
	body := []byte(`{
		"model":"m",
		"input":[{"type":"additional_tools","role":"developer","tools":[
			{"type":"namespace","name":"functions","tools":[
				{"type":"custom","name":"exec","description":"run js"},
				{"type":"function","name":"wait","parameters":{"type":"object"}}
			]},
			{"type":"namespace","name":"clock","tools":[
				{"type":"function","name":"sleep","parameters":{"type":"object"}}
			]}
		]}]
	}`)
	_, ctx, err := ToChat(body)
	if err != nil {
		t.Fatal(err)
	}
	// 流式回程：两个裸名调用（exec custom + sleep function），arguments 分 2 片
	st := NewStreamState(ctx)
	var out bytes.Buffer
	tc1 := map[string]any{"index": 0, "id": "call_c1", "type": "function",
		"function": map[string]any{"name": "exec", "arguments": ""}}
	tc2 := map[string]any{"index": 1, "id": "call_c2", "type": "function",
		"function": map[string]any{"name": "sleep", "arguments": `{"ms":`}}
	out.Write(st.HandleChunk(map[string]any{"id": "chat-1", "model": "m", "created": float64(1),
		"choices": []any{map[string]any{"delta": map[string]any{"tool_calls": []any{tc1, tc2}}}}}))
	tc3 := map[string]any{"index": 1, "function": map[string]any{"arguments": `10}`}}
	out.Write(st.HandleChunk(map[string]any{"id": "chat-1", "model": "m", "created": float64(1),
		"choices": []any{map[string]any{"delta": map[string]any{"tool_calls": []any{tc3}}}}}))
	out.Write(st.Finalize())

	events := parseSSEEvents(t, out.Bytes())
	// exec（裸名，custom）→ custom_tool_call（本场景未发 arguments，input 为空）
	execDone := findDoneItem(t, events, "custom_tool_call")
	if execDone["name"] != "exec" {
		t.Errorf("exec item name=%v", execDone["name"])
	}
	// sleep（裸名，namespace function）→ function_call，且还原 namespace=clock
	sleepDone := findDoneItem(t, events, "function_call")
	if sleepDone["name"] != "sleep" {
		t.Errorf("sleep item name=%v, want bare sleep (namespace restored separately)", sleepDone["name"])
	}
	if sleepDone["namespace"] != "clock" {
		t.Errorf("sleep item namespace=%v, want clock", sleepDone["namespace"])
	}
	if sleepDone["arguments"] != `{"ms":10}` {
		t.Errorf("sleep arguments=%v, want跨片拼接", sleepDone["arguments"])
	}
	// item_id 前缀：exec 用 ctc_，sleep 用 fc_
	if !hasEventWith(t, events, "response.output_item.added", `"id":"ctc_call_c1"`) {
		t.Error("exec item_id prefix should be ctc_")
	}
	if !hasEventWith(t, events, "response.output_item.added", `"id":"fc_call_c2"`) {
		t.Error("sleep item_id prefix should be fc_")
	}
}

// parseSSEEvents 从 SSE 字节流解析 (event, data) 对。
func parseSSEEvents(t *testing.T, raw []byte) []map[string]any {
	t.Helper()
	var events []map[string]any
	for _, block := range strings.Split(string(raw), "\n\n") {
		for _, line := range strings.Split(block, "\n") {
			if strings.HasPrefix(line, "data: ") {
				var m map[string]any
				if json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &m) == nil {
					events = append(events, m)
				}
			}
		}
	}
	return events
}

// findDoneItem 取指定类型的 response.output_item.done 事件 item（取最后一个）。
func findDoneItem(t *testing.T, events []map[string]any, itemType string) map[string]any {
	t.Helper()
	var hit map[string]any
	for _, e := range events {
		if e["type"] != "response.output_item.done" {
			continue
		}
		if item, ok := e["item"].(map[string]any); ok && item["type"] == itemType {
			hit = item
		}
	}
	if hit == nil {
		t.Fatalf("no done item of type %s", itemType)
	}
	return hit
}

// hasEventWith 报告是否存在 type 相符且 data 含子串的事件。
func hasEventWith(t *testing.T, events []map[string]any, eventType, substr string) bool {
	t.Helper()
	for _, e := range events {
		if e["type"] != eventType {
			continue
		}
		raw, _ := json.Marshal(e)
		if strings.Contains(string(raw), substr) {
			return true
		}
	}
	return false
}
