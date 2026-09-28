package trae

import (
	"encoding/json"
	"testing"
)

func testPrepareOpt() PrepareOptions {
	n := 0
	return PrepareOptions{
		DefaultModel:   "glm-5.2",
		UID:            "u-1",
		DeviceID:       "dev-1",
		MachineID:      "machine-1",
		IDEVersion:     DefaultIDEVersion,
		IDEVersionCode: DefaultIDEVersionCode,
		AppID:          DefaultAppID,
		NewID: func() string {
			n++
			return "id-" + string(rune('0'+n))
		},
	}
}

func prepare(t *testing.T, body string, opt PrepareOptions) map[string]any {
	t.Helper()
	out, err := PrepareChatBody([]byte(body), opt)
	if err != nil {
		t.Fatalf("PrepareChatBody: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("输出非 JSON: %v", err)
	}
	return m
}

// TestPrepareRequiredFields llm_utils_chat 必需字段必须齐备，且 stream 恒为 true。
func TestPrepareRequiredFields(t *testing.T) {
	m := prepare(t, `{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`, testPrepareOpt())
	checks := map[string]any{
		"config_name":       "glm-5.2",
		"model_name":        "glm-5.2__dev",
		"stream":            true,
		"function":          FunctionSoloWorkLite,
		"user_id":           "u-1",
		"device_id":         "dev-1",
		"machine_id":        "machine-1",
		"workspace_id":      DefaultWorkspaceID,
		"prompt_max_tokens": float64(DefaultPromptMaxTokens),
		"mode":              "FunctionCall",
		"ide_version":       DefaultIDEVersion,
		"app_id":            DefaultAppID,
		"package_type":      "stable_cn",
	}
	for k, want := range checks {
		if got := m[k]; got != want {
			t.Errorf("%s = %v, want %v", k, got, want)
		}
	}
	for _, k := range []string{"conversation_id", "session_id", "project_id"} {
		if s, _ := m[k].(string); s == "" {
			t.Errorf("%s 未注入", k)
		}
	}
	// 客户端未给 max_tokens → 兜底 4096（上游必填）
	if got := m["max_tokens"]; got != float64(DefaultMaxTokens) {
		t.Errorf("max_tokens = %v, want %d", got, DefaultMaxTokens)
	}
}

// TestPrepareContentArrayize content 字符串必须转成 [{type:text,text}] 数组。
func TestPrepareContentArrayize(t *testing.T) {
	m := prepare(t, `{"model":"glm-5.2","messages":[
		{"role":"system","content":"be brief"},
		{"role":"user","content":"hi"}
	]}`, testPrepareOpt())
	msgs, _ := m["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("messages 数 = %d", len(msgs))
	}
	for i, raw := range msgs {
		msg, _ := raw.(map[string]any)
		arr, ok := msg["content"].([]any)
		if !ok || len(arr) != 1 {
			t.Fatalf("messages[%d].content 未数组化: %v", i, msg["content"])
		}
		part, _ := arr[0].(map[string]any)
		if part["type"] != "text" {
			t.Errorf("messages[%d].content[0].type = %v", i, part["type"])
		}
	}
}

// TestPrepareToolCallsRename assistant 的 tool_calls[].function 必须改名为
// function_call，且空名调用被剔除（否则上游 400）。
func TestPrepareToolCallsRename(t *testing.T) {
	m := prepare(t, `{"model":"glm-5.2","messages":[{"role":"assistant","tool_calls":[
		{"id":"c1","type":"function","function":{"name":"read_file","arguments":"{}"}},
		{"id":"c2","type":"function","function":{"name":"  ","arguments":"{}"}}
	]}]}`, testPrepareOpt())
	msgs, _ := m["messages"].([]any)
	msg, _ := msgs[0].(map[string]any)
	tcs, ok := msg["tool_calls"].([]any)
	if !ok {
		t.Fatalf("tool_calls 丢失: %v", msg)
	}
	if len(tcs) != 1 {
		t.Fatalf("空名调用未被剔除，剩 %d 条", len(tcs))
	}
	tc, _ := tcs[0].(map[string]any)
	if _, bad := tc["function"]; bad {
		t.Error("function 字段应已改名为 function_call")
	}
	fc, ok := tc["function_call"].(map[string]any)
	if !ok || fc["name"] != "read_file" {
		t.Fatalf("function_call 不符: %v", tc)
	}

	// 全部空名 → 整个 tool_calls 字段删除
	m = prepare(t, `{"model":"glm-5.2","messages":[{"role":"assistant","tool_calls":[
		{"id":"c1","function":{"name":""}}]}]}`, testPrepareOpt())
	msgs, _ = m["messages"].([]any)
	msg, _ = msgs[0].(map[string]any)
	if _, exists := msg["tool_calls"]; exists {
		t.Error("全空名时应删除 tool_calls 字段")
	}
}

// TestPrepareModelMapping 收录模型查表；未收录模型按 model_name = config_name__dev 透传。
func TestPrepareModelMapping(t *testing.T) {
	cases := []struct {
		model      string
		configName string
		modelName  string
		function   string
	}{
		{"glm-5.2", "glm-5.2", "glm-5.2__dev", FunctionSoloWorkLite},
		{"GLM-5.2", "glm-5.2", "glm-5.2__dev", FunctionSoloWorkLite}, // 大小写不敏感
		{"deepseek-v4-pro", "DeepSeek-V4-Pro", "deepseek_v4_pro__dev", FunctionSoloWorkLite},
		{"deepseek-v4.1-flash", "deepseek-v4.1-flash", "deepseek-v4.1-flash__dev", FunctionSoloAgent},
		{"kimi-k2.8-preview", "kimi-k2.8-preview", "kimi-k2.8-preview__dev", FunctionSoloAgent},
		{"brand-new-model", "brand-new-model", "brand-new-model__dev", FunctionSoloWorkLite},
	}
	for _, c := range cases {
		m := prepare(t, `{"model":"`+c.model+`","messages":[]}`, testPrepareOpt())
		if m["config_name"] != c.configName {
			t.Errorf("%s: config_name = %v, want %v", c.model, m["config_name"], c.configName)
		}
		if m["model_name"] != c.modelName {
			t.Errorf("%s: model_name = %v, want %v", c.model, m["model_name"], c.modelName)
		}
		if m["function"] != c.function {
			t.Errorf("%s: function = %v, want %v", c.model, m["function"], c.function)
		}
	}

	// 未给 model → 回落 DefaultModel
	m := prepare(t, `{"messages":[]}`, testPrepareOpt())
	if m["config_name"] != "glm-5.2" {
		t.Errorf("缺 model 应回落默认，得到 %v", m["config_name"])
	}
}

// TestPrepareToolChoice tool_choice 归一化：对象 → 字符串；none → 删 tools。
func TestPrepareToolChoice(t *testing.T) {
	base := `{"model":"glm-5.2","messages":[],"tools":[{"type":"function","function":{"name":"f","parameters":{"type":"object"}}}]`

	// 对象形态 → 字符串
	m := prepare(t, base+`,"tool_choice":{"type":"auto"}}`, testPrepareOpt())
	if m["tool_choice"] != "auto" {
		t.Errorf("tool_choice = %v, want auto", m["tool_choice"])
	}
	// function 对象 → 函数名
	m = prepare(t, base+`,"tool_choice":{"type":"function","function":{"name":"f"}}}`, testPrepareOpt())
	if m["tool_choice"] != "f" {
		t.Errorf("tool_choice = %v, want f", m["tool_choice"])
	}
	// none → tools 与 tool_choice 一并删除
	m = prepare(t, base+`,"tool_choice":"none"}`, testPrepareOpt())
	if _, exists := m["tools"]; exists {
		t.Error("tool_choice=none 时应删除 tools")
	}
	if _, exists := m["tool_choice"]; exists {
		t.Error("tool_choice=none 时应删除 tool_choice 自身")
	}
}

// TestPrepareToolsParametersString tools[].function.parameters 必须转成 JSON 字符串。
func TestPrepareToolsParametersString(t *testing.T) {
	m := prepare(t, `{"model":"glm-5.2","messages":[],"tools":[
		{"type":"function","function":{"name":"f","parameters":{"type":"object","properties":{"a":{"type":"string"}}}}},
		{"type":"function"}
	]}`, testPrepareOpt())
	tools, _ := m["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("缺 function 的条目应被丢弃，剩 %d 条", len(tools))
	}
	tool, _ := tools[0].(map[string]any)
	fn, _ := tool["function"].(map[string]any)
	params, ok := fn["parameters"].(string)
	if !ok {
		t.Fatalf("parameters 应为字符串，得到 %T", fn["parameters"])
	}
	var parsed map[string]any
	if err := json.Unmarshal([]byte(params), &parsed); err != nil {
		t.Fatalf("parameters 不是合法 JSON: %v", err)
	}
	if parsed["type"] != "object" {
		t.Errorf("parameters.type = %v", parsed["type"])
	}

	// 空 tools → 字段删除
	m = prepare(t, `{"model":"glm-5.2","messages":[],"tools":[]}`, testPrepareOpt())
	if _, exists := m["tools"]; exists {
		t.Error("空 tools 应删除字段")
	}
}

// TestPrepareMaxTokens 客户端显式 max_tokens / max_completion_tokens 优先。
func TestPrepareMaxTokens(t *testing.T) {
	m := prepare(t, `{"model":"glm-5.2","messages":[],"max_tokens":1234}`, testPrepareOpt())
	if m["max_tokens"] != float64(1234) {
		t.Errorf("max_tokens = %v, want 1234", m["max_tokens"])
	}
	m = prepare(t, `{"model":"glm-5.2","messages":[],"max_completion_tokens":77}`, testPrepareOpt())
	if m["max_tokens"] != float64(77) {
		t.Errorf("max_completion_tokens 应被采信，得到 %v", m["max_tokens"])
	}
	// 0 / 负数不采信 → 兜底
	m = prepare(t, `{"model":"glm-5.2","messages":[],"max_tokens":0}`, testPrepareOpt())
	if m["max_tokens"] != float64(DefaultMaxTokens) {
		t.Errorf("max_tokens=0 应回落兜底，得到 %v", m["max_tokens"])
	}
}

// TestPrepareInvalidJSON 非法 JSON 返回原样字节 + error（调用方决定是否放行）。
func TestPrepareInvalidJSON(t *testing.T) {
	src := []byte(`{not json`)
	out, err := PrepareChatBody(src, testPrepareOpt())
	if err == nil {
		t.Fatal("非法 JSON 应返回 error")
	}
	if string(out) != string(src) {
		t.Errorf("非法 JSON 应原样返回，得到 %s", out)
	}
}

// TestFunctionForModel 模型 → function 映射（solo_agent 白名单）。
func TestFunctionForModel(t *testing.T) {
	if got := FunctionForModel("glm-5.2"); got != FunctionSoloWorkLite {
		t.Errorf("glm-5.2 function = %s", got)
	}
	for _, m := range []string{"deepseek-v4.1-flash", "GLM-5.3-FLASH", "doubao-seed-code"} {
		if got := FunctionForModel(m); got != FunctionSoloAgent {
			t.Errorf("%s function = %s, want %s", m, got, FunctionSoloAgent)
		}
	}
}
