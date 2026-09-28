package trae

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// payload.go OpenAI Chat Completions 请求体 → Trae `llm_utils_chat` 请求体。
//
// 蓝本：TraeWorkAssistant 的 api_server/payload.rs（2026-09 实测固化）。
// 转换是**纯函数**（除注入的 id 生成器外无副作用），便于表驱动单测。

// function 取值：Trae Work 模式默认 solo_work_lite；部分模型仅在 solo_agent 下可用。
const (
	FunctionSoloWorkLite = "solo_work_lite"
	FunctionSoloAgent    = "solo_agent"
)

// soloAgentModels 仅在 solo_agent 视图可用的模型（实测）。
var soloAgentModels = map[string]bool{
	"doubao-seed-code":    true,
	"glm-5.3-flash":       true,
	"qwen3.8-flash":       true,
	"deepseek-v4.1-flash": true,
	"kimi-k2.8-preview":   true,
}

// FunctionForModel 模型 → function（硬编码回退；调用方传小写模型名）。
func FunctionForModel(modelLower string) string {
	if soloAgentModels[strings.ToLower(strings.TrimSpace(modelLower))] {
		return FunctionSoloAgent
	}
	return FunctionSoloWorkLite
}

// modelPair 模型显示名 → (config_name, model_name)。
// 未收录模型返回 ok=false，调用方按实证规律原样透传（model_name = config_name__dev）。
var modelPairs = map[string][2]string{
	"doubao-seed-evolving":       {"Doubao-Seed-Evolving", "Doubao-Seed-Evolving__dev"},
	"doubao-seed-2.1-pro":        {"Doubao-Seed-2.1-Pro", "Doubao-Seed-2.1-Pro__dev"},
	"seed-code-pro-0430":         {"Doubao-Seed-2.1-Pro", "Doubao-Seed-2.1-Pro__dev"},
	"doubao-seed-2.1-turbo":      {"Doubao-Seed-2.1-Turbo", "Doubao-Seed-2.1-Turbo__dev"},
	"doubao-seed-code":           {"Doubao-Seed-Code", "Doubao-Seed-Code__dev"},
	"glm-5.3-flash":              {"glm-5.3-flash", "glm-5.3-flash__dev"},
	"qwen3.8-flash":              {"qwen3.8-flash", "qwen3.8-flash__dev"},
	"glm-5.2":                    {"glm-5.2", "glm-5.2__dev"},
	"glm-5.3":                    {"glm-5.3", "glm-5.3__dev"},
	"glm-5":                      {"glm-5", "glm-5__dev"},
	"glm-5-turbo":                {"glm-5-turbo", "glm-5-turbo__dev"},
	"deepseek-v4.1-flash":        {"deepseek-v4.1-flash", "deepseek-v4.1-flash__dev"},
	"deepseek-v4-flash":          {"DeepSeek-V4-Flash", "deepseek_v4_flash__dev"},
	"deepseek-v4-flash-official": {"DeepSeek-V4-Flash-Official", "DeepSeek-V4-Flash-Official__dev"},
	"deepseek-v4-pro":            {"DeepSeek-V4-Pro", "deepseek_v4_pro__dev"},
	"deepseek-v4-pro-official":   {"DeepSeek-V4-Pro-Official", "DeepSeek-V4-Pro-Official__dev"},
	"kimi-k2.6":                  {"kimi-k2.6", "kimi-k2.6__dev"},
	"kimi-k2.7-code":             {"kimi-k2.7-code", "kimi-k2.7-code__dev"},
	"kimi-k2.8-preview":          {"kimi-k2.8-preview", "kimi-k2.8-preview__dev"},
	"kimi-k3":                    {"kimi-k3", "kimi-k3__dev"},
	"minimax-m3":                 {"minimax-m3", "minimax-m3__dev"},
	"qwen3.8-max":                {"qwen3.8-max", "qwen3.8-max__dev"},
	"qwen-3.7-plus":              {"qwen-3.7-plus", "qwen-3.7-plus__dev"},
}

// ModelPair 返回模型的 (config_name, model_name)；未收录时 ok=false。
func ModelPair(model string) (configName, modelName string, ok bool) {
	p, hit := modelPairs[strings.ToLower(strings.TrimSpace(model))]
	if !hit {
		return "", "", false
	}
	return p[0], p[1], true
}

// KnownModels 返回内置模型目录（config_name 列表，去重排序）。
// 供 /trae/v1/models 在无账号、无法拉取上游目录时使用。
func KnownModels() []string {
	seen := make(map[string]bool, len(modelPairs))
	out := make([]string, 0, len(modelPairs))
	for _, p := range modelPairs {
		if seen[p[0]] {
			continue
		}
		seen[p[0]] = true
		out = append(out, p[0])
	}
	sort.Strings(out)
	return out
}

// PrepareOptions 请求体构造参数。
type PrepareOptions struct {
	// DefaultModel 请求体未给 model 时的兜底。
	DefaultModel string
	// UID 账号 user_id。
	UID string
	// DeviceID / MachineID 设备标识（出站体字段）。
	DeviceID  string
	MachineID string
	// IDEVersion / IDEVersionCode / AppID 客户端身份字段。
	IDEVersion     string
	IDEVersionCode string
	AppID          string
	// NewID 生成 uuid-like 字符串；nil 用内置实现（测试可注入固定值）。
	NewID func() string
}

// PrepareChatBody 把 OpenAI Chat 请求体改写为 llm_utils_chat 请求体。
//
// 转换内容（逐条对应参考实现）：
//  1. messages[].content 字符串 → [{type:"text", text:...}]；
//  2. assistant 消息的 tool_calls[].function → function_call（上游用旧字段名），
//     并剔除 function_call.name 为空的调用（否则上游 400）；
//  3. model → config_name + model_name（未收录时 model_name = config_name__dev）；
//  4. tool_choice 归一化为字符串（对象形态上游报 400）；"none" 直接删除 tools；
//  5. tools[].function.parameters 对象 → JSON 字符串（上游要求）；
//  6. 注入 llm_utils_chat 必需字段（stream 恒 true、function、max_tokens、
//     conversation_id/session_id/project_id、device_id/machine_id、ide_version 等）。
//
// 解析失败返回原样字节与 error（调用方决定是否放行原始体）。
func PrepareChatBody(src []byte, opt PrepareOptions) ([]byte, error) {
	var obj map[string]any
	if err := json.Unmarshal(src, &obj); err != nil {
		return src, fmt.Errorf("请求体不是合法 JSON: %w", err)
	}
	if opt.NewID == nil {
		opt.NewID = genUUIDLike
	}

	rewriteMessages(obj)
	normalizeToolChoice(obj)
	normalizeTools(obj)

	model := ""
	if m, ok := obj["model"].(string); ok {
		model = strings.TrimSpace(m)
	}
	if model == "" {
		model = strings.TrimSpace(opt.DefaultModel)
	}
	configName, modelName, ok := ModelPair(model)
	if !ok {
		// 未收录模型（官方新上架等）原样透传：实证 model_name = config_name__dev。
		configName = model
		modelName = model + "__dev"
	}

	obj["config_name"] = configName
	obj["model_name"] = modelName
	obj["stream"] = true
	obj["function"] = FunctionForModel(strings.ToLower(model))
	obj["max_tokens"] = maxTokensOf(obj)
	obj["conversation_id"] = opt.NewID()
	obj["session_id"] = opt.NewID()
	obj["project_id"] = opt.NewID()
	obj["user_id"] = opt.UID
	obj["device_id"] = opt.DeviceID
	obj["machine_id"] = opt.MachineID
	obj["workspace_id"] = DefaultWorkspaceID
	obj["prompt_max_tokens"] = DefaultPromptMaxTokens
	obj["mode"] = "FunctionCall"
	obj["ide_version"] = opt.IDEVersion
	obj["ide_version_code"] = opt.IDEVersionCode
	obj["app_id"] = opt.AppID
	obj["package_type"] = "stable_cn"

	out, err := json.Marshal(obj)
	if err != nil {
		return src, fmt.Errorf("请求体序列化失败: %w", err)
	}
	return out, nil
}

// rewriteMessages content 数组化 + tool_calls 字段名归一。
func rewriteMessages(obj map[string]any) {
	msgs, _ := obj["messages"].([]any)
	for _, raw := range msgs {
		m, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		role, _ := m["role"].(string)
		if role == "assistant" {
			rewriteToolCalls(m)
		}
		if s, ok := m["content"].(string); ok {
			m["content"] = []any{map[string]any{"type": "text", "text": s}}
		}
	}
}

// rewriteToolCalls 把 tool_calls[].function 改名为 function_call，并剔除空名调用。
func rewriteToolCalls(m map[string]any) {
	tcs, ok := m["tool_calls"].([]any)
	if !ok {
		return
	}
	kept := make([]any, 0, len(tcs))
	for _, raw := range tcs {
		tc, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if fn, exists := tc["function"]; exists {
			tc["function_call"] = fn
			delete(tc, "function")
		}
		fc, _ := tc["function_call"].(map[string]any)
		name, _ := fc["name"].(string)
		if strings.TrimSpace(name) == "" {
			continue
		}
		kept = append(kept, tc)
	}
	if len(kept) == 0 {
		delete(m, "tool_calls")
		return
	}
	m["tool_calls"] = kept
}

// normalizeToolChoice 把对象形态的 tool_choice 归一化为字符串；
// "none" 同时删除 tools / functions（上游不认空工具集）。
func normalizeToolChoice(obj map[string]any) {
	tc, exists := obj["tool_choice"]
	if !exists {
		return
	}
	suppress := func() {
		delete(obj, "tools")
		delete(obj, "functions")
		delete(obj, "tool_choice")
	}
	switch v := tc.(type) {
	case string:
		if strings.EqualFold(strings.TrimSpace(v), "none") {
			suppress()
		}
	case map[string]any:
		switch strings.ToLower(strings.TrimSpace(str(v["type"]))) {
		case "none":
			suppress()
		case "auto", "required":
			obj["tool_choice"] = strings.ToLower(strings.TrimSpace(str(v["type"])))
		case "function":
			name := ""
			if fn, ok := v["function"].(map[string]any); ok {
				name = strings.TrimSpace(str(fn["name"]))
			}
			if name == "" {
				name = strings.TrimSpace(str(v["name"]))
			}
			if name == "" {
				name = "auto"
			}
			obj["tool_choice"] = name
		}
	}
}

// normalizeTools 把 tools[].function.parameters 对象转成 JSON 字符串（上游要求），
// 并丢弃缺 function 对象的条目；空列表直接删除字段。
func normalizeTools(obj map[string]any) {
	list, ok := obj["tools"].([]any)
	if !ok {
		return
	}
	if len(list) == 0 {
		delete(obj, "tools")
		return
	}
	out := make([]any, 0, len(list))
	for _, raw := range list {
		t, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		fn, ok := t["function"].(map[string]any)
		if !ok {
			continue
		}
		if params, ok := fn["parameters"].(map[string]any); ok {
			if b, err := json.Marshal(params); err == nil {
				fn["parameters"] = string(b)
			}
		}
		out = append(out, t)
	}
	if len(out) == 0 {
		delete(obj, "tools")
		return
	}
	obj["tools"] = out
}

// maxTokensOf 取客户端显式 max_tokens / max_completion_tokens（>0 才采信），
// 否则回落 DefaultMaxTokens——上游该字段必填。
func maxTokensOf(obj map[string]any) int64 {
	for _, k := range []string{"max_tokens", "max_completion_tokens"} {
		switch v := obj[k].(type) {
		case float64:
			if v > 0 {
				return int64(v)
			}
		case int64:
			if v > 0 {
				return v
			}
		case json.Number:
			if n, err := v.Int64(); err == nil && n > 0 {
				return n
			}
		}
	}
	return DefaultMaxTokens
}

// str 取字符串值（非字符串返回空串）。
func str(v any) string {
	s, _ := v.(string)
	return s
}

// genUUIDLike 生成 uuid-like 十六进制串（8-4-4-4-12），
// 与参考实现同为"格式像 UUID 即可"的上游占位值。
func genUUIDLike() string {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		// 极端情况（系统熵源不可用）退化为全零——上游只校验格式。
		buf = make([]byte, 16)
	}
	h := hex.EncodeToString(buf)
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}
