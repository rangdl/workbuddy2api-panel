package responses

import "testing"

func chatResp(message map[string]any, finishReason string, usage map[string]any) map[string]any {
	choice := map[string]any{"index": float64(0), "message": message, "finish_reason": finishReason}
	resp := map[string]any{
		"id":      "chatcmpl-1",
		"object":  "chat.completion",
		"created": float64(1753600000),
		"model":   "glm-5.2",
		"choices": []any{choice},
	}
	if usage != nil {
		resp["usage"] = usage
	}
	return resp
}

func TestFromChatMessage(t *testing.T) {
	resp := chatResp(map[string]any{"role": "assistant", "content": "hello"}, "stop", map[string]any{
		"prompt_tokens":     float64(3),
		"completion_tokens": float64(5),
		"total_tokens":      float64(8),
	})
	out, err := FromChat(resp, NewToolContext())
	if err != nil {
		t.Fatal(err)
	}
	if out["object"] != "response" || out["id"] != "resp_chatcmpl-1" {
		t.Errorf("id/object=%v/%v", out["id"], out["object"])
	}
	if out["status"] != "completed" {
		t.Errorf("status=%v", out["status"])
	}
	output, _ := out["output"].([]any)
	if len(output) != 1 {
		t.Fatalf("output len=%d", len(output))
	}
	msg, _ := output[0].(map[string]any)
	if msg["type"] != "message" || msg["role"] != "assistant" {
		t.Errorf("message item=%v", msg)
	}
	content, _ := msg["content"].([]any)
	part, _ := content[0].(map[string]any)
	if part["type"] != "output_text" || part["text"] != "hello" {
		t.Errorf("content part=%v", part)
	}
	usage, _ := out["usage"].(map[string]any)
	if usage["input_tokens"] != int64(3) || usage["output_tokens"] != int64(5) || usage["total_tokens"] != int64(8) {
		t.Errorf("usage=%v", usage)
	}
}

func TestFromChatReasoningAndToolCalls(t *testing.T) {
	ctx := NewToolContext()
	ctx.add("shell", ToolSpec{Kind: toolKindFunction, Name: "shell"}, map[string]any{"type": "function"})

	message := map[string]any{
		"role":              "assistant",
		"content":           "",
		"reasoning_content": "need to inspect",
		"tool_calls": []any{map[string]any{
			"id":   "call_1",
			"type": "function",
			"function": map[string]any{
				"name":      "shell",
				"arguments": `{"cmd":"ls"}`,
			},
		}},
	}
	out, err := FromChat(chatResp(message, "tool_calls", nil), ctx)
	if err != nil {
		t.Fatal(err)
	}
	output, _ := out["output"].([]any)
	if len(output) != 2 {
		t.Fatalf("output len=%d (%v)", len(output), output)
	}
	reasoning, _ := output[0].(map[string]any)
	if reasoning["type"] != "reasoning" {
		t.Errorf("first item type=%v", reasoning["type"])
	}
	summary, _ := reasoning["summary"].([]any)
	sPart, _ := summary[0].(map[string]any)
	if sPart["text"] != "need to inspect" {
		t.Errorf("summary=%v", summary)
	}
	fc, _ := output[1].(map[string]any)
	if fc["type"] != "function_call" || fc["call_id"] != "call_1" || fc["name"] != "shell" {
		t.Errorf("function_call item=%v", fc)
	}
	if fc["arguments"] != `{"cmd":"ls"}` {
		t.Errorf("arguments=%v", fc["arguments"])
	}
}

func TestFromChatLengthIncomplete(t *testing.T) {
	out, err := FromChat(chatResp(map[string]any{"role": "assistant", "content": "partial"}, "length", nil), NewToolContext())
	if err != nil {
		t.Fatal(err)
	}
	if out["status"] != "incomplete" {
		t.Errorf("status=%v", out["status"])
	}
	details, _ := out["incomplete_details"].(map[string]any)
	if details["reason"] != "max_output_tokens" {
		t.Errorf("incomplete_details=%v", out["incomplete_details"])
	}
}

func TestFromChatInlineThinkSplit(t *testing.T) {
	message := map[string]any{
		"role":    "assistant",
		"content": "\x3cthink\x3ebecause reasons</think>the answer",
	}
	out, err := FromChat(chatResp(message, "stop", nil), NewToolContext())
	if err != nil {
		t.Fatal(err)
	}
	output, _ := out["output"].([]any)
	if len(output) != 2 {
		t.Fatalf("output len=%d (%v)", len(output), output)
	}
	reasoning, _ := output[0].(map[string]any)
	if reasoning["type"] != "reasoning" {
		t.Errorf("reasoning item=%v", reasoning)
	}
	msg, _ := output[1].(map[string]any)
	content, _ := msg["content"].([]any)
	part, _ := content[0].(map[string]any)
	if part["text"] != "the answer" {
		t.Errorf("answer=%v", part["text"])
	}
}

func TestUsageMappingDetails(t *testing.T) {
	u := chatUsageToResponsesUsage(map[string]any{
		"prompt_tokens":     float64(10),
		"completion_tokens": float64(20),
		"total_tokens":      float64(30),
		"prompt_tokens_details": map[string]any{
			"cached_tokens": float64(4),
		},
		"completion_tokens_details": map[string]any{
			"reasoning_tokens": float64(7),
		},
	})
	if u["input_tokens"] != int64(10) || u["output_tokens"] != int64(20) || u["total_tokens"] != int64(30) {
		t.Errorf("usage=%v", u)
	}
	details, _ := u["input_tokens_details"].(map[string]any)
	if details["cached_tokens"] != int64(4) {
		t.Errorf("input_tokens_details=%v", details)
	}
	outDetails, _ := u["output_tokens_details"].(map[string]any)
	if outDetails["reasoning_tokens"] != float64(7) {
		t.Errorf("output_tokens_details=%v", outDetails)
	}
}

func TestUsageMappingEmpty(t *testing.T) {
	u := chatUsageToResponsesUsage(nil)
	if u["input_tokens"] != int64(0) || u["total_tokens"] != int64(0) {
		t.Errorf("empty usage=%v", u)
	}
}

func TestErrorToResponses(t *testing.T) {
	resp := ErrorToResponses([]byte(`{"error":{"message":"boom","type":"invalid_request_error","code":"bad"}}`))
	errObj, _ := resp["error"].(map[string]any)
	if errObj["message"] != "boom" || errObj["type"] != "invalid_request_error" || errObj["code"] != "bad" {
		t.Errorf("error=%v", errObj)
	}

	plain := ErrorToResponses([]byte(`{"message":"plain"}`))
	pErr, _ := plain["error"].(map[string]any)
	if pErr["message"] != "plain" {
		t.Errorf("plain error=%v", pErr)
	}
}

func TestSplitLeadingThinkBlock(t *testing.T) {
	reasoning, answer, ok := splitLeadingThinkBlock("\x3cthink\x3er</think>a")
	if !ok || reasoning != "r" || answer != "a" {
		t.Errorf("split=(%q,%q,%v)", reasoning, answer, ok)
	}
	if _, _, ok := splitLeadingThinkBlock("no think here"); ok {
		t.Errorf("should not detect think block")
	}
}
