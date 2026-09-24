package responses

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
)

// buildToolContext 从 Responses 请求的 tools 声明构造 Chat 工具映射。
func buildToolContext(body map[string]any) *ToolContext {
	ctx := NewToolContext()
	tools, _ := body["tools"].([]any)
	for _, raw := range tools {
		switch tool := raw.(type) {
		case string:
			// 字符串工具名视作 custom 工具（cc-switch 同口径）。
			ctx.addCustom(map[string]any{"type": toolKindCustom, "name": tool})
		case map[string]any:
			// 仅识别可映射到 Chat 的工具类型。web_search / x_search /
			// image_generation 等服务端工具（Responses server-side tools）无法在
			// Chat 上游执行，**有意忽略**（对齐 cc-switch：落到 default 不转换、
			// 不报错）。Codex 发现该工具不可用后会自行降级；保留它会让严格上游 400。
			switch stringField(tool, "type") {
			case toolKindFunction:
				ctx.addFunction(tool, "")
			case toolKindCustom:
				ctx.addCustom(tool)
			case toolKindToolSearch:
				ctx.addToolSearch()
			case toolKindNamespace:
				ctx.addNamespace(tool)
			}
		}
	}
	return ctx
}

// addFunction 登记 function 工具（namespace 非空时扁平化命名）。
func (c *ToolContext) addFunction(tool map[string]any, namespace string) {
	name := responsesToolName(tool)
	if name == "" {
		return
	}
	chatName := name
	kind := toolKindFunction
	if namespace != "" {
		chatName = flattenNamespaceToolName(namespace, name)
		kind = toolKindNamespace
	}
	chatTool := responsesFunctionToolToChatTool(tool, chatName)
	if chatTool == nil {
		return
	}
	c.add(chatName, ToolSpec{Kind: kind, Name: name, Namespace: namespace}, chatTool)
}

// addCustom 登记 custom 工具：包成带 {input:string} 入参的 Chat function，
// 描述中嵌入原始工具定义，供回程还原。
func (c *ToolContext) addCustom(tool map[string]any) {
	name := responsesToolName(tool)
	if name == "" {
		return
	}
	chatTool := map[string]any{
		"type": "function",
		"function": map[string]any{
			"name":        name,
			"description": customToolDescription(tool),
			"parameters": map[string]any{
				"type": "object",
				"properties": map[string]any{
					customToolInputField: map[string]any{
						"type":        "string",
						"description": "Raw string input for the original custom tool. Preserve formatting exactly.",
					},
				},
				"required": []any{customToolInputField},
			},
		},
	}
	c.add(name, ToolSpec{Kind: toolKindCustom, Name: name}, chatTool)
}

// addToolSearch 登记 tool_search 代理工具。
func (c *ToolContext) addToolSearch() {
	chatTool := map[string]any{
		"type": "function",
		"function": map[string]any{
			"name":        toolSearchProxyName,
			"description": "Search and load Codex tools, plugins, connectors, and MCP namespaces for the current task.",
			"parameters": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"query": map[string]any{"type": "string", "description": "Search query."},
					"limit": map[string]any{"type": "integer", "description": "Max tool groups."},
				},
				"required": []any{"query"},
			},
		},
	}
	c.add(toolSearchProxyName, ToolSpec{Kind: toolKindToolSearch, Name: toolSearchProxyName}, chatTool)
}

// addNamespace 展开 namespace 工具的 function 子工具。
func (c *ToolContext) addNamespace(tool map[string]any) {
	namespace := stringField(tool, "name")
	if namespace == "" {
		return
	}
	children, _ := tool["tools"].([]any)
	if children == nil {
		children, _ = tool["children"].([]any)
	}
	for _, raw := range children {
		child, ok := raw.(map[string]any)
		if !ok || stringField(child, "type") != toolKindFunction {
			continue
		}
		c.addFunction(child, namespace)
	}
}

// responsesToolName 提取 Responses 工具名：优先 tool.function.name，其次 tool.name。
func responsesToolName(tool map[string]any) string {
	if fn, ok := tool["function"].(map[string]any); ok {
		if name := stringField(fn, "name"); name != "" {
			return name
		}
	}
	return stringField(tool, "name")
}

// responsesFunctionToolToChatTool 把 Responses function 工具转成 Chat function 工具。
// 兼容扁平（name/description/parameters 在顶层）与嵌套（在 function 对象内）两种形态。
func responsesFunctionToolToChatTool(tool map[string]any, chatName string) map[string]any {
	if stringField(tool, "type") != toolKindFunction {
		return nil
	}
	inner := map[string]any{}
	params := tool["parameters"]
	if nested, ok := tool["function"].(map[string]any); ok {
		for k, v := range nested {
			inner[k] = v
		}
		params = nested["parameters"]
	} else if desc, ok := tool["description"]; ok && desc != nil {
		inner["description"] = desc
	}
	inner["name"] = chatName
	inner["parameters"] = normalizeFunctionParameters(params)
	if strict, ok := tool["strict"]; ok {
		if _, exists := inner["strict"]; !exists {
			inner["strict"] = strict
		}
	}
	return map[string]any{"type": "function", "function": inner}
}

// normalizeFunctionParameters 保证 parameters.type 恒为 "object"
// （严格 OpenAI 兼容上游要求）。
func normalizeFunctionParameters(params any) map[string]any {
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

// flattenNamespaceToolName 把 namespace 子工具扁平化为 namespace__name；
// 超长时用短 hash 后缀（避免不同子工具截断后重名）。
func flattenNamespaceToolName(namespace, name string) string {
	full := namespace + "__" + name
	if len(full) <= chatToolNameMaxLen {
		return full
	}
	suffix := "__" + shortHash(full)
	prefixLen := chatToolNameMaxLen - len(suffix)
	if prefixLen < 0 {
		prefixLen = 0
	}
	return full[:prefixLen] + suffix
}

// shortHash 返回字符串 sha256 的前 8 位 hex。
func shortHash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])[:8]
}

// customToolDescription 生成嵌入原始工具定义的描述文本。
func customToolDescription(tool map[string]any) string {
	raw, err := json.Marshal(tool)
	if err != nil {
		raw = []byte("{}")
	}
	return "Original tool definition:\n```json\n" + string(raw) + "\n```"
}

// responsesToolChoiceToChat 把 Responses 的 tool_choice 转成 Chat 形态。
func responsesToolChoiceToChat(toolChoice any, ctx *ToolContext) any {
	obj, ok := toolChoice.(map[string]any)
	if !ok {
		return toolChoice
	}
	switch stringField(obj, "type") {
	case toolKindFunction:
		name := stringField(obj, "name")
		chatName := ctx.chatNameForResponseFunction(name, stringField(obj, "namespace"))
		return map[string]any{"type": "function", "function": map[string]any{"name": chatName}}
	case toolKindToolSearch:
		return map[string]any{"type": "function", "function": map[string]any{"name": toolSearchProxyName}}
	case toolKindCustom:
		return map[string]any{"type": "function", "function": map[string]any{"name": stringField(obj, "name")}}
	default:
		return toolChoice
	}
}
