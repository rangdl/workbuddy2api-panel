package responses

import (
	"encoding/json"
	"strings"
)

// preserveInvalidToolCallsAsText 把「arguments 不是合法 JSON 对象」的历史工具调用
// 降级为纯文本上下文，并把它的配对结果折回同一条 assistant 消息。
//
// 背景（对齐参考实现 cc-switch codex_chat_tool_history.rs / #8241）：
// 工具执行失败时客户端会把调用持久化进会话历史，arguments 可能是半截 JSON
// （流被截断）或数组/标量。严格 Chat 上游对这类条目直接 400，而 Codex 每次
// 重试都会原样重放同一段历史——整条会话报废，池侧换号也无效（不是账号问题）。
// 网关是最后一道防线：把坏调用降级为「历史上下文」文本（保留 name/call_id/
// arguments 供模型理解），并删掉对应的 tool 结果，宁可丢一轮工具上下文。
//
// 判定口径与参考实现一致：arguments 为对象、或字符串能解析为对象才合法；
// 缺失/空串（转换层已归一为 "{}"）/无法解析/解析为非对象均判非法。
//
// 返回恢复（降级）的调用条数；无非法条目时原 slice 原样返回。
func preserveInvalidToolCallsAsText(messages []any) ([]any, int) {
	invalidOwners := map[string]int{} // call id → 所属 assistant 在 out 中的下标
	out := make([]any, 0, len(messages))
	recovered := 0
	for _, raw := range messages {
		msg, ok := raw.(map[string]any)
		if !ok {
			out = append(out, raw)
			continue
		}
		switch msg["role"] {
		case "assistant":
			calls, _ := msg["tool_calls"].([]any)
			if len(calls) > 0 {
				kept := make([]any, 0, len(calls))
				var notes []string
				for _, ci := range calls {
					call, ok := ci.(map[string]any)
					if !ok {
						continue
					}
					id, _ := call["id"].(string)
					// 后续合法调用可能复用同一个 id：它自己的结果不能被更早的
					// 坏调用吞掉，先解除归属再判定。
					if id != "" {
						delete(invalidOwners, id)
					}
					if toolArgumentsAreObject(call) {
						kept = append(kept, ci)
						continue
					}
					if id != "" {
						invalidOwners[id] = len(out)
					}
					notes = append(notes, invalidToolCallNote(call, id))
					recovered++
				}
				if len(kept) == 0 {
					delete(msg, "tool_calls")
				} else {
					msg["tool_calls"] = kept
				}
				if len(notes) > 0 {
					msg["content"] = appendContextText(msg["content"], notes)
				}
			}
		case "tool":
			if id, _ := msg["tool_call_id"].(string); id != "" {
				if owner, ok := invalidOwners[id]; ok {
					target, _ := out[owner].(map[string]any)
					if target != nil {
						target["content"] = appendContextText(target["content"], []string{
							"[Historical tool result for invalid call " + id + "; context only]\n" +
								contextText(msg["content"]),
						})
					}
					delete(invalidOwners, id)
					continue
				}
			}
		}
		out = append(out, msg)
	}
	if recovered == 0 {
		return messages, 0
	}
	return out, recovered
}

// toolArgumentsAreObject 判定 Chat tool_call 的 arguments 是否为合法参数包：
// 对象本身，或可解析为对象的字符串。nil/其他类型一律非法。
func toolArgumentsAreObject(call map[string]any) bool {
	fn, _ := call["function"].(map[string]any)
	if fn == nil {
		return false
	}
	args, exists := fn["arguments"]
	if !exists {
		return false
	}
	switch v := args.(type) {
	case map[string]any:
		return true
	case string:
		trimmed := strings.TrimSpace(v)
		if trimmed == "" {
			return false
		}
		var parsed any
		if json.Unmarshal([]byte(trimmed), &parsed) != nil {
			return false
		}
		_, isObject := parsed.(map[string]any)
		return isObject
	default:
		return false
	}
}

// invalidToolCallNote 生成降级文本（模型据此理解「这里曾有一次失败的工具调用」）。
func invalidToolCallNote(call map[string]any, id string) string {
	fn, _ := call["function"].(map[string]any)
	name, _ := fn["name"].(string)
	if name == "" {
		name = "<unknown>"
	}
	callID := id
	if callID == "" {
		callID = "<missing>"
	}
	return "[Historical tool call with invalid arguments; context only, not a new tool invocation]\n" +
		"name: " + name + "\ncall_id: " + callID + "\narguments: " + contextText(fn["arguments"])
}

// contextText 把任意值转成适合嵌入文本上下文的字符串。
func contextText(v any) string {
	switch c := v.(type) {
	case nil:
		return "<missing>"
	case string:
		return c
	default:
		raw, err := json.Marshal(c)
		if err != nil {
			return "<unserializable>"
		}
		return string(raw)
	}
}

// appendContextText 把若干段文本追加到消息 content：字符串用空行分隔拼接；
// 内容块数组追加 text 块；其他（null/缺失）直接落成字符串。
func appendContextText(content any, texts []string) any {
	switch c := content.(type) {
	case string:
		if strings.TrimSpace(c) == "" {
			return strings.Join(texts, "\n\n")
		}
		return c + "\n\n" + strings.Join(texts, "\n\n")
	case []any:
		parts := make([]any, 0, len(c)+len(texts))
		parts = append(parts, c...)
		for _, t := range texts {
			parts = append(parts, map[string]any{"type": "text", "text": t})
		}
		return parts
	default:
		return strings.Join(texts, "\n\n")
	}
}

// orphanToolOutputMarker 无配对调用的工具结果降级文案（对齐参考
// ORPHAN_TOOL_OUTPUT_MARKER）。
const orphanToolOutputMarker = "[tool_result without a matching tool_call]"

// orphanToolOutputMessage 把孤儿工具结果降级成 user 消息：文本 body 内联
// 「媒体已挂在本条消息」占位，媒体本体作为同一条消息的 content parts 跟随
// （避免 base64 被序列化成纯文本既看不到图又撑爆 token）。
func orphanToolOutputMessage(item map[string]any) map[string]any {
	callID := responseItemCallID(item)
	body, media := planToolOutputMediaWithMarker(item["output"], toolMediaAttachedMarker)
	marker := orphanToolOutputMarker
	if callID != "" {
		marker += " (call_id: " + callID + ")"
	}
	text := marker
	if body != "" {
		text += "\n" + body
	}
	if len(media) == 0 {
		return map[string]any{"role": "user", "content": text}
	}
	parts := make([]any, 0, len(media)+1)
	parts = append(parts, map[string]any{"type": "text", "text": text})
	parts = append(parts, media...)
	return map[string]any{"role": "user", "content": parts}
}
