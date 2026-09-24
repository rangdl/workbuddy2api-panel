// Package responses 实现 OpenAI Responses API（/v1/responses）与 Chat Completions
// 的双向协议转换。
//
// 本包是纯转换层：不依赖 HTTP、不持有跨请求状态，输入输出均为 []byte / map[string]any，
// 便于表驱动单测。上游 CodeBuddy 只提供 Chat Completions，而 Codex 等客户端只讲
// Responses，转换在网关边界完成（蓝本：cc-switch 的 transform_codex_chat.rs）。
package responses

import (
	"encoding/json"
	"strings"
)

// Responses 工具类型。
const (
	toolKindFunction   = "function"
	toolKindCustom     = "custom"
	toolKindToolSearch = "tool_search"
	toolKindNamespace  = "namespace"
)

// customToolInputField custom 工具包成 Chat function 时的入参字段名。
const customToolInputField = "input"

// toolSearchProxyName tool_search 工具在 Chat 侧的代理函数名。
const toolSearchProxyName = "tool_search"

// chatToolNameMaxLen Chat 工具名长度上限（部分严格上游限制 64）。
const chatToolNameMaxLen = 64

// ToolSpec 记录一个 Responses 工具到 Chat 工具的映射，供回程把 Chat 的
// function_call 还原为 Responses 的 function_call / custom_tool_call / tool_search_call。
type ToolSpec struct {
	Kind      string // function / custom / tool_search / namespace
	Name      string // Responses 侧原始工具名
	Namespace string // namespace 工具所属命名空间（空 = 无）
}

// ToolContext 承载一次请求内 Responses 工具 ↔ Chat 工具的映射关系。
// 每个请求独立构造、不跨请求共享：映射表从当前请求的 tools 声明重建
// （增量补全场景下缓存的是 Chat 侧 call item，按原样回填）。
type ToolContext struct {
	chatTools      []any
	chatNameToSpec map[string]ToolSpec
	seenChatNames  map[string]bool
	// nsNameToChat (namespace\x00name) → 扁平化后的 Chat 工具名，供回程精确还原。
	nsNameToChat map[string]string
}

// NewToolContext 构造空工具上下文（导出供 server 层与测试使用）。
func NewToolContext() *ToolContext {
	return &ToolContext{
		chatNameToSpec: map[string]ToolSpec{},
		seenChatNames:  map[string]bool{},
		nsNameToChat:   map[string]string{},
	}
}

// ChatTools 返回转换后的 Chat 工具数组（可能为空）。
func (c *ToolContext) ChatTools() []any { return c.chatTools }

// Lookup 按 Chat 工具名反查 Responses 工具规格。
func (c *ToolContext) Lookup(chatName string) (ToolSpec, bool) {
	spec, ok := c.chatNameToSpec[chatName]
	return spec, ok
}

// isCustom 报告 Chat 工具名是否对应 custom 工具。
func (c *ToolContext) isCustom(chatName string) bool {
	spec, ok := c.chatNameToSpec[chatName]
	return ok && spec.Kind == toolKindCustom
}

// chatNameForResponseFunction 由 Responses 的 (namespace, name) 求 Chat 侧工具名。
func (c *ToolContext) chatNameForResponseFunction(name, namespace string) string {
	if namespace != "" {
		if chatName, ok := c.nsNameToChat[namespace+"\x00"+name]; ok {
			return chatName
		}
		return flattenNamespaceToolName(namespace, name)
	}
	return name
}

// add 登记 Chat 工具及映射（重名跳过，与 cc-switch 的 seen_chat_names 语义一致）。
func (c *ToolContext) add(chatName string, spec ToolSpec, chatTool any) {
	chatName = strings.TrimSpace(chatName)
	if chatName == "" || c.seenChatNames[chatName] {
		return
	}
	c.seenChatNames[chatName] = true
	c.chatNameToSpec[chatName] = spec
	if spec.Namespace != "" {
		c.nsNameToChat[spec.Namespace+"\x00"+spec.Name] = chatName
	}
	c.chatTools = append(c.chatTools, chatTool)
}

// ---- 通用小工具 ----

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

// numToInt 把 JSON 数字（float64）转为 int64（失败返回 0）。
func numToInt(v any) int64 {
	switch n := v.(type) {
	case float64:
		return int64(n)
	case int64:
		return n
	case int:
		return int64(n)
	case json.Number:
		i, _ := n.Int64()
		return i
	default:
		return 0
	}
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
