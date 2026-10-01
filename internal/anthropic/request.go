package anthropic

import (
	"encoding/json"
	"fmt"
	"strings"
)

// ToChat 把 Anthropic Messages 请求体转换为 Chat Completions 请求体。
//
// 蓝本：cc-switch transform.rs anthropic_to_openai_with_reasoning_content（155-281 行）。
// 转换后的请求体是标准 Chat 形态，可直接交给既有 chatCompletions 链路
// （realm/粘性/提示词/脱敏/轮转/冷却 全部复用）。
//
// 第二个返回值是为 A2 工具链路预留的上下文（A1 恒为 nil，调用方无需判空——
// 与 responses.ToChat 的返回签名保持同构，后续补工具时不改调用方）。
func ToChat(body []byte) ([]byte, error) {
	var req map[string]any
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, fmt.Errorf("invalid anthropic request: %w", err)
	}
	out := map[string]any{}

	if model, ok := req["model"]; ok {
		out["model"] = model
	}

	messages := make([]any, 0, 4)
	if sys := systemToChatMessages(req["system"]); len(sys) > 0 {
		messages = append(messages, sys...)
	}
	if msgs, ok := req["messages"].([]any); ok {
		converted, err := messagesToChat(msgs)
		if err != nil {
			return nil, err
		}
		messages = append(messages, converted...)
	}
	out["messages"] = messages

	// max_tokens：Anthropic 必填而 Chat 可省——必须透传，不能丢。
	if v, ok := req["max_tokens"]; ok {
		out["max_tokens"] = v
	}
	for _, k := range []string{"temperature", "top_p", "stream"} {
		if v, ok := req[k]; ok {
			out[k] = v
		}
	}
	// stop_sequences → stop（Anthropic 侧为必填字段 stop_sequences；Chat 叫 stop）。
	if v, ok := req["stop_sequences"]; ok {
		out["stop"] = v
	}

	// thinking → reasoning_effort（见 applyThinking）。
	applyThinking(out, req)

	// tools / tool_choice（A2；映射规则见 toolsToChat / toolChoiceToChat）。
	if tools, ok := req["tools"].([]any); ok {
		if chatTools := toolsToChat(tools); len(chatTools) > 0 {
			out["tools"] = chatTools
		}
	}
	if tc, ok := req["tool_choice"]; ok {
		out["tool_choice"] = toolChoiceToChat(tc)
	}

	// 其余 Anthropic 专有字段**有意忽略**（Chat 上游无对应语义）：
	//   - metadata.user_id —— 会话标识（粘性接入为 P2，见方案 §6.4）；
	//   - top_k —— CodeBuddy 未实测，丢弃防 400；
	//   - cache_control（块级）—— Chat 无对应语义，块转换时直接不携带。

	raw, err := json.Marshal(out)
	if err != nil {
		return nil, err
	}
	return raw, nil
}

// billingHdrPrefix Claude Code 注入 system prompt 的指纹头前缀（与 upstream/sanitize.go
// 的 x-anthropic-billing-hdr 同源）。sanitize 层清洗 messages 内的指纹，但顶层 system
// 字段不经过 sanitizeMessages，必须在转换层先剥，否则整单被上游审核拦截。
const billingHdrPrefix = "x-anthropic-billing-hdr"

// stripBillingHeader 从 system 文本里剥离 CC 的 billing 指纹行（如 "x-anthropic-billing-hdr: ..."）。
// 只删指纹行，其余内容原样保留（保留原始换行结构，护 prompt cache 字节稳定）。
func stripBillingHeader(text string) string {
	if !strings.Contains(strings.ToLower(text), billingHdrPrefix) {
		return text
	}
	var kept []string
	for _, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(strings.ToLower(strings.TrimSpace(line)), billingHdrPrefix) {
			continue
		}
		kept = append(kept, line)
	}
	return strings.Join(kept, "\n")
}

// systemToChatMessages 把顶层 system（字符串或 blocks 数组）转为首条 system 消息。
// blocks 数组合并为一条消息（跨轮字节稳定，护 prompt cache——cc-switch 同口径）。
func systemToChatMessages(v any) []any {
	switch s := v.(type) {
	case string:
		text := strings.TrimSpace(stripBillingHeader(s))
		if text == "" {
			return nil
		}
		return []any{map[string]any{"role": "system", "content": text}}
	case []any:
		var parts []string
		for _, raw := range s {
			block, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			if stringField(block, "type") != "text" {
				continue
			}
			text := stripBillingHeader(rawString(block, "text"))
			if strings.TrimSpace(text) == "" {
				continue
			}
			parts = append(parts, text)
		}
		if len(parts) == 0 {
			return nil
		}
		return []any{map[string]any{"role": "system", "content": strings.Join(parts, "\n")}}
	}
	return nil
}

// messagesToChat 把 Anthropic messages 数组转为 Chat messages。
// A1 处理 text/image/thinking 块；tool_use/tool_result 在 A2 补（预留 ToolContext 位）。
func messagesToChat(msgs []any) ([]any, error) {
	out := make([]any, 0, len(msgs))
	for _, raw := range msgs {
		msg, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		role := anthropicRoleToChatRole(stringField(msg, "role"))
		content := msg["content"]
		switch c := content.(type) {
		case string:
			// 纯字符串形态（部分客户端这么发）：单条消息。
			if strings.TrimSpace(c) == "" {
				continue
			}
			out = append(out, map[string]any{"role": role, "content": c})
		case []any:
			out = append(out, blocksToChatMessages(role, c)...)
		case nil:
			// content=null 仅在 assistant 带 tool_calls 时合法（A2）；A1 跳过空消息。
			continue
		}
	}
	return out, nil
}

// anthropicRoleToChatRole 映射 Anthropic role 到 Chat role。
// Anthropic 只有 user/assistant 两态（system 在顶层），其余值防御性归为 user。
func anthropicRoleToChatRole(role string) string {
	if role == "assistant" {
		return "assistant"
	}
	return "user"
}

// blocksToChatMessages 把一个 Anthropic message 的 blocks 数组转成 1..n 条 Chat 消息。
// n>1 的场景：user 消息里的 tool_result 块（CC 并行工具回喂）必须拆成独立的
// role:tool 消息，且紧跟所属 assistant 的 tool_calls 之后——与本网关
// repackToolResultBlocks 的「同批结果连续」预期一致，cleanupOrphanToolCalls 兜底配对。
// assistant 消息的 tool_use 块累积为该消息的 tool_calls；纯 tool_use 时 content=null
// （Chat 规范形态，空串会被严格上游 400）。
func blocksToChatMessages(role string, blocks []any) []any {
	// assistant 的 thinking 块提取为 reasoning_content（DeepSeek 多轮一致性管线消费；
	// 其他上游由 payload.go 既有管线决定去留）。
	var reasoning strings.Builder
	var textParts []string
	var chatParts []any // 含图片时保留数组形态
	var toolCalls []any // assistant: tool_use 块累积
	var toolMsgs []any  // user: tool_result 块生成的独立 tool 消息
	hasImage := false

	for _, raw := range blocks {
		block, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		switch stringField(block, "type") {
		case "text":
			if text := rawString(block, "text"); strings.TrimSpace(text) != "" {
				textParts = append(textParts, text)
				chatParts = append(chatParts, map[string]any{"type": "text", "text": text})
			}
		case "image":
			if part, ok := anthropicImageToChatPart(block); ok {
				chatParts = append(chatParts, part)
				hasImage = true
			}
		case "thinking":
			if t := rawString(block, "thinking"); t != "" {
				reasoning.WriteString(joinReasoning(reasoning.String(), t))
			}
		case "redacted_thinking":
			// CC 把历史 thinking 加密成此块，内容不可还原。reasoning_content 需要
			// 非空占位（DeepSeek 对 thinking 形态校验 len>0，见 upstream/thinking.go），
			// 占位对模型上下文无语义影响。
			reasoning.WriteString(joinReasoning(reasoning.String(), "[redacted thinking]"))
		case "tool_use":
			// assistant 发起的调用：累积为 tool_calls（蓝本 transform.rs:387-398）。
			id := stringField(block, "id")
			name := stringField(block, "name")
			if id == "" || name == "" {
				continue // 缺 id/name 的调用无法配对，丢弃（畸形防御）
			}
			toolCalls = append(toolCalls, map[string]any{
				"id":   id,
				"type": "function",
				"function": map[string]any{
					"name":      name,
					"arguments": canonicalizeToolArguments(block["input"]),
				},
			})
		case "tool_result":
			// 工具结果：独立 tool 消息（蓝本 transform.rs:400-431）。
			// content 三形态：字符串原样 / blocks 数组合并文本 / 对象 JSON 序列化。
			callID := stringField(block, "tool_use_id")
			if callID == "" {
				continue
			}
			toolMsgs = append(toolMsgs, map[string]any{
				"role":         "tool",
				"tool_call_id": callID,
				"content":      toolResultContent(block["content"]),
			})
		case "document":
			// PDF 块：CodeBuddy 未验证 file 形态，忽略（不留残块）。
		}
	}

	// 组装主体消息（assistant 带 tool_calls 时 content=null）。
	var main map[string]any
	content := chatContentOf(hasImage, chatParts, textParts)
	if content == nil && len(toolCalls) == 0 && reasoning.Len() == 0 {
		// 全空消息（如 assistant 只有空块）：跳过。
	} else {
		main = map[string]any{"role": role}
		// assistant 带 tool_calls 时 content=null；文本/图片正常放。
		if role == "assistant" && len(toolCalls) > 0 {
			if content != nil {
				main["content"] = content
			} else {
				main["content"] = nil
			}
		} else if content != nil {
			main["content"] = content
		}
		if reasoning.Len() > 0 {
			main["reasoning_content"] = reasoning.String()
		}
		if len(toolCalls) > 0 {
			main["tool_calls"] = toolCalls
		}
	}

	var out []any
	if main != nil {
		out = append(out, main)
	}
	// tool 结果消息跟在主体之后（顺序即 wire 顺序）。
	out = append(out, toolMsgs...)
	return out
}

// toolResultContent 把 Anthropic tool_result.content 转成 Chat tool 消息的字符串形态：
// 字符串原样；blocks 数组提取 text 部分合并；其他值 JSON 序列化。
// （媒体块剥离为后续增强，与 responses 包 media.go 同思路；第一阶段文本化。）
func toolResultContent(v any) string {
	switch c := v.(type) {
	case nil:
		return ""
	case string:
		return c
	case []any:
		var texts []string
		for _, raw := range c {
			block, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			switch stringField(block, "type") {
			case "text":
				if t := rawString(block, "text"); t != "" {
					texts = append(texts, t)
				}
			case "image":
				texts = append(texts, "[image omitted]")
			}
		}
		return strings.Join(texts, "\n")
	default:
		return canonicalJSONStringOf(v)
	}
}

// chatContentOf 依图片有无决定 content 形态：纯文本合并为字符串（prompt cache 友好），
// 含图片保留数组。
func chatContentOf(hasImage bool, chatParts []any, textParts []string) any {
	if hasImage {
		if len(chatParts) == 0 {
			return nil
		}
		return chatParts
	}
	if len(textParts) == 0 {
		return nil
	}
	return strings.Join(textParts, "\n")
}

// anthropicImageToChatPart 把 Anthropic image 块转成 Chat image_url part。
// source 两种形态：base64（{"type":"base64","media_type","data"}）与 URL。
func anthropicImageToChatPart(block map[string]any) (map[string]any, bool) {
	source, ok := block["source"].(map[string]any)
	if !ok {
		return nil, false
	}
	switch stringField(source, "type") {
	case "base64":
		media := stringField(source, "media_type")
		data := rawString(source, "data")
		if media == "" || data == "" {
			return nil, false
		}
		url := "data:" + media + ";base64," + data
		return map[string]any{"type": "image_url", "image_url": map[string]any{"url": url}}, true
	case "url":
		url := stringField(source, "url")
		if url == "" {
			return nil, false
		}
		return map[string]any{"type": "image_url", "image_url": map[string]any{"url": url}}, true
	}
	return nil, false
}

// joinReasoning 追加 reasoning 文本（非空时以空行分隔；与 responses 包同口径）。
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

// applyThinking 把 Anthropic thinking 配置映射到 Chat reasoning_effort。
//
// 蓝本：cc-switch transform.rs resolve_reasoning_effort——thinking.type=enabled 时按
// budget_tokens 分档；未开 thinking 不注入任何字段（避免触发本网关 DeepSeek
// thinking 注入管线误开思维链，upstream/thinking.go）。
// 档位简化：budget ≥ 8192 → high，其余 → medium（CodeBuddy 上游实测支持 low/medium/high）。
func applyThinking(out, req map[string]any) {
	thinking, _ := req["thinking"].(map[string]any)
	if thinking == nil || stringField(thinking, "type") != "enabled" {
		return
	}
	budget := numToU64(thinking["budget_tokens"])
	if budget >= 8192 {
		out["reasoning_effort"] = "high"
	} else {
		out["reasoning_effort"] = "medium"
	}
}

// toolsToChat 把 Anthropic tools 数组转成 Chat tools。
//
// 蓝本：cc-switch transform.rs:246-272。
//   - 扁平 function 工具：input_schema → parameters；description 缺失时省略字段
//     而非 null（严格上游收到 null 会 400 "expected string, received null"）；
//   - server 工具（web_search_* 等）与 BatchTool：Chat 上游无法执行，丢弃
//     （与 Responses 侧 web_search 同口径——保留会让严格上游 400）。
func toolsToChat(tools []any) []any {
	out := make([]any, 0, len(tools))
	for _, raw := range tools {
		tool, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		// server 工具（web_search_*/web_search_*_20xx/code_execution_*/computer_* 等）
		// 与 BatchTool：type 不是省略或缺省的 "custom" 语义时丢弃。Anthropic 里
		// 自定义 function 工具没有 type 字段（或 type="custom"），server 工具的
		// type 是 "名字_日期" 形态。Chat 上游无法执行服务端工具，保留会让严格上游 400。
		if typ := stringField(tool, "type"); typ != "" && typ != "custom" {
			continue
		}
		name := stringField(tool, "name")
		if name == "" {
			continue
		}
		fn := map[string]any{"name": name}
		if desc, ok := tool["description"]; ok && desc != nil {
			fn["description"] = desc
		}
		fn["parameters"] = normalizeParameters(tool["input_schema"])
		out = append(out, map[string]any{"type": "function", "function": fn})
	}
	return out
}

// normalizeParameters 保证 parameters.type 恒为 "object"（严格 OpenAI 兼容上游要求；
// 与 responses 包 normalizeFunctionParameters 同口径）。
func normalizeParameters(params any) map[string]any {
	obj, ok := params.(map[string]any)
	if !ok {
		return map[string]any{"type": "object", "properties": map[string]any{}}
	}
	out := make(map[string]any, len(obj))
	for k, v := range obj {
		out[k] = v
	}
	if s, _ := out["type"].(string); s != "object" {
		out["type"] = "object"
	}
	return out
}

// toolChoiceToChat 把 Anthropic tool_choice 转成 Chat 形态（蓝本 transform.rs map_tool_choice_to_chat）。
//
//	{"type":"auto"}  → "auto"
//	{"type":"any"}   → "required"
//	{"type":"none"}  → "none"（本网关 payload.go 会同时删 tools——语义正确）
//	{"type":"tool","name":N} → {"type":"function","function":{"name":N}}
func toolChoiceToChat(v any) any {
	obj, ok := v.(map[string]any)
	if !ok {
		return v
	}
	switch stringField(obj, "type") {
	case "auto":
		return "auto"
	case "any":
		return "required"
	case "none":
		return "none"
	case "tool":
		name := stringField(obj, "name")
		if name == "" {
			return "auto"
		}
		return map[string]any{"type": "function", "function": map[string]any{"name": name}}
	default:
		return "auto"
	}
}
