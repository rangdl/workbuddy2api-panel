package responses

import (
	"encoding/json"
	"fmt"
	"strings"
)

// FromChat 把 Chat Completions 响应（upstream.Aggregate 结果）转换为 Responses 响应。
func FromChat(resp map[string]any, ctx *ToolContext) (map[string]any, error) {
	choices, _ := resp["choices"].([]any)
	if len(choices) == 0 {
		return nil, fmt.Errorf("no choices in chat response")
	}
	choice, _ := choices[0].(map[string]any)
	message, _ := choice["message"].(map[string]any)
	if message == nil {
		return nil, fmt.Errorf("no message in chat choice")
	}

	responseID := responseIDFromChatID(rawString(resp, "id"))
	model := rawString(resp, "model")
	createdAt := numToInt(resp["created"])
	finishReason := rawString(choice, "finish_reason")

	reasoning := chatReasoningText(message)
	output := []any{}
	if reasoning != "" {
		output = append(output, map[string]any{
			"id":      "rs_" + responseID,
			"type":    "reasoning",
			"summary": []any{map[string]any{"type": "summary_text", "text": reasoning}},
		})
	}
	if item := chatMessageToResponseItem(message, responseID); item != nil {
		output = append(output, item)
	}
	output = append(output, chatToolCallsToResponseItems(message, reasoning, ctx)...)

	out := map[string]any{
		"id":         responseID,
		"object":     "response",
		"created_at": createdAt,
		"status":     responseStatusFromFinishReason(finishReason),
		"model":      model,
		"output":     output,
		"usage":      chatUsageToResponsesUsage(resp["usage"]),
	}
	if finishReason == "length" {
		out["incomplete_details"] = map[string]any{"reason": "max_output_tokens"}
	}
	return out, nil
}

// responseIDFromChatID 把 Chat id 规范化为 resp_ 前缀。
func responseIDFromChatID(id string) string {
	if id == "" {
		return "resp_wb2api"
	}
	if len(id) >= 5 && id[:5] == "resp_" {
		return id
	}
	return "resp_" + id
}

// responseStatusFromFinishReason 把 finish_reason 映射为 Responses status。
func responseStatusFromFinishReason(finishReason string) string {
	if finishReason == "length" {
		return "incomplete"
	}
	return "completed"
}

// chatReasoningText 提取 Chat 响应的 reasoning 文本：
// 优先 reasoning_content/reasoning/reasoning_details，其次 content 前置 think 块。
func chatReasoningText(message map[string]any) string {
	if text := extractReasoningFieldText(message); text != "" {
		return text
	}
	if content := rawString(message, "content"); content != "" {
		if reasoning, _, ok := splitLeadingThinkBlock(content); ok && reasoning != "" {
			return reasoning
		}
	}
	return ""
}

// chatMessageToResponseItem 把 Chat message 转成 Responses message item。
func chatMessageToResponseItem(message map[string]any, responseID string) map[string]any {
	content := []any{}

	if text := rawString(message, "content"); text != "" {
		// 去掉前置 think 块，正文只留 answer。
		if _, answer, ok := splitLeadingThinkBlock(text); ok {
			text = answer
		}
		if text != "" {
			content = append(content, map[string]any{"type": "output_text", "text": text, "annotations": []any{}})
		}
	} else if parts, ok := message["content"].([]any); ok {
		for _, raw := range parts {
			part, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			switch stringField(part, "type") {
			case "text", "output_text":
				if t := rawString(part, "text"); t != "" {
					content = append(content, map[string]any{"type": "output_text", "text": t, "annotations": []any{}})
				}
			case "refusal":
				if t := rawString(part, "refusal"); t != "" {
					content = append(content, map[string]any{"type": "refusal", "refusal": t})
				}
			}
		}
	}
	if refusal := rawString(message, "refusal"); refusal != "" {
		content = append(content, map[string]any{"type": "refusal", "refusal": refusal})
	}

	if len(content) == 0 {
		return nil
	}
	return map[string]any{
		"id":      responseID + "_msg",
		"type":    "message",
		"status":  "completed",
		"role":    "assistant",
		"content": content,
	}
}

// chatToolCallsToResponseItems 把 Chat tool_calls 还原为 Responses output items。
// 依据 ToolContext 把 Chat 侧扁平/代理名还原为 function_call / custom_tool_call /
// tool_search_call；缺失 name 的调用丢弃（防御畸形上游）。
func chatToolCallsToResponseItems(message map[string]any, reasoning string, ctx *ToolContext) []any {
	toolCalls, _ := message["tool_calls"].([]any)
	if len(toolCalls) == 0 {
		if legacy, ok := message["function_call"].(map[string]any); ok {
			if item := chatLegacyFunctionCallToResponseItem(legacy, reasoning, ctx); item != nil {
				return []any{item}
			}
		}
		return nil
	}

	items := make([]any, 0, len(toolCalls))
	for i, raw := range toolCalls {
		tc, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		fn, _ := tc["function"].(map[string]any)
		name := stringField(fn, "name")
		if name == "" {
			continue
		}
		callID := stringField(tc, "id")
		if callID == "" {
			callID = fmt.Sprintf("call_%d", i)
		}
		arguments := canonicalizeToolArguments(fn["arguments"])
		items = append(items, chatToolCallToResponseItem(callID, name, arguments, reasoning, ctx))
	}
	return items
}

// chatToolCallToResponseItem 还原单个 tool_call。
func chatToolCallToResponseItem(callID, chatName, arguments, reasoning string, ctx *ToolContext) map[string]any {
	spec, known := ctx.Lookup(chatName)
	switch {
	case known && spec.Kind == toolKindCustom:
		item := map[string]any{
			"id":      "ctc_" + callID,
			"type":    "custom_tool_call",
			"status":  "completed",
			"call_id": callID,
			"name":    spec.Name,
			"input":   customToolInputFromArguments(arguments),
		}
		attachReasoning(item, reasoning)
		return item
	case known && spec.Kind == toolKindToolSearch:
		item := map[string]any{
			"type":      "tool_search_call",
			"call_id":   callID,
			"status":    "completed",
			"execution": "client",
			"arguments": parseToolArgumentsObject(arguments),
		}
		attachReasoning(item, reasoning)
		return item
	default:
		name, namespace := chatName, ""
		if known {
			name = spec.Name
			namespace = spec.Namespace
		}
		item := map[string]any{
			"id":        "fc_" + callID,
			"type":      "function_call",
			"status":    "completed",
			"call_id":   callID,
			"name":      name,
			"arguments": arguments,
		}
		if namespace != "" {
			item["namespace"] = namespace
		}
		attachReasoning(item, reasoning)
		return item
	}
}

// chatLegacyFunctionCallToResponseItem 兼容旧式 message.function_call。
func chatLegacyFunctionCallToResponseItem(fc map[string]any, reasoning string, ctx *ToolContext) map[string]any {
	name := stringField(fc, "name")
	if name == "" {
		return nil
	}
	callID := firstNonEmpty(stringField(fc, "id"), "call_0")
	return chatToolCallToResponseItem(callID, name, canonicalizeToolArguments(fc["arguments"]), reasoning, ctx)
}

// customToolInputFromArguments 从 Chat arguments 还原 custom 工具的原始 input。
func customToolInputFromArguments(arguments string) string {
	trimmed := strings.TrimSpace(arguments)
	if trimmed == "" {
		return ""
	}
	var parsed any
	if err := json.Unmarshal([]byte(arguments), &parsed); err == nil {
		if obj, ok := parsed.(map[string]any); ok {
			if input, ok := obj[customToolInputField].(string); ok {
				return input
			}
		}
	}
	return arguments
}

// parseToolArgumentsObject 把 arguments 解析为 object；非 object 时包成 {query: ...}。
func parseToolArgumentsObject(arguments string) map[string]any {
	if strings.TrimSpace(arguments) == "" {
		return map[string]any{}
	}
	var parsed any
	if err := json.Unmarshal([]byte(arguments), &parsed); err == nil {
		if obj, ok := parsed.(map[string]any); ok {
			return obj
		}
	}
	return map[string]any{"query": arguments}
}

// attachReasoning 把 reasoning 附加到 item 的 reasoning_content 字段。
func attachReasoning(item map[string]any, reasoning string) {
	if strings.TrimSpace(reasoning) == "" {
		return
	}
	item["reasoning_content"] = reasoning
}
