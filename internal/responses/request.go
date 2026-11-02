package responses

import (
	"encoding/json"
	"fmt"
	"log"
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

	// 工具名单提示（见 toolRosterHint）：与 instructions 一样在收尾时被
	// collapseSystemMessages 合并进头部唯一 system 消息（参考实现的单 system
	// 形态），约束模型行为的同时保持 prompt cache 前缀稳定。压缩回合不带工具，
	// 也不注入名单（避免诱导模型发起调用）。
	chatTools := toolCtx.ChatTools()
	if len(chatTools) > 0 && !toolCtx.IsCompaction() {
		messages = append(messages, map[string]any{"role": "system", "content": toolRosterHint(chatTools)})
	}
	// 非法历史工具调用降级为文本（对齐 cc-switch #8241）：arguments 不是合法
	// JSON 对象时，严格 Chat 上游会对之后每次重放都 400，整条会话报废。
	var recovered int
	messages, recovered = preserveInvalidToolCallsAsText(messages)
	if recovered > 0 {
		log.Printf("WARN: [responses] preserved %d malformed historical tool call(s) as text", recovered)
	}
	// DeepSeek 严格校验 tool 结果与发起调用的 assistant 相邻：把夹在中间的
	// 回放正文/思考合并回该 assistant（其他模型不动，避免破坏 prompt cache）。
	if model, _ := req["model"].(string); isDeepSeekChatModel(model) {
		messages = coalesceReplayedToolTurnMessages(messages)
	}
	// 带 tool_calls 的 assistant 必须携带非空 reasoning_content（kimi/DeepSeek
	// 等 thinking 模型校验；跨轮恢复 miss 时防 400）。
	messages = backfillToolCallReasoningPlaceholders(messages)
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

	// tool_choice 只在确有工具时下发：没有 tools 却带 tool_choice，严格上游
	// 会直接 400（对齐 cc-switch #3640）。
	// 压缩回合只要一段摘要：不带工具与结构化输出（与 Codex 本地压缩请求同形）。
	if tools := toolCtx.ChatTools(); len(tools) > 0 && !toolCtx.IsCompaction() {
		out["tools"] = tools
		if tc, ok := req["tool_choice"]; ok {
			out["tool_choice"] = responsesToolChoiceToChat(tc, toolCtx)
		}
	}

	for _, k := range extraChatPassthroughFields {
		if toolCtx.IsCompaction() && k == "response_format" {
			continue
		}
		if v, ok := req[k]; ok {
			out[k] = v
		}
	}

	// 没有 tools 时 tool_choice / parallel_tool_calls 一并剔除：严格上游会 400
	// （对齐参考 #3640）。
	if _, hasTools := out["tools"]; !hasTools {
		delete(out, "tool_choice")
		delete(out, "parallel_tool_calls")
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
	var pendingMedia []any
	var pendingOrphans []any // 无配对调用的工具结果：延后到回合边界降级为 user 消息
	seenCalls := map[string]bool{}
	lastAssistant := -1

	flushCalls := func() {
		if len(pendingCalls) == 0 {
			return
		}
		// 最后一条是 assistant 且尚无 tool_calls → 并入它（对齐 cc-switch
		// merge_pending_tool_calls_into_adjacent_assistant，避免多出空 assistant 消息）。
		for j := len(messages) - 1; j >= 0; j-- {
			last, ok := messages[j].(map[string]any)
			if !ok {
				break
			}
			if last["role"] == "system" {
				continue // 跳过尾部 system（参考 rposition 口径）
			}
			if last["role"] == "assistant" {
				if tcs, ok := last["tool_calls"].([]any); !ok || len(tcs) == 0 {
					last["tool_calls"] = pendingCalls
					if pendingReasoning != "" {
						last["reasoning_content"] = joinReasoningUnique(rawString(last, "reasoning_content"), pendingReasoning)
					}
					pendingCalls = nil
					pendingReasoning = ""
					return
				}
			}
			break
		}
		// assistant 带 tool_calls 时显式 content:null（严格上游要求字段存在）。
		msg := map[string]any{"role": "assistant", "content": nil, "tool_calls": pendingCalls}
		if pendingReasoning != "" {
			msg["reasoning_content"] = pendingReasoning
		}
		messages = append(messages, msg)
		lastAssistant = len(messages) - 1
		pendingCalls = nil
		pendingReasoning = ""
	}

	// flushMedia 把累计的工具结果媒体作为一条合成 user 消息投递
	// （Chat 的 tool 消息是纯文本，媒体必须搬到 user 消息）。
	flushMedia := func() {
		if len(pendingMedia) == 0 {
			return
		}
		messages = append(messages, map[string]any{"role": "user", "content": pendingMedia})
		pendingMedia = nil
	}

	// flushOrphans 在回合边界把暂存的孤儿结果降级成 user 消息（对齐参考
	// flush_pending_orphan_results）：立刻输出会形成 tool,user,tool，严格上游 400。
	flushOrphans := func() {
		if len(pendingOrphans) == 0 {
			return
		}
		for _, raw := range pendingOrphans {
			if item, ok := raw.(map[string]any); ok {
				messages = append(messages, orphanToolOutputMessage(item))
			}
		}
		pendingOrphans = nil
	}

	// appendUserItem 追加一条 user 消息（压缩提示词 / 摘要回放共用）：按回合边界
	// 处理 pending reasoning，不产生 assistant 索引。
	appendUserItem := func(msg map[string]any) {
		flushCalls()
		flushMedia()
		flushOrphans()
		attachReasoningToPreviousAssistant(messages, lastAssistant, &pendingReasoning)
		messages = append(messages, msg)
	}

	for _, raw := range items {
		item, ok := raw.(map[string]any)
		if !ok {
			if s, ok := raw.(string); ok {
				appendUserItem(map[string]any{"role": "user", "content": s})
			}
			continue
		}
		// 非工具结果条目 = 回合边界：先降级孤儿结果（hoisted system 会被合并到
		// 头部，不构成工具序列中断，参考同口径跳过）。
		if !isToolResultItem(item) && !isSystemItem(item) {
			flushOrphans()
		}
		switch stringField(item, "type") {
		case "function_call":
			flushMedia()
			pendingReasoning = joinReasoningUnique(pendingReasoning, responsesItemReasoning(item))
			if id := responseItemCallID(item); id != "" {
				seenCalls[id] = true
			}
			pendingCalls = append(pendingCalls, responsesFunctionCallToChatToolCall(item, ctx))
		case "custom_tool_call":
			flushMedia()
			pendingReasoning = joinReasoningUnique(pendingReasoning, responsesItemReasoning(item))
			if id := responseItemCallID(item); id != "" {
				seenCalls[id] = true
			}
			pendingCalls = append(pendingCalls, responsesCustomToolCallToChatToolCall(item))
		case "tool_search_call":
			flushMedia()
			pendingReasoning = joinReasoningUnique(pendingReasoning, responsesItemReasoning(item))
			if id := responseItemCallID(item); id != "" {
				seenCalls[id] = true
			}
			pendingCalls = append(pendingCalls, responsesToolSearchCallToChatToolCall(item))
		case "function_call_output":
			flushCalls()
			callID := responseItemCallID(item)
			if callID == "" || !seenCalls[callID] {
				pendingOrphans = append(pendingOrphans, item)
				continue
			}
			toolContent, media := planToolOutputMedia(item["output"])
			queueToolMedia(&pendingMedia, callID, media)
			if len(media) == 0 {
				if _, isString := item["output"].(string); isString {
					// 字符串输出可解析为 JSON 时规范化（参考 canonicalize_json_string_if_parseable）。
					toolContent = canonicalizeJSONStringIfParseable(toolContent)
				}
			}
			messages = append(messages, map[string]any{
				"role":         "tool",
				"tool_call_id": callID,
				"content":      toolContent,
			})
		case "custom_tool_call_output", "tool_search_output":
			flushCalls()
			callID := responseItemCallID(item)
			if callID == "" || !seenCalls[callID] {
				pendingOrphans = append(pendingOrphans, item)
				continue
			}
			// 参考口径：custom/tool_search 结果以「整条 item 的 canonical JSON」
			// 作为 tool content；媒体剥离后回写 output 字段。
			transformed := cloneStringMap(item)
			output, media := planToolOutputMedia(item["output"])
			if len(media) > 0 {
				queueToolMedia(&pendingMedia, callID, media)
				transformed["output"] = output
			}
			messages = append(messages, map[string]any{
				"role":         "tool",
				"tool_call_id": callID,
				"content":      canonicalJSON(transformed),
			})
		case "compaction_trigger":
			// Codex 远程压缩：触发条目换成压缩提示词（用户消息）。
			appendUserItem(compactionPromptItem())
		case "compaction", "compaction_summary", "context_compaction":
			// 历史压缩条目：解回摘要正文回放给模型；解不开的密文降级为说明文本。
			if text, ok := compactionItemReplayText(item); ok {
				appendUserItem(compactionSummaryUserItem(text))
			}
		case "reasoning":
			flushMedia()
			pendingReasoning = joinReasoning(pendingReasoning, responsesReasoningItemText(item))
		case "additional_tools":
			// 工具声明已在 buildToolContext 提升，此处不产生消息。
		case "input_text", "input_image", "input_file", "input_audio":
			// 裸 content-part item（无 type=message）：按单条消息处理，不丢内容。
			flushCalls()
			flushMedia()
			flushOrphans()
			synthetic := map[string]any{
				"type":    "message",
				"role":    firstNonEmpty(stringField(item, "role"), "user"),
				"content": []any{item},
			}
			msg := responsesMessageToChatMessage(synthetic)
			attachReasoningToPreviousAssistant(messages, lastAssistant, &pendingReasoning)
			messages = append(messages, msg)
		default:
			if stringField(item, "type") == "message" || item["role"] != nil || item["content"] != nil {
				flushCalls()
				flushMedia()
				flushOrphans()
				msg := responsesMessageToChatMessage(item)
				if msg["role"] == "assistant" {
					// message item 自身也可能带 reasoning（客户端回放历史时把思考
					// 挂在 assistant message 上，对齐 cc-switch responses_message_
					// item_to_chat_message）：先收自身，再并入 pending。
					if own := responsesItemReasoning(item); own != "" {
						msg["reasoning_content"] = joinReasoningUnique(rawString(msg, "reasoning_content"), own)
					}
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
	flushMedia()
	flushOrphans()
	attachReasoningToPreviousAssistant(messages, lastAssistant, &pendingReasoning)
	return messages
}

// canonicalJSON 把值序列化为键序稳定的 JSON 字符串。
func canonicalJSON(v any) string {
	raw, err := json.Marshal(v)
	if err != nil {
		return "{}"
	}
	return string(raw)
}

// isToolResultItem 报告 item 是否为工具结果条目。
func isToolResultItem(item map[string]any) bool {
	switch stringField(item, "type") {
	case "function_call_output", "custom_tool_call_output", "tool_search_output":
		return true
	}
	return false
}

// isSystemItem 报告 item 是否 system/developer 角色（会被合并到头部）。
func isSystemItem(item map[string]any) bool {
	role := stringField(item, "role")
	return role == "system" || role == "developer"
}

// cloneStringMap 浅拷贝 top-level map（只用于替换 output 字段，避免改动原始 item）。
func cloneStringMap(src map[string]any) map[string]any {
	dst := make(map[string]any, len(src))
	for k, v := range src {
		dst[k] = v
	}
	return dst
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

// joinReasoningUnique 同 joinReasoning，但已包含该文本时跳过（对齐 cc-switch
// append_unique_pending_reasoning，避免同一段 reasoning 被重复拼接）。
func joinReasoningUnique(existing, add string) string {
	add = strings.TrimSpace(add)
	if add == "" {
		return existing
	}
	if existing == "" {
		return add
	}
	// 按空行分段去重（对齐参考#1341-1381 的 segment 口径）：子串判断会把
	// "abc" 含 "bc" 误判成重复而丢段。
	splitSegments := func(s string) []string {
		var out []string
		for _, seg := range strings.Split(s, "\n\n") {
			if seg = strings.TrimSpace(seg); seg != "" {
				out = append(out, seg)
			}
		}
		return out
	}
	existingSegs := splitSegments(existing)
	seen := make(map[string]bool, len(existingSegs))
	for _, seg := range existingSegs {
		seen[seg] = true
	}
	var missing []string
	for _, seg := range splitSegments(add) {
		if !seen[seg] {
			missing = append(missing, seg)
		}
	}
	if len(missing) == 0 {
		return existing
	}
	return existing + "\n\n" + strings.Join(missing, "\n\n")
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

// collapseSystemMessages 把字符串 content 的 system 消息合并成一条并放到
// messages 头部（对齐参考 collapse_system_messages_to_head）：部分上游只允许
// 首条 system（MiniMax 等），逐条保留会 400；合并同时稳定提示词前缀。
// 非字符串 content 的 system 消息保持原位（参考同口径）。
func collapseSystemMessages(messages []any) []any {
	chunks := []string{}
	rest := make([]any, 0, len(messages))
	for _, m := range messages {
		if msg, ok := m.(map[string]any); ok && msg["role"] == "system" {
			if text, ok := msg["content"].(string); ok {
				if strings.TrimSpace(text) != "" {
					chunks = append(chunks, text)
				}
				continue
			}
		}
		rest = append(rest, m)
	}
	if len(chunks) == 0 {
		return rest
	}
	out := make([]any, 0, len(rest)+1)
	out = append(out, map[string]any{"role": "system", "content": strings.Join(chunks, "\n\n")})
	return append(out, rest...)
}

// backfillToolCallReasoningPlaceholders 给带 tool_calls 但缺非空 reasoning_content
// 的 assistant 补占位 "tool call"（对齐参考 backfill_tool_call_reasoning_placeholders）：
// kimi/Moonshot、DeepSeek 等 thinking 模型要求该字段非空，跨轮恢复 miss 时防 400。
func backfillToolCallReasoningPlaceholders(messages []any) []any {
	for _, raw := range messages {
		msg, ok := raw.(map[string]any)
		if !ok || msg["role"] != "assistant" {
			continue
		}
		calls, _ := msg["tool_calls"].([]any)
		if len(calls) == 0 {
			continue
		}
		if rc, ok := msg["reasoning_content"].(string); ok && strings.TrimSpace(rc) != "" {
			continue
		}
		msg["reasoning_content"] = "tool call"
	}
	return messages
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

// canonicalizeToolArguments 把 tool arguments 规范化为字符串（对齐 cc-switch
// json_canonical）：缺失/空 → "{}"；字符串若是 JSON 则规范化（Go 的 map 序列化
// 按键排序，保证参数顺序稳定）；其他值 JSON 序列化。
func canonicalizeToolArguments(v any) string {
	switch args := v.(type) {
	case nil:
		return "{}"
	case string:
		return canonicalizeJSONStringIfParseable(args)
	default:
		raw, err := json.Marshal(args)
		if err != nil {
			return "{}"
		}
		return string(raw)
	}
}

// canonicalizeJSONStringIfParseable 若字符串是合法 JSON 则规范化（键排序），否则原样返回。
func canonicalizeJSONStringIfParseable(s string) string {
	if strings.TrimSpace(s) == "" {
		return "{}"
	}
	var parsed any
	if json.Unmarshal([]byte(s), &parsed) != nil {
		return s
	}
	raw, err := json.Marshal(parsed)
	if err != nil {
		return s
	}
	return string(raw)
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

// toolRosterHint 生成「工具名单提示」：声明本次会话可用的工具全集，禁止调用
// 名单之外的名字。背景：codex 新版（0.15x）注册的工具是 exec_command /
// write_stdin / view_image 等，glm / deepseek 类模型按训练记忆常输出旧版 codex
// 的 shell / apply_patch / update_plan——codex 端报 "unsupported call: shell"
// 并拒绝执行（实测复现）。名单放在消息序列末尾（collapseSystemMessages 之后追加），
// 贴近模型生成 tool_calls 的位置，约束力最强。
func toolRosterHint(chatTools []any) string {
	var b strings.Builder
	b.WriteString("Tool availability for this turn (authoritative list):\n")
	for _, raw := range chatTools {
		tool, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		fn, _ := tool["function"].(map[string]any)
		name := rawString(fn, "name")
		if name == "" {
			name = rawString(tool, "name")
		}
		if name == "" {
			continue
		}
		b.WriteString("- ")
		b.WriteString(name)
		if desc := rawString(fn, "description"); desc != "" {
			// 单行摘要：截到首句/120 字符，避免提示本身膨胀。
			if i := strings.IndexAny(desc, ".\n"); i > 0 && i < 120 {
				desc = desc[:i]
			} else if len(desc) > 120 {
				desc = desc[:120]
			}
			b.WriteString(": ")
			b.WriteString(desc)
		}
		b.WriteString("\n")
	}
	b.WriteString(`Call tools ONLY by the exact names listed above. Do not invent or use tool names from other coding-agent versions (for example "shell", "apply_patch", "update_plan", "read_file" are NOT available unless listed above); use the closest listed tool instead. If no listed tool fits, answer in plain text instead of calling a tool.`)
	return b.String()
}
