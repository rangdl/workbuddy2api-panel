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
			out = append(out, blocksToChatMessage(role, c)...)
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

// blocksToChatMessage 把一个 Anthropic message 的 blocks 数组转成 0..n 条 Chat 消息。
// n>1 的场景：tool_result 块必须拆成独立的 tool 消息（A2）；A1 只产出文本/图片消息。
func blocksToChatMessage(role string, blocks []any) []any {
	// assistant 的 thinking 块提取为 reasoning_content（DeepSeek 多轮一致性管线消费；
	// 其他上游由 payload.go 既有管线决定去留）。
	var reasoning strings.Builder
	var textParts []string
	var chatParts []any // 含图片时保留数组形态
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
		case "tool_use", "tool_result", "document":
			// A2 处理 tool_use / tool_result；document（PDF）CodeBuddy 未验证，忽略。
			// A1 阶段遇到时静默跳过（不留残块）。
		}
	}

	content := chatContentOf(hasImage, chatParts, textParts)
	if content == nil && reasoning.Len() == 0 {
		return nil
	}
	msg := map[string]any{"role": role}
	if content != nil {
		msg["content"] = content
	}
	if reasoning.Len() > 0 {
		msg["reasoning_content"] = reasoning.String()
	}
	return []any{msg}
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
