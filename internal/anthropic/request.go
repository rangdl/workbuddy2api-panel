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

// stripBillingHeader 只剥离 system 开头的那一行 billing 指纹（如
// "x-anthropic-billing-hdr: ..."）。后续出现的前缀行可能是用户自己写的 prompt，
// 一律保留（对齐参考 strip_leading_anthropic_billing_header）。
func stripBillingHeader(text string) string {
	if !strings.HasPrefix(text, billingHdrPrefix) {
		return text
	}
	idx := strings.IndexAny(text, "\r\n")
	if idx < 0 {
		return ""
	}
	rest := text[idx+1:]
	// 吃掉一对 CRLF 的剩余半行。
	rest = strings.TrimPrefix(rest, "\r\n")
	rest = strings.TrimPrefix(rest, "\n")
	rest = strings.TrimPrefix(rest, "\r")
	return rest
}

// systemToChatMessages 把顶层 system（字符串或 blocks 数组）转为首条 system 消息。
// blocks 数组合并为一条消息（跨轮字节稳定，护 prompt cache——cc-switch 同口径）。
func systemToChatMessages(v any) []any {
	switch s := v.(type) {
	case string:
		text := stripBillingHeader(s)
		if strings.TrimSpace(text) == "" {
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
// system 在顶层，但 Claude Code 新版本也会在 messages 里发 system（cc-switch
// issue #4778），必须原样保留 system 语义，不能改写成 user。
func anthropicRoleToChatRole(role string) string {
	switch role {
	case "assistant":
		return "assistant"
	case "system", "developer":
		return "system"
	default:
		return "user"
	}
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
	var chatParts []any  // 含图片时保留数组形态
	var toolCalls []any  // assistant: tool_use 块累积
	var toolMsgs []any   // user: tool_result 块生成的独立 tool 消息
	var mediaParts []any // tool_result 里的图片：Chat tool 消息不收媒体，改为随后的 user 消息
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
			// Chat 的 tool 消息只收文本：图片搬进随后的合成 user 消息
			// （对齐 cc-switch tool_media.rs）。Claude Code 的 Read 读图
			// 正是把图片放在 tool_result 里，此前会被替换成 [image omitted]
			// 丢弃，模型根本看不到图。
			callID := stringField(block, "tool_use_id")
			if callID == "" {
				continue
			}
			text, media := splitToolResultContent(block["content"])
			if len(media) > 0 {
				mediaParts = append(mediaParts, map[string]any{
					"type": "text",
					"text": "[media output of tool call " + callID + "]",
				})
				mediaParts = append(mediaParts, media...)
			}
			toolMsgs = append(toolMsgs, map[string]any{
				"role":         "tool",
				"tool_call_id": callID,
				"content":      text,
			})
		case "document":
			// PDF 块：CodeBuddy 未验证 file 形态，忽略（不留残块）。
		}
	}

	// 组装主体消息（assistant 带 tool_calls 时 content=null）。
	var main map[string]any
	content := chatContentOf(hasImage, chatParts, textParts)
	if content == nil && len(toolCalls) == 0 {
		// 无正文、无工具调用（只有 thinking/空块）：整条丢弃（对齐参考
		// test_anthropic_to_openai_skips_thinking_only_message），否则会产出
		// 没有 content 字段的畸形 assistant 消息。
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
			// 本网关上游固定为 CodeBuddy（deepseek/glm 等），等价于参考实现的
			// preserve_reasoning_content=true：思考型模型要求多轮带 reasoning_content，
			// 故对所有 assistant 消息保留思考（generic Chat 上游才需要按工具调用门控）。
			main["reasoning_content"] = reasoning.String()
		}
		if len(toolCalls) > 0 {
			main["tool_calls"] = toolCalls
		}
	}

	// 顺序即 wire 顺序，必须与参考实现（cc-switch transform.rs）一致：user 消息里的
	// tool_result 先出，同一条消息里的正文文本后出。
	//
	// 2026-10-28 线上 11148 根因：Claude Code 中断工具调用 / 往当前会话插入消息时，
	// 同一条 user 消息会同时带 tool_result 与 text。此前固定「主体先、tool 后」，
	// 出站长成 assistant(tool_calls) → user(text) → tool(id)，结果被正文隔断，上游
	// deepseek 系判 11148（tool calls and tool results do not match）并顶死整条会话；
	// upstream.repackToolResultBlocks 在「assistant 后第一条不是 tool」时立即 break，
	// 兜不住这个形态（cleanupOrphanToolCalls 只删孤儿、不重排）。参考实现在遍历
	// blocks 时按原序推入 tool 消息、最后才追加主体，这里对齐。
	var out []any
	if role == "user" {
		out = append(out, toolMsgs...)
		if len(mediaParts) > 0 {
			out = append(out, map[string]any{"role": "user", "content": mediaParts})
		}
		if main != nil {
			out = append(out, main)
		}
		return out
	}
	if main != nil {
		out = append(out, main)
	}
	out = append(out, toolMsgs...)
	if len(mediaParts) > 0 {
		out = append(out, map[string]any{"role": "user", "content": mediaParts})
	}
	return out
}

// anthropicToolMediaMovedMarker 媒体块被搬离 tool 结果后的原位占位文本
// （文案对齐参考实现 cc-switch tool_media.rs 的 TOOL_RESULT_MEDIA_MOVED_MARKER）。
const anthropicToolMediaMovedMarker = "[tool result media moved to the following user message]"

// splitToolResultContent 把 Anthropic tool_result.content 拆成
// (Chat tool 消息的文本 content, 需要改走 user 消息的媒体 part 列表)。
//
// 规则对齐参考 tool_media.rs：媒体块原位替换成占位文本；字符串里的 JSON 会递归
// 解析继续找媒体；对象下探 content 字段；媒体搬走后对残余长 data:/base64 串裁剪。
//   - 字符串：整段是图片 data URL 时搬走；
//   - 数组/对象：递归剥离后按 canonical_json_string 口径序列化；
//   - nil → 空串。
func splitToolResultContent(v any) (string, []any) {
	var media []any
	newValue, replaced := stripToolResultMedia(v, &media, 0)
	if replaced == 0 {
		switch c := v.(type) {
		case nil:
			return "", nil
		case string:
			return c, nil
		default:
			return canonicalJSONStringOf(v), nil
		}
	}
	newValue = clampToolBase64ish(newValue)
	if s, ok := newValue.(string); ok {
		return s, media
	}
	return canonicalJSONStringOf(newValue), media
}

// stripToolResultMedia 递归剥离工具结果里的媒体块，返回新值与替换计数。
func stripToolResultMedia(value any, media *[]any, depth int) (any, int) {
	if depth > 32 {
		return value, 0
	}
	switch c := value.(type) {
	case string:
		if part, ok := anthropicToolResultStringMedia(c); ok {
			*media = append(*media, part)
			return anthropicToolMediaMovedMarker, 1
		}
		trimmed := strings.TrimSpace(c)
		if trimmed == "" {
			return value, 0
		}
		var parsed any
		if json.Unmarshal([]byte(trimmed), &parsed) != nil {
			return value, 0
		}
		newParsed, n := stripToolResultMedia(parsed, media, depth+1)
		if n == 0 {
			return value, 0
		}
		// 解析出的结构里的残余 base64 也要裁剪（参考 clamp_parsed_strings）。
		newParsed = clampToolBase64ish(newParsed)
		raw, err := json.Marshal(newParsed)
		if err != nil {
			return value, 0
		}
		return string(raw), n
	case []any:
		total := 0
		for i := range c {
			nv, n := stripToolResultMedia(c[i], media, depth+1)
			if n > 0 {
				c[i] = nv
			}
			total += n
		}
		return c, total
	case map[string]any:
		if part, ok := anthropicToolResultMediaPart(c); ok {
			*media = append(*media, part)
			return map[string]any{"type": "text", "text": anthropicToolMediaMovedMarker}, 1
		}
		if content, ok := c["content"]; ok {
			nv, n := stripToolResultMedia(content, media, depth+1)
			if n > 0 {
				c["content"] = nv
			}
			return c, n
		}
		return c, 0
	default:
		return value, 0
	}
}

// anthropicToolResultMediaPart 把 tool_result 里的媒体块转成 Chat user content part。
func anthropicToolResultMediaPart(block map[string]any) (map[string]any, bool) {
	if stringField(block, "type") != "image" {
		return nil, false
	}
	return anthropicImageToChatPart(block)
}

// anthropicToolResultStringMedia 判定整段字符串本身是否图片 data URL
// （与参考 whole_string_image_data_url 同口径：data:image/...;base64 且 ≥8KB）。
func anthropicToolResultStringMedia(value string) (map[string]any, bool) {
	trimmed := strings.TrimSpace(value)
	if len(trimmed) < 8*1024 {
		return nil, false
	}
	idx := strings.IndexByte(trimmed, ',')
	if idx < 0 {
		return nil, false
	}
	header := strings.ToLower(trimmed[:idx])
	if !strings.HasPrefix(header, "data:image/") || !strings.HasSuffix(header, ";base64") {
		return nil, false
	}
	return map[string]any{"type": "image_url", "image_url": map[string]any{"url": trimmed}}, true
}

// clampToolBase64ish 媒体搬走后把残余的长 data:/base64 串替换成字节数标记。
func clampToolBase64ish(value any) any {
	switch v := value.(type) {
	case string:
		trimmed := strings.TrimSpace(v)
		looksData := len(trimmed) >= 8*1024 && strings.HasPrefix(strings.ToLower(trimmed), "data:")
		if looksData || looksLikeBase64Text(trimmed) {
			return fmt.Sprintf("[omitted %d bytes]", len(v))
		}
		return v
	case []any:
		for i := range v {
			v[i] = clampToolBase64ish(v[i])
		}
		return v
	case map[string]any:
		for k := range v {
			v[k] = clampToolBase64ish(v[k])
		}
		return v
	default:
		return value
	}
}

// looksLikeBase64Text 判定长串是否全部由 base64 字符组成。
func looksLikeBase64Text(value string) bool {
	if len(value) < 16*1024 {
		return false
	}
	for i := 0; i < len(value); i++ {
		c := value[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9',
			c == '+', c == '/', c == '=':
		default:
			return false
		}
	}
	return true
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
//
// 判定顺序对齐参考 tool_media.rs（不依赖 source.type）：
//  1. source.url；
//  2. source.data（base64；media_type/mime_type/mimeType 兜底 image/png，
//     已是 data:image/ 串则原样用）；
//  3. source 缺失时接受顶格 data + mimeType（Gemini 形态）。
//
// detail 合并进 image_url；OpenAI 兼容网关只接受 auto/low/high，Codex 风格的
// "original" 降级为 "auto"。
func anthropicImageToChatPart(block map[string]any) (map[string]any, bool) {
	var url string
	if source, ok := block["source"].(map[string]any); ok {
		if !sourceMediaTypeIsImage(source) {
			return nil, false
		}
		switch {
		case stringField(source, "url") != "":
			url = stringField(source, "url")
		case rawString(source, "data") != "":
			data := rawString(source, "data")
			mediaType := firstNonEmpty(stringField(source, "media_type"),
				stringField(source, "mime_type"), stringField(source, "mimeType"))
			if strings.HasPrefix(strings.ToLower(data), "data:image/") {
				url = data
			} else {
				if mediaType == "" {
					mediaType = "image/png"
				}
				url = "data:" + mediaType + ";base64," + data
			}
		default:
			return nil, false
		}
	} else if data := rawString(block, "data"); data != "" {
		mediaType := firstNonEmpty(stringField(block, "mimeType"), stringField(block, "mime_type"))
		if !strings.HasPrefix(strings.ToLower(mediaType), "image/") {
			return nil, false
		}
		url = "data:" + mediaType + ";base64," + data
	} else {
		return nil, false
	}
	imageURL := map[string]any{"url": url}
	if detail, exists := block["detail"]; exists {
		if s, ok := detail.(string); ok && s == "original" {
			detail = "auto"
		}
		imageURL["detail"] = detail
	}
	return map[string]any{"type": "image_url", "image_url": imageURL}, true
}

// sourceMediaTypeIsImage 判定 source 声明的媒体类型是否为图片（缺失视为图片）。
func sourceMediaTypeIsImage(source map[string]any) bool {
	mediaType := firstNonEmpty(stringField(source, "media_type"),
		stringField(source, "mime_type"), stringField(source, "mimeType"))
	return mediaType == "" || strings.HasPrefix(strings.ToLower(mediaType), "image/")
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
	// 参考 transform.rs 的 reasoning_parts.join("\n")。
	return existing + "\n" + add
}

// applyThinking 把 Anthropic thinking / output_config 映射到 Chat reasoning_effort。
//
// 对齐参考 resolve_requested_reasoning_effort + effort_from_thinking：
//  1. 优先 output_config.effort（low/medium/high/xhigh 直映；max 归 xhigh；
//     thinking 关闭时一律降到 low）；
//  2. 其次 thinking：adaptive → xhigh；enabled 按 budget_tokens 分档
//     (<4000 low / <16000 medium / 其余 high，无 budget 视为 high)；
//  3. Claude 5.x 客户端（Fable/Mythos/Opus/Sonnet 5）省略 thinking 时按 adaptive
//     默认处理（参考 make_default_adaptive_thinking_explicit）。
//
// 最后按目标模型收敛档位（gpt-5-pro 只接受 high）。
func applyThinking(out, req map[string]any) {
	model, _ := req["model"].(string)
	effort := requestedReasoningEffort(req, model)
	if effort == "" {
		return
	}
	out["reasoning_effort"] = clampReasoningEffort(model, effort)
}

func requestedReasoningEffort(req map[string]any, model string) string {
	thinking, _ := req["thinking"].(map[string]any)
	thinkingType := stringField(thinking, "type")
	if oc, ok := req["output_config"].(map[string]any); ok {
		if raw := stringField(oc, "effort"); raw != "" {
			mapped := map[string]string{
				"low": "low", "medium": "medium", "high": "high",
				"xhigh": "xhigh", "max": "xhigh",
			}[raw]
			if mapped == "" {
				return ""
			}
			if thinkingType == "" || thinkingType == "disabled" {
				return "low"
			}
			return mapped
		}
	}
	switch thinkingType {
	case "adaptive":
		return "xhigh"
	case "enabled":
		budget := numToU64(thinking["budget_tokens"])
		switch {
		case budget == 0:
			return "high"
		case budget < 4000:
			return "low"
		case budget < 16000:
			return "medium"
		default:
			return "high"
		}
	}
	// thinking 缺失/null：Claude 5.x 客户端默认开启 adaptive（省略即默认开）。
	if thinking == nil && adaptiveThinkingIsDefault(model) {
		return "xhigh"
	}
	return ""
}

// adaptiveThinkingIsDefault 判定客户端模型是否 5.x 默认 adaptive（对齐参考名单）。
func adaptiveThinkingIsDefault(model string) bool {
	normalized := strings.ToLower(strings.TrimSpace(model))
	normalized = strings.NewReplacer(".", "-", "_", "-").Replace(normalized)
	for _, needle := range []string{"fable-5", "mythos-5", "mythos-preview", "opus-5", "sonnet-5"} {
		if strings.Contains(normalized, needle) {
			return true
		}
	}
	return false
}

// clampReasoningEffort 按目标模型收敛档位（gpt-5-pro 只接受 high；*-pro 不接受 low）。
func clampReasoningEffort(model, effort string) string {
	normalized := strings.ToLower(strings.TrimSpace(model))
	if normalized == "gpt-5-pro" || strings.HasPrefix(normalized, "gpt-5-pro-") {
		return "high"
	}
	if strings.HasPrefix(normalized, "gpt-") &&
		(strings.HasSuffix(normalized, "-pro") || strings.Contains(normalized, "-pro-")) &&
		effort == "low" {
		return "medium"
	}
	return effort
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
	cleanSchema(out)
	return out
}

// cleanSchema 递归删除 OpenAI 不认识的 `format:"uri"`（严格上游会整单 400，
// 对齐参考 clean_schema），并下探 properties / items。
func cleanSchema(schema map[string]any) {
	if schema == nil {
		return
	}
	if f, ok := schema["format"].(string); ok && f == "uri" {
		delete(schema, "format")
	}
	if props, ok := schema["properties"].(map[string]any); ok {
		for _, v := range props {
			if child, ok := v.(map[string]any); ok {
				cleanSchema(child)
			}
		}
	}
	if items, ok := schema["items"].(map[string]any); ok {
		cleanSchema(items)
	}
}

// toolChoiceToChat 把 Anthropic tool_choice 转成 Chat 形态（蓝本 transform.rs map_tool_choice_to_chat）。
//
//	{"type":"auto"}  → "auto"
//	{"type":"any"}   → "required"
//	{"type":"none"}  → "none"（本网关 payload.go 会同时删 tools——语义正确）
//	{"type":"tool","name":N} → {"type":"function","function":{"name":N}}
func toolChoiceToChat(v any) any {
	// 字符串形态：`"any"` → `"required"`，其余原样（对齐参考 map_tool_choice_to_chat）。
	if s, ok := v.(string); ok {
		if s == "any" {
			return "required"
		}
		return s
	}
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
		// 未知形态原样保留（对齐参考 map_tool_choice_to_chat），不擅自改写。
		return v
	}
}
