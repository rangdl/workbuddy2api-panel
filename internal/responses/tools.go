package responses

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
)

// buildToolContext 从 Responses 请求的 tools 声明构造 Chat 工具映射。
// 除顶层 tools 外，也提升 input 里声明的工具（additional_tools / tool_search_output）。
// 最后按别名表注入别名工具（见 legacyAliasTools）。
func buildToolContext(body map[string]any) *ToolContext {
	ctx := NewToolContext()
	if tools, ok := body["tools"].([]any); ok {
		for _, raw := range tools {
			ctx.addResponseTool(raw)
		}
	}
	if input, ok := body["input"]; ok {
		collectInputDeclaredTools(input, ctx)
	}
	ctx.injectAliasTools()
	return ctx
}

// legacyAlias 旧版 codex 工具名 → 新版真名的别名条目。
type legacyAlias struct {
	legacyName  string            // 模型可能幻觉输出的旧名（Chat 侧别名工具名）
	targetName  string            // codex 实际声明的新名（Responses 侧真名）
	legacyDesc  string            // 别名工具描述（供模型选择）
	legacyParam map[string]any    // 别名工具的旧版参数 schema
	remapArgs   func(string) any // 旧版参数 → 新版参数（nil = 原样透传）
}

// legacyAliases 旧版 codex 工具别名表。
// 背景：codex 0.15x 注册的工具是 exec_command / write_stdin 等，glm / deepseek
// 类模型按训练记忆常输出旧版 codex 的 shell / apply_patch 等——codex 端报
// "unsupported call: shell" 并拒绝执行（实测复现）。提示注入只能纠偏概率，
// 这里补确定性兜底：当 codex 声明了新版工具时，向 Chat 上游注入同名旧版工具，
// 模型输出旧名也能落到「真实声明」的调用，回程还原为新名。
// 只处理 shell（旧版执行命令的入口，模型幻觉的重灾区）：
// apply_patch / update_plan 等旧名在新版 codex 中无一一对应的新工具
// （apply_patch 的语义已并入 exec_command 的 heredoc 用法），无法安全映射。
var legacyAliases = []legacyAlias{
	{
		legacyName: "shell",
		targetName: "exec_command",
		legacyDesc: "Runs a shell command and returns its output.",
		legacyParam: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"command": map[string]any{
					"type":        "array",
					"items":       map[string]any{"type": "string"},
					"description": "The command tokens to execute (first element is the program).",
				},
				"workdir": map[string]any{"type": "string", "description": "Working directory."},
				"timeout": map[string]any{"type": "number", "description": "Timeout in milliseconds."},
			},
		},
		// 旧版 command 是 token 数组（["bash","-lc","..."]），新版 cmd 是 shell 字符串。
		// 数组按 OpenAI shells 的 bash -lc 惯例拼回一行；已是字符串则原样用。
		remapArgs: func(arguments string) any {
			var args map[string]any
			if err := json.Unmarshal([]byte(arguments), &args); err != nil || args == nil {
				return map[string]any{"cmd": arguments}
			}
			out := map[string]any{}
			for k, v := range args {
				out[k] = v
			}
			if _, ok := out["cmd"]; ok {
				return out // 已有 cmd（模型混用两套参数）：原样
			}
			switch cmd := args["command"].(type) {
			case []any:
				out["cmd"] = joinCommandTokens(cmd)
			case string:
				out["cmd"] = cmd
			default:
				// 无 command 参数：保留其余字段（workdir/timeout），cmd 留空由上游纠错
				out["cmd"] = ""
			}
			delete(out, "command")
			return out
		},
	},
}

// joinCommandTokens 把旧版 command token 数组拼回 shell 字符串。
// 首个 token 是程序名（惯常 bash/sh）；bash/sh 的 -lc 惯例下最后一个 token 是
// 完整脚本，直接取它；否则逐 token 空格拼接。
func joinCommandTokens(tokens []any) string {
	parts := make([]string, 0, len(tokens))
	for _, t := range tokens {
		if s, ok := t.(string); ok {
			parts = append(parts, s)
		}
	}
	if len(parts) == 0 {
		return ""
	}
	for _, shell := range []string{"bash", "sh", "zsh", "dash"} {
		if parts[0] == shell && len(parts) >= 3 {
			for _, flag := range parts[1 : len(parts)-1] {
				if flag == "-lc" || flag == "-c" {
					return parts[len(parts)-1]
				}
			}
		}
	}
	return strings.Join(parts, " ")
}

// injectAliasTools 按 legacyAliases 注入别名工具：仅当目标工具已被 codex 声明
// 且旧名未被占用时。别名与真名并存——模型输出哪个名字都能命中真实声明。
func (c *ToolContext) injectAliasTools() {
	for _, alias := range legacyAliases {
		spec, declared := c.chatNameToSpec[alias.targetName]
		if !declared || spec.Kind != toolKindFunction || spec.Namespace != "" {
			continue
		}
		if _, taken := c.chatNameToSpec[alias.legacyName]; taken {
			continue
		}
		chatTool := map[string]any{
			"type": "function",
			"function": map[string]any{
				"name":        alias.legacyName,
				"description": alias.legacyDesc,
				"parameters":  alias.legacyParam,
			},
		}
		c.add(alias.legacyName, ToolSpec{Kind: toolKindAlias, Name: alias.legacyName, AliasFor: alias.targetName}, chatTool)
	}
}

// addResponseTool 登记一个 Responses 工具声明。
func (c *ToolContext) addResponseTool(raw any) {
	switch tool := raw.(type) {
	case string:
		// 字符串工具名视作 custom 工具（cc-switch 同口径）。
		c.addCustom(map[string]any{"type": toolKindCustom, "name": tool})
	case map[string]any:
		// 仅识别可映射到 Chat 的工具类型。web_search / x_search /
		// image_generation 等服务端工具（Responses server-side tools）无法在
		// Chat 上游执行，**有意忽略**（对齐 cc-switch：落到 default 不转换、
		// 不报错）。Codex 发现该工具不可用后会自行降级；保留它会让严格上游 400。
		switch stringField(tool, "type") {
		case toolKindFunction:
			c.addFunction(tool, "")
		case toolKindCustom:
			c.addCustom(tool)
		case toolKindToolSearch:
			c.addToolSearch()
		case toolKindNamespace:
			c.addNamespace(tool)
		}
	}
}

// collectInputDeclaredTools 递归收集 input 里声明的工具
// （additional_tools / tool_search_output 的 tools 数组）。
func collectInputDeclaredTools(value any, ctx *ToolContext) {
	switch v := value.(type) {
	case []any:
		for _, item := range v {
			collectInputDeclaredTools(item, ctx)
		}
	case map[string]any:
		switch stringField(v, "type") {
		case "tool_search_output", "additional_tools":
			if tools, ok := v["tools"].([]any); ok {
				for _, tool := range tools {
					ctx.addResponseTool(tool)
				}
			}
		}
		for _, item := range v {
			collectInputDeclaredTools(item, ctx)
		}
	}
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
