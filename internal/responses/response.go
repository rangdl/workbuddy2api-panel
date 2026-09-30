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

// chatToolCallToResponseItem 还原单个 tool_call（非流式，status=completed）。
func chatToolCallToResponseItem(callID, chatName, arguments, reasoning string, ctx *ToolContext) map[string]any {
	return toolCallItem(callID, chatName, arguments, reasoning, "completed", ctx)
}

// toolCallItem 按 ToolContext 生成 Responses 工具调用 item，供流式/非流式共用：
// 依据 Chat 工具名还原为 function_call / custom_tool_call / tool_search_call。
// status 为 in_progress 时 arguments/input 传空。
// 反查用 LookupLoose（精确 miss 后按裸名宽松纠偏）：部分模型不按声明输出扁平名
// functions__exec 而是裸名 exec，精确 miss 会把 custom 工具还原成 function_call，
// codex 端校验失败执行被中止（会话级故障，实测 deepseek 上游）。
func toolCallItem(callID, chatName, arguments, reasoning, status string, ctx *ToolContext) map[string]any {
	var spec ToolSpec
	known := false
	if ctx != nil {
		spec, known = ctx.LookupLoose(chatName)
	}
	switch {
	case known && spec.Kind == toolKindCustom:
		item := map[string]any{
			"id":      "ctc_" + callID,
			"type":    "custom_tool_call",
			"status":  status,
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
			"status":    status,
			"execution": "client",
			"arguments": parseToolArgumentsObject(arguments),
		}
		attachReasoning(item, reasoning)
		return item
	default:
		name, namespace := chatName, ""
		if known {
			if spec.Kind == toolKindAlias {
				// 别名工具：还原为 codex 实际声明的新版工具名（如 shell→exec_command），
				// 参数同步转换（旧版 command 数组 → 新版 cmd 字符串）。
				name = spec.AliasFor
				arguments = remapAliasArguments(chatName, arguments)
			} else {
				name = spec.Name
			}
			namespace = spec.Namespace
		}
		item := map[string]any{
			"id":        "fc_" + callID,
			"type":      "function_call",
			"status":    status,
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

// toolCallItemID 计算流式事件里的 item_id（custom 用 ctc_ 前缀，其余 fc_）。
func toolCallItemID(callID, chatName string, ctx *ToolContext) string {
	if ctx != nil && ctx.isCustom(chatName) {
		return "ctc_" + callID
	}
	// 精确 miss 时按裸名宽松纠偏（与 toolCallItem 的 LookupLoose 同口径）：
	// 模型输出裸名 exec 时 item 会还原成 custom_tool_call，item_id 前缀必须
	// 同步为 ctc_，否则 codex 端按 id 前缀分发事件会错配。
	if ctx != nil && !ctx.isKnown(chatName) {
		if spec, ok := ctx.LookupLoose(chatName); ok && spec.Kind == toolKindCustom {
			return "ctc_" + callID
		}
	}
	return "fc_" + callID
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

// remapAliasArguments 把模型按旧版工具 schema 生成的 arguments 转换为新版工具
// 的参数（shell 的 command 数组 → exec_command 的 cmd 字符串）。chatName 未命中
// 别名表时原样返回。返回值重新序列化为 JSON 字符串（保持 Responses arguments
// 的字符串形态）；转换产出的 map 不再二次 canonicalize（键序无语义影响）。
func remapAliasArguments(chatName, arguments string) string {
	for _, alias := range legacyAliases {
		if alias.legacyName != chatName || alias.remapArgs == nil {
			continue
		}
		out := alias.remapArgs(arguments)
		raw, err := json.Marshal(out)
		if err != nil {
			return arguments
		}
		return string(raw)
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
