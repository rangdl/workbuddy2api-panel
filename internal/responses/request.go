package responses

import (
	"encoding/json"
	"fmt"
	"strings"
)

// extraChatPassthroughFields Responses 与 Chat 同名的字段，直接透传。
var extraChatPassthroughFields = []string{
	"frequency_penalty", "logit_bias", "logprobs", "metadata", "n",
	"parallel_tool_calls", "presence_penalty", "response_format", "seed",
	"service_tier", "stop", "stream_options", "top_logprobs", "user",
}

// ToChat 把 Responses 请求体转换为 Chat Completions 请求体，并返回工具上下文
// （供回程把 Chat function_call 还原为 Responses item）。
//
// 转换后的请求体是标准 Chat 形态，可直接交给既有 chatCompletions 链路
// （realm/粘性/提示词/脱敏/轮转/冷却 全部复用）。
func ToChat(body []byte) ([]byte, *ToolContext, error) {
	var req map[string]any
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, nil, fmt.Errorf("invalid responses request: %w", err)
	}

	toolCtx := buildToolContext(req)
	out := map[string]any{}

	if model, ok := req["model"]; ok {
		out["model"] = model
	}

	messages := make([]any, 0, 4)
	if instructions := instructionText(req["instructions"]); instructions != "" {
		messages = append(messages, map[string]any{"role": "system", "content": instructions})
	}
	if input, ok := req["input"]; ok {
		msgs := appendResponsesInput(input, toolCtx)
		messages = append(messages, msgs...)
	}
	out["messages"] = collapseSystemMessages(messages)

	// 输出上限：max_output_tokens → max_tokens（项目出站管线会再做一次别名翻译）。
	if v, ok := req["max_output_tokens"]; ok {
		out["max_tokens"] = v
	}
	for _, k := range []string{"max_tokens", "max_completion_tokens", "temperature", "top_p", "stream"} {
		if v, ok := req[k]; ok {
			out[k] = v
		}
	}

	applyReasoning(out, req)

	if tools := toolCtx.ChatTools(); len(tools) > 0 {
		out["tools"] = tools
	}
	if tc, ok := req["tool_choice"]; ok {
		out["tool_choice"] = responsesToolChoiceToChat(tc, toolCtx)
	}

	for _, k := range extraChatPassthroughFields {
		if v, ok := req[k]; ok {
			out[k] = v
		}
	}

	// 其余 Responses 专有字段**有意忽略**（Chat 上游无对应语义）：
	//   - include: ["reasoning.encrypted_content"] —— codex 期望加密思维链，
	//     Chat 上游不提供；实测（codex 0.154.0）能接受无 encrypted_content 的
	//     reasoning item，故无需伪造。
	//   - client_metadata / prompt_cache_key / store / previous_response_id ——
	//     网关无状态，忽略（previous_response_id 已在 server 层用于增量补全）。

	raw, err := json.Marshal(out)
	if err != nil {
		return nil, nil, err
	}
	return raw, toolCtx, nil
}

// instructionText 提取 instructions（字符串或 content parts 数组）。
func instructionText(v any) string {
	switch s := v.(type) {
	case string:
		return strings.TrimSpace(s)
	case []any:
		var parts []string
		for _, raw := range s {
			if m, ok := raw.(map[string]any); ok {
				if t := rawString(m, "text"); t != "" {
					parts = append(parts, t)
				}
			}
		}
		return strings.TrimSpace(strings.Join(parts, "\n"))
	}
	return ""
}

// appendResponsesInput 处理 input 的三种形态：字符串 / 单对象 / 对象数组。
func appendResponsesInput(input any, ctx *ToolContext) []any {
	switch v := input.(type) {
	case string:
		return []any{map[string]any{"role": "user", "content": v}}
	case []any:
		return appendResponsesItems(v, ctx)
	case map[string]any:
		return appendResponsesItems([]any{v}, ctx)
	default:
		return nil
	}
}

// appendResponsesItems 把 Responses input items 转成 Chat messages。
//
// 关键点（对齐 cc-switch）：
//   - function_call 先累积，遇到下一条 message / call_output 时才 flush 成一条带
//     tool_calls 的 assistant 消息，保证 Chat 侧 tool 配对合法；
//   - reasoning 暂存 pending，前向附挂到随后的 assistant / tool_calls；
//     回合边界（user）时回溯附挂到上一条 assistant，防止跨回合泄漏。
func appendResponsesItems(items []any, ctx *ToolContext) []any {
	messages := []any{}
	var pendingCalls []any
	var pendingReasoning string
	lastAssistant := -1

	flushCalls := func() {
		if len(pendingCalls) == 0 {
			return
		}
		msg := map[string]any{"role": "assistant", "tool_calls": pendingCalls}
		if pendingReasoning != "" {
			msg["reasoning_content"] = pendingReasoning
		}
		messages = append(messages, msg)
		lastAssistant = len(messages) - 1
		pendingCalls = nil
		pendingReasoning = ""
	}

	for _, raw := range items {
		item, ok := raw.(map[string]any)
		if !ok {
			if s, ok := raw.(string); ok {
				flushCalls()
				messages = append(messages, map[string]any{"role": "user", "content": s})
			}
			continue
		}
		switch stringField(item, "type") {
		case "function_call":
			pendingReasoning = joinReasoning(pendingReasoning, responsesItemReasoning(item))
			pendingCalls = append(pendingCalls, responsesFunctionCallToChatToolCall(item, ctx))
		case "custom_tool_call":
			pendingReasoning = joinReasoning(pendingReasoning, responsesItemReasoning(item))
			pendingCalls = append(pendingCalls, responsesCustomToolCallToChatToolCall(item))
		case "tool_search_call":
			pendingReasoning = joinReasoning(pendingReasoning, responsesItemReasoning(item))
			pendingCalls = append(pendingCalls, responsesToolSearchCallToChatToolCall(item))
		case "function_call_output", "custom_tool_call_output", "tool_search_output":
			flushCalls()
			messages = append(messages, map[string]any{
				"role":         "tool",
				"tool_call_id": responseItemCallID(item),
				"content":      toolOutputString(item["output"]),
			})
		case "reasoning":
			pendingReasoning = joinReasoning(pendingReasoning, responsesReasoningItemText(item))
		case "additional_tools":
			// 工具声明已在 buildToolContext 提升，此处不产生消息。
		default:
			if stringField(item, "type") == "message" || item["role"] != nil || item["content"] != nil {
				flushCalls()
				msg := responsesMessageToChatMessage(item)
				if msg["role"] == "assistant" {
					if pendingReasoning != "" {
						msg["reasoning_content"] = joinReasoning(rawString(msg, "reasoning_content"), pendingReasoning)
						pendingReasoning = ""
					}
					messages = append(messages, msg)
					lastAssistant = len(messages) - 1
				} else {
					attachReasoningToPreviousAssistant(messages, lastAssistant, &pendingReasoning)
					messages = append(messages, msg)
				}
			}
		}
	}
	flushCalls()
	attachReasoningToPreviousAssistant(messages, lastAssistant, &pendingReasoning)
	return messages
}

// responsesMessageToChatMessage 把 Responses message item 转成 Chat message。
func responsesMessageToChatMessage(item map[string]any) map[string]any {
	role := responsesRoleToChatRole(stringField(item, "role"))
	return map[string]any{"role": role, "content": responsesContentToChatContent(item["content"])}
}

// responsesRoleToChatRole 映射 Responses role 到 Chat role。
func responsesRoleToChatRole(role string) string {
	switch role {
	case "system", "developer":
		return "system"
	case "assistant":
		return "assistant"
	case "tool":
		return "tool"
	default:
		return "user"
	}
}

// responsesContentToChatContent 把 Responses content 转成 Chat content：
// 纯文本 parts 合并为字符串；含非文本 part（图片/文件/音频）时保留数组。
func responsesContentToChatContent(content any) any {
	if content == nil {
		return nil
	}
	if s, ok := content.(string); ok {
		return s
	}
	parts, ok := content.([]any)
	if !ok {
		return content
	}
	chatParts := []any{}
	texts := []string{}
	hasNonText := false
	for _, raw := range parts {
		part, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		switch stringField(part, "type") {
		case "input_text", "output_text", "text":
			if text := rawString(part, "text"); text != "" {
				chatParts = append(chatParts, map[string]any{"type": "text", "text": text})
				texts = append(texts, text)
			}
		case "refusal":
			if text := rawString(part, "refusal"); text != "" {
				chatParts = append(chatParts, map[string]any{"type": "text", "text": text})
				texts = append(texts, text)
			}
		case "input_image":
			if img, ok := part["image_url"]; ok {
				url := img
				if _, isObj := img.(map[string]any); !isObj {
					url = map[string]any{"url": strVal(img)}
				}
				chatParts = append(chatParts, map[string]any{"type": "image_url", "image_url": url})
				hasNonText = true
			}
		case "input_file":
			if f, ok := responsesInputFileToChatFile(part); ok {
				chatParts = append(chatParts, map[string]any{"type": "file", "file": f})
				hasNonText = true
			}
		case "input_audio":
			if a, ok := part["input_audio"]; ok {
				chatParts = append(chatParts, map[string]any{"type": "input_audio", "input_audio": a})
				hasNonText = true
			}
		}
	}
	if !hasNonText {
		return strings.Join(texts, "\n")
	}
	return chatParts
}

// responsesInputFileToChatFile 把 input_file part 转成 Chat file part。
func responsesInputFileToChatFile(part map[string]any) (map[string]any, bool) {
	if file, ok := part["file"].(map[string]any); ok {
		return file, true
	}
	out := map[string]any{}
	if id := stringField(part, "file_id"); id != "" {
		out["file_id"] = id
	}
	if data := stringField(part, "file_data"); data != "" {
		out["file_data"] = data
	}
	if name := stringField(part, "filename"); name != "" {
		out["filename"] = name
	}
	if len(out) == 0 {
		return nil, false
	}
	return out, true
}

// responsesFunctionCallToChatToolCall 把 Responses function_call 转成 Chat tool_call。
func responsesFunctionCallToChatToolCall(item map[string]any, ctx *ToolContext) map[string]any {
	chatName := ctx.chatNameForResponseFunction(stringField(item, "name"), stringField(item, "namespace"))
	return map[string]any{
		"id":   responseItemCallID(item),
		"type": "function",
		"function": map[string]any{
			"name":      chatName,
			"arguments": canonicalizeToolArguments(item["arguments"]),
		},
	}
}

// responsesCustomToolCallToChatToolCall 把 custom_tool_call 转成 Chat tool_call
// （input 包进 {input: ...} arguments）。
func responsesCustomToolCallToChatToolCall(item map[string]any) map[string]any {
	input := item["input"]
	if input == nil {
		input = ""
	}
	args, _ := json.Marshal(map[string]any{customToolInputField: input})
	return map[string]any{
		"id":   responseItemCallID(item),
		"type": "function",
		"function": map[string]any{
			"name":      stringField(item, "name"),
			"arguments": string(args),
		},
	}
}

// responsesToolSearchCallToChatToolCall 把 tool_search_call 转成 Chat tool_call。
func responsesToolSearchCallToChatToolCall(item map[string]any) map[string]any {
	args := canonicalizeToolArguments(item["arguments"])
	if strings.TrimSpace(args) == "" {
		args = "{}"
	}
	return map[string]any{
		"id":   responseItemCallID(item),
		"type": "function",
		"function": map[string]any{
			"name":      toolSearchProxyName,
			"arguments": args,
		},
	}
}

// responseItemCallID 取 item 的 call_id（回退 id）。
func responseItemCallID(item map[string]any) string {
	return firstNonEmpty(stringField(item, "call_id"), stringField(item, "id"))
}

// responsesItemReasoning 提取 item 上的 reasoning 文本。
func responsesItemReasoning(item map[string]any) string {
	return extractReasoningFieldText(item)
}

// responsesReasoningItemText 提取 reasoning item 的文本。
func responsesReasoningItemText(item map[string]any) string {
	if text := extractReasoningFieldText(item); text != "" {
		return text
	}
	if summary, ok := item["summary"]; ok {
		return extractReasoningSummaryText(summary)
	}
	return ""
}

// joinReasoning 追加 reasoning 文本（非空时以空行分隔）。
func joinReasoning(existing, add string) string {
	add = strings.TrimSpace(add)
	if add == "" {
		return existing
	}
	if existing == "" {
		return add
	}
	return existing + "\n\n" + add
}

// attachReasoningToPreviousAssistant 把剩余 pending reasoning 回溯附挂到上一条
// assistant 消息（无上一条 assistant 时自然丢弃）。
func attachReasoningToPreviousAssistant(messages []any, lastAssistant int, pending *string) {
	if *pending == "" || lastAssistant < 0 || lastAssistant >= len(messages) {
		return
	}
	if msg, ok := messages[lastAssistant].(map[string]any); ok {
		msg["reasoning_content"] = joinReasoning(rawString(msg, "reasoning_content"), *pending)
	}
	*pending = ""
}

// collapseSystemMessages 把 system 消息折叠到 messages 头部。
func collapseSystemMessages(messages []any) []any {
	systems := []any{}
	rest := []any{}
	for _, m := range messages {
		if msg, ok := m.(map[string]any); ok && msg["role"] == "system" {
			systems = append(systems, m)
		} else {
			rest = append(rest, m)
		}
	}
	return append(systems, rest...)
}

// applyReasoning 把 Responses 的 reasoning.effort 映射到 Chat 的 reasoning_effort。
// 更复杂的按供应商能力注入（thinking/enable_thinking）交由项目既有 payload.go 管线。
func applyReasoning(out, req map[string]any) {
	reasoning, _ := req["reasoning"].(map[string]any)
	if reasoning == nil {
		return
	}
	effort, _ := reasoning["effort"].(string)
	if effort == "" || isEffortDisabled(effort) {
		return
	}
	out["reasoning_effort"] = effort
}

// isEffortDisabled 报告 effort 是否表达「显式关闭推理」。
func isEffortDisabled(effort string) bool {
	switch strings.ToLower(strings.TrimSpace(effort)) {
	case "none", "off", "disabled":
		return true
	default:
		return false
	}
}

// canonicalizeToolArguments 把 tool arguments 规范化为字符串：
// 字符串原样返回，其他值 JSON 序列化（Go 的 map 序列化按键排序）。
func canonicalizeToolArguments(v any) string {
	switch args := v.(type) {
	case nil:
		return ""
	case string:
		return args
	default:
		raw, err := json.Marshal(args)
		if err != nil {
			return ""
		}
		return string(raw)
	}
}

// toolOutputString 把 tool output 转成字符串：字符串原样，其他值 JSON 序列化。
func toolOutputString(v any) string {
	switch out := v.(type) {
	case nil:
		return ""
	case string:
		return out
	default:
		raw, err := json.Marshal(out)
		if err != nil {
			return ""
		}
		return string(raw)
	}
}
