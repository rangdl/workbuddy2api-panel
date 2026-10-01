package anthropic

import (
	"encoding/json"
	"fmt"
	"strings"
)

// FromChat 把 Chat Completions 响应（upstream.Aggregate 结果）转换为 Anthropic message。
//
// 蓝本：cc-switch transform.rs openai_to_anthropic（533-726 行）。
func FromChat(resp map[string]any) (map[string]any, error) {
	choices, _ := resp["choices"].([]any)
	if len(choices) == 0 {
		return nil, fmt.Errorf("no choices in chat response")
	}
	choice, _ := choices[0].(map[string]any)
	message, _ := choice["message"].(map[string]any)
	if message == nil {
		return nil, fmt.Errorf("no message in chat choice")
	}

	finishReason := stringField(choice, "finish_reason")

	// content blocks：thinking（若有）→ text（若有）→ tool_use（每个一次）。
	content := []any{}

	// reasoning_content → thinking 块（DeepSeek 系上游把思维链放这个字段）。
	if rc := rawString(message, "reasoning_content"); rc != "" {
		content = append(content, map[string]any{"type": "thinking", "thinking": rc})
	} else if content != nil {
		if think, _, ok := splitLeadingThinkBlock(rawString(message, "content")); ok && think != "" {
			content = append(content, map[string]any{"type": "thinking", "thinking": think})
		}
	}

	// 正文：字符串（剥前置 think 块）或多模态数组。
	texts := []any{}
	if text := rawString(message, "content"); text != "" {
		if _, answer, ok := splitLeadingThinkBlock(text); ok {
			text = answer
		}
		if text != "" {
			texts = append(texts, map[string]any{"type": "text", "text": text})
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
					texts = append(texts, map[string]any{"type": "text", "text": t})
				}
			case "refusal":
				if t := rawString(part, "refusal"); t != "" {
					texts = append(texts, map[string]any{"type": "text", "text": t})
				}
			}
		}
	}
	// message 级 refusal（部分上游放外层）。
	if refusal := rawString(message, "refusal"); refusal != "" {
		texts = append(texts, map[string]any{"type": "text", "text": refusal})
	}
	content = append(content, texts...)

	// tool_calls → tool_use 块（input 必须反序列化回对象；解析失败回退 {_raw:...} 保真）。
	if tcs, ok := message["tool_calls"].([]any); ok {
		for _, raw := range tcs {
			tc, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			fn, _ := tc["function"].(map[string]any)
			name := stringField(fn, "name")
			if name == "" {
				continue // 与 responses 侧同口径：缺名调用丢弃（畸形上游防御）
			}
			content = append(content, map[string]any{
				"type":  "tool_use",
				"id":    firstNonEmpty(stringField(tc, "id"), "toolu_wb2api"),
				"name":  name,
				"input": parseToolInput(fn["arguments"]),
			})
		}
	}

	return map[string]any{
		"id":            messageIDOf(rawString(resp, "id")),
		"type":          "message",
		"role":          "assistant",
		"model":         rawString(resp, "model"),
		"content":       content,
		"stop_reason":   stopReasonOf(finishReason, hasToolUse(message)),
		"stop_sequence": nil,
		"usage":         usageOf(resp["usage"]),
	}, nil
}

// messageIDOf 把 Chat id 规范化为 Anthropic 的 msg_ 前缀形态。
func messageIDOf(id string) string {
	if id == "" {
		return "msg_wb2api"
	}
	if strings.HasPrefix(id, "msg_") {
		return id
	}
	return "msg_" + id
}

// hasToolUse 报告 Chat message 是否带 tool_calls（含旧式 function_call）。
func hasToolUse(message map[string]any) bool {
	if tcs, ok := message["tool_calls"].([]any); ok && len(tcs) > 0 {
		return true
	}
	_, legacy := message["function_call"]
	return legacy
}

// stopReasonOf 映射 finish_reason → stop_reason（蓝本 transform.rs:648-664）。
// 缺失但有 tool_use 时强制 tool_use。
func stopReasonOf(finishReason string, hasTools bool) string {
	switch finishReason {
	case "length":
		return "max_tokens"
	case "tool_calls", "function_call":
		return "tool_use"
	case "stop", "content_filter", "":
		if hasTools {
			return "tool_use"
		}
		return "end_turn"
	default:
		// 未知值按蓝本 WARN+end_turn 处理；这里交给调用方日志（纯函数无 logger）。
		if hasTools {
			return "tool_use"
		}
		return "end_turn"
	}
}

// parseToolInput 把 Chat arguments 字符串反序列化为对象（Anthropic tool_use.input 是对象）。
// 解析失败时回退 {"_raw": 原字符串} 保真（模型吐坏 JSON 时客户端至少能看到原文）。
func parseToolInput(arguments any) map[string]any {
	switch a := arguments.(type) {
	case string:
		var parsed map[string]any
		if json.Unmarshal([]byte(a), &parsed) == nil && parsed != nil {
			return parsed
		}
		return map[string]any{"_raw": a}
	case map[string]any:
		return a
	default:
		return map[string]any{}
	}
}

// splitLeadingThinkBlock 从 content 前置 <think>...</think> 块拆出思维链与正文
// （与 responses 包 reasoning.go 同口径；DeepSeek 系上游常把思维链内嵌正文）。
func splitLeadingThinkBlock(content string) (think, rest string, ok bool) {
	trimmed := strings.TrimSpace(content)
	if !strings.HasPrefix(trimmed, "<think>") {
		return "", "", false
	}
	end := strings.Index(trimmed, "</think>")
	if end < 0 {
		return "", "", false
	}
	think = strings.TrimSpace(trimmed[len("<think>"):end])
	rest = strings.TrimSpace(trimmed[end+len("</think>"):])
	return think, rest, true
}

// usageOf 把 OpenAI usage 映射为 Anthropic usage（蓝本 transform.rs:667-709）。
//
// 关键账务语义：OpenAI prompt_tokens 是**含缓存命中**的 inclusive 口径，Anthropic 的
// input_tokens 不含——必须做减法 input = prompt − cache_read − cache_creation，
// 否则缓存 token 在 input 与 cache 桶双计（上游计费对不上账）。
// 为 0 的可选桶**省略字段**（Anthropic 规范）。
func usageOf(u any) map[string]any {
	obj, ok := u.(map[string]any)
	if !ok {
		return map[string]any{"input_tokens": 0, "output_tokens": 0}
	}
	prompt := numToU64(obj["prompt_tokens"])
	completion := numToU64(obj["completion_tokens"])

	cacheRead := nestedU64(obj, "prompt_tokens_details", "cached_tokens")
	cacheWrite := nestedU64(obj, "prompt_tokens_details", "cache_write_tokens")

	input := prompt
	if cacheRead > 0 || cacheWrite > 0 {
		input = subSaturate(subSaturate(prompt, cacheRead), cacheWrite)
	}

	out := map[string]any{"input_tokens": input, "output_tokens": completion}
	if cacheRead > 0 {
		out["cache_read_input_tokens"] = cacheRead
	}
	if cacheWrite > 0 {
		out["cache_creation_input_tokens"] = cacheWrite
	}
	return out
}

// nestedU64 取 obj[outer][inner] 的非负数值（缺失/类型不符返回 0）。
func nestedU64(obj map[string]any, outer, inner string) uint64 {
	nested, ok := obj[outer].(map[string]any)
	if !ok {
		return 0
	}
	return numToU64(nested[inner])
}

// subSaturate 无符号饱和减法（a ≥ b 时返回 a-b，否则 0）。
func subSaturate(a, b uint64) uint64 {
	if a < b {
		return 0
	}
	return a - b
}
