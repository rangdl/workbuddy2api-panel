// Package anthropic 实现 Anthropic Messages API（/v1/messages）与 Chat Completions
// 的双向协议转换。
//
// 本包是纯转换层：不依赖 HTTP、不持有跨请求状态，输入输出均为 []byte / map[string]any，
// 便于表驱动单测。上游 CodeBuddy / 第三方只提供 Chat Completions，而 Claude Code 等
// 客户端只讲 Anthropic Messages，转换在网关边界完成。
//
// 蓝本：cc-switch 的 transform.rs（anthropic_to_openai / openai_to_anthropic）与
// streaming.rs（create_anthropic_sse_stream）。映射规则见 docs/anthropic-messages-plan.md。
//
// 与 internal/responses（Codex 适配）同构：两包共享的候选（ToolContext / 参数规范化 /
// SSE 解析）在两协议各自稳定后再抽公共包（见方案 §10），第一阶段独立演进。
package anthropic

import (
	"encoding/json"
	"strings"
)

// stringField 取字符串字段并 TrimSpace（用于 type/role/name 等枚举/标识）。
func stringField(m map[string]any, key string) string {
	s, _ := m[key].(string)
	return strings.TrimSpace(s)
}

// rawString 取字符串字段但不 TrimSpace（用于 text/content 等正文，保留原始空白）。
func rawString(m map[string]any, key string) string {
	s, _ := m[key].(string)
	return s
}

// strVal 把任意值转为字符串（非字符串返回空串）。
func strVal(v any) string {
	s, _ := v.(string)
	return s
}

// firstNonEmpty 返回第一个非空字符串。
func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// numToU64 把 JSON 数字（float64）转为 uint64（失败/负数返回 0）。
// Anthropic 的 usage 字段全为非负整数。
func numToU64(v any) uint64 {
	switch n := v.(type) {
	case float64:
		if n <= 0 {
			return 0
		}
		return uint64(n)
	case uint64:
		return n
	case int64:
		if n <= 0 {
			return 0
		}
		return uint64(n)
	case int:
		if n <= 0 {
			return 0
		}
		return uint64(n)
	default:
		return 0
	}
}

// canonicalizeToolArguments 把 tool arguments 规范化为字符串（与 responses 包同口径）：
// 缺失/空 → "{}"；对象按键排序序列化（Go map 序列化天然键序稳定，护 prompt cache）；
// 字符串若是 JSON 则解析后重序列化规范化；其他值 JSON 序列化。
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

// canonicalJSONStringOf 把任意值序列化为键序稳定的 JSON 字符串（tool_use.input →
// Chat arguments 用）；失败返回 "{}"。
func canonicalJSONStringOf(v any) string {
	raw, err := json.Marshal(v)
	if err != nil {
		return "{}"
	}
	return string(raw)
}
