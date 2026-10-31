package responses

import (
	"encoding/json"
	"strings"
)

// isDeepSeekChatModel 判定 bare 模型名是否 DeepSeek 系（对齐参考实现
// transform_codex_chat.rs is_deepseek_chat_model）：
// 去掉可能的 "cn:"/"global:" realm 前缀与 "provider/" 前缀后，精确等于
// "deepseek" 或以 "deepseek-" 开头。大小写不敏感。
func isDeepSeekChatModel(model string) bool {
	name := strings.TrimSpace(model)
	if i := strings.LastIndexByte(name, '/'); i >= 0 {
		name = name[i+1:]
	}
	if i := strings.IndexByte(name, ':'); i >= 0 {
		name = name[i+1:]
	}
	name = strings.ToLower(name)
	return name == "deepseek" || strings.HasPrefix(name, "deepseek-")
}

// coalesceReplayedToolTurnMessages 只对 DeepSeek 系模型启用：把「工具调用之后、
// 结果到达之前」回放的 assistant 消息（含空消息与 reasoning）合并回发起调用的
// 那条 assistant，直到本批调用全部拿到结果。
//
// 背景（对齐 cc-switch #8351）：Responses 客户端回放历史时，可能先放
// function_call，再把 assistant 文本/思考作为独立 message 插进来，最后才给
// function_call_output；同一批并行调用也可能被拆到多条 assistant 上。出站
// Chat 序列于是变成 assistant(tool_calls) → assistant(text) → tool → …，
// DeepSeek 严格校验「tool 结果必须紧跟发起调用的 assistant」，直接 400。
// 合并后 wire 恒为一条 assistant(tool_calls，含回放正文) 后接连续 tool。
//
// 为什么限定 DeepSeek：其他上游对 assistant 回合的解释不同，且改写历史会破坏
// 它们的 prompt cache（参考实现同样只在 DeepSeek 上做该重写）。
func coalesceReplayedToolTurnMessages(messages []any) []any {
	out := make([]any, 0, len(messages))
	openTurn := -1 // out 中发起本批 tool_calls 的 assistant 下标
	pending := map[string]bool{}
	for _, raw := range messages {
		msg, ok := raw.(map[string]any)
		if !ok {
			openTurn, pending = -1, map[string]bool{}
			out = append(out, raw)
			continue
		}
		switch msg["role"] {
		case "assistant":
			calls, _ := msg["tool_calls"].([]any)
			for _, ci := range calls {
				if call, ok := ci.(map[string]any); ok {
					if id, _ := call["id"].(string); id != "" {
						pending[id] = true
					}
				}
			}
			if openTurn >= 0 {
				mergeIntoOpenToolTurn(out[openTurn].(map[string]any), msg)
				continue
			}
			if len(pending) > 0 {
				openTurn = len(out)
			}
		case "tool":
			if id, _ := msg["tool_call_id"].(string); id != "" {
				delete(pending, id)
			}
			if len(pending) == 0 {
				openTurn = -1
			}
		default:
			openTurn, pending = -1, map[string]bool{}
		}
		out = append(out, msg)
	}
	if len(out) == len(messages) {
		// 合并必然减少消息条数；条数不变 = 零改动，返回原 slice 保持上层语义。
		return messages
	}
	return out
}

// mergeIntoOpenToolTurn 把回放的 assistant 合并进发起调用的那条：正文与
// reasoning_content 追加合并，tool_calls 按原序拼接。
func mergeIntoOpenToolTurn(target, incoming map[string]any) {
	if content, exists := incoming["content"]; exists {
		target["content"] = mergeReplayContent(target["content"], content)
	}
	// reasoning_content 优先；兼容历史字段 reasoning。
	reasoning, _ := incoming["reasoning_content"].(string)
	if reasoning == "" {
		reasoning, _ = incoming["reasoning"].(string)
	}
	if reasoning != "" {
		target["reasoning_content"] = joinReasoningUnique(rawString(target, "reasoning_content"), reasoning)
	}
	if calls, ok := incoming["tool_calls"].([]any); ok && len(calls) > 0 {
		existing, _ := target["tool_calls"].([]any)
		target["tool_calls"] = append(existing, calls...)
	}
}

// mergeReplayContent 合并回放正文：空内容不覆盖；同为空时取后者；两个字符串
// 用换行拼接；其余（含内容块数组）统一转成 text 块数组，保持结构不丢。
func mergeReplayContent(previous, incoming any) any {
	if replayContentEmpty(incoming) {
		return previous
	}
	if replayContentEmpty(previous) {
		return incoming
	}
	prevStr, prevIsStr := previous.(string)
	inStr, inIsStr := incoming.(string)
	if prevIsStr && inIsStr {
		return prevStr + "\n" + inStr
	}
	parts := replayContentParts(previous)
	parts = append(parts, replayContentParts(incoming)...)
	return parts
}

func replayContentEmpty(v any) bool {
	switch c := v.(type) {
	case nil:
		return true
	case string:
		return c == ""
	case []any:
		return len(c) == 0
	default:
		return false
	}
}

func replayContentParts(v any) []any {
	switch c := v.(type) {
	case []any:
		return append([]any{}, c...)
	case string:
		return []any{map[string]any{"type": "text", "text": c}}
	case nil:
		return nil
	default:
		return []any{c}
	}
}

// ApplyUpstreamModelFixes 在「客户端模型名 → 上游模型名」映射之后，按**最终**
// 上游模型补跑依赖模型身份的修正。
//
// 为什么需要：ToChat 执行时请求体里还是客户端模型名（Codex 发 gpt-5.5），
// 映射到 cn:deepseek-v4.1-flash 发生在转换之后；只在 ToChat 内判定 DeepSeek
// 会漏掉绝大多数线上请求。本函数幂等，可在映射后安全补跑。
func ApplyUpstreamModelFixes(body []byte) []byte {
	var req map[string]any
	if json.Unmarshal(body, &req) != nil {
		return body
	}
	model, _ := req["model"].(string)
	if !isDeepSeekChatModel(model) {
		return body
	}
	msgs, ok := req["messages"].([]any)
	if !ok || len(msgs) < 2 {
		return body
	}
	fixed := coalesceReplayedToolTurnMessages(msgs)
	if len(fixed) == len(msgs) {
		return body
	}
	req["messages"] = fixed
	out, err := json.Marshal(req)
	if err != nil {
		return body
	}
	return out
}
