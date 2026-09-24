package responses

import (
	"encoding/json"
	"os"
	"testing"
)

// TestRealCodexRequestFixture 用真实 codex 捕获的请求体验证 ToChat。
// 通过 CODEX_FIXTURE=/path/to/req.json 触发；未设置时跳过（CI 无 fixture）。
func TestRealCodexRequestFixture(t *testing.T) {
	path := os.Getenv("CODEX_FIXTURE")
	if path == "" {
		t.Skip("set CODEX_FIXTURE to a captured codex responses request")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}

	chatBody, ctx, err := ToChat(raw)
	if err != nil {
		t.Fatalf("ToChat: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal(chatBody, &out); err != nil {
		t.Fatalf("chat body not json: %v", err)
	}

	msgs, _ := out["messages"].([]any)
	if len(msgs) == 0 {
		t.Fatalf("no messages produced")
	}

	// 断言：function_call → assistant.tool_calls；function_call_output → role=tool。
	var sawAssistantToolCalls, sawToolResult bool
	for _, raw := range msgs {
		msg, _ := raw.(map[string]any)
		if _, ok := msg["tool_calls"]; ok {
			sawAssistantToolCalls = true
		}
		if msg["role"] == "tool" {
			sawToolResult = true
		}
	}

	// 断言 tools 转换：function + namespace 展开，web_search 忽略。
	tools, _ := out["tools"].([]any)
	t.Logf("messages=%d tools=%d ctx.chatTools=%d", len(msgs), len(tools), len(ctx.ChatTools()))

	// web_search 等服务端工具必须被忽略（不产生 Chat 工具）。
	for _, raw := range tools {
		tool, _ := raw.(map[string]any)
		fn, _ := tool["function"].(map[string]any)
		if name, _ := fn["name"].(string); name == "web_search" {
			t.Errorf("web_search should be dropped from chat tools")
		}
	}

	// 若 fixture 含工具往返（第二轮），应同时出现 assistant.tool_calls 与 tool。
	if !sawAssistantToolCalls && !sawToolResult {
		t.Logf("fixture has no tool round-trip (first turn)")
	} else if !sawAssistantToolCalls || !sawToolResult {
		t.Errorf("incomplete tool round-trip: assistant.tool_calls=%v tool=%v", sawAssistantToolCalls, sawToolResult)
	}

	// 顶层：model 必须保留。
	if _, ok := out["model"]; !ok {
		t.Errorf("model missing")
	}
}
