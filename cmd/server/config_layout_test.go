package main

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// config_layout_test.go 把「配置该放哪」这条规则变成可执行断言。
//
// 判定规则只有一句：**这个字段只对某一个上游有意义吗？**
// 是 → 进该上游的 codebuddy.json / trae.json；否 → 进 config.json。
//
// 规则写在 docs/config-layout.md，但文档不会在 CI 里失败，这条测试会。
// 加新配置时如果放错地方，这里直接报错并说明该往哪放。

// upstreamOwnedTopLevel config.json **不允许**出现的顶层段。
//
// 这些段曾经在主配置里，装的是上游专有字段（域 base、UA、版本、指纹脱敏），
// 已迁到各自的 codebuddy.json / trae.json。它们回来就意味着规则被破坏。
var upstreamOwnedTopLevel = map[string]string{
	"cn":       "域 base 属于上游（codebuddy.json / trae.json 的 cn 段）",
	"global":   "域 base 与域开关属于上游（codebuddy.json 的 global 段）",
	"features": "指纹脱敏是 CodeBuddy 专有（codebuddy.json 的 sanitize_blacklist_fingerprints）",
}

// upstreamOwnedFields config.json 的**任意层级**都不许出现的字段名。
//
// 用字段名而不是完整路径匹配：这些名字在上游文件里是核心概念，出现在主配置
// 里几乎必然是放错了位置（哪怕嵌在某个看起来通用的段下面）。
var upstreamOwnedFields = map[string]string{
	"chat_base":                       "上游端点地址属于上游文件",
	"billing_base":                    "上游端点地址属于上游文件",
	"api_base":                        "上游端点地址属于上游文件",
	"ide_version":                     "出站头族属于上游文件",
	"ide_version_code":                "出站头族属于上游文件",
	"checkin_user_agent":              "上游签到链路的 UA 属于上游文件",
	"oauth_client_id":                 "上游 OAuth 常量属于上游文件",
	"oauth_client_secret":             "上游 OAuth 常量属于上游文件",
	"sanitize_blacklist_fingerprints": "指纹脱敏是 CodeBuddy 专有",
	"device_token_file":               "上游风控令牌机制属于上游文件",
}

// TestConfigJSONHasNoUpstreamFields 序列化后的 config.json 不得含上游专有字段。
//
// 这是「一上游一文件」规则的守门测试。它检查的是**序列化结果**而不是结构体
// 定义——因为真正写给用户的文件才是事实，结构体里藏个 json tag 同样会污染配置。
func TestConfigJSONHasNoUpstreamFields(t *testing.T) {
	c := Default()
	if err := c.normalize(); err != nil {
		t.Fatalf("normalize: %v", err)
	}
	raw, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	var root map[string]json.RawMessage
	if err := json.Unmarshal(raw, &root); err != nil {
		t.Fatal(err)
	}

	for key, why := range upstreamOwnedTopLevel {
		if _, ok := root[key]; ok {
			t.Errorf("config.json 顶层出现 %q —— %s。应放进对应的上游文件。", key, why)
		}
	}

	// 递归检查任意层级的字段名。
	var walk func(v any, path string)
	walk = func(v any, path string) {
		switch node := v.(type) {
		case map[string]any:
			for k, child := range node {
				p := k
				if path != "" {
					p = path + "." + k
				}
				if why, bad := upstreamOwnedFields[k]; bad {
					t.Errorf("config.json 出现上游专有字段 %q —— %s。应放进对应的上游文件。", p, why)
				}
				walk(child, p)
			}
		case []any:
			for _, child := range node {
				walk(child, path)
			}
		}
	}
	var generic map[string]any
	if err := json.Unmarshal(raw, &generic); err != nil {
		t.Fatal(err)
	}
	walk(generic, "")
}

// TestConfigTopLevelSectionsAreKnown config.json 的顶层段必须都在白名单里。
//
// 白名单是「已按语义审视过、确认与上游无关」的集合。新增顶层段时这里会失败，
// 强制加配置的人回答一次：这个段真的与上游无关吗？还是该进某个上游文件？
//
// 为什么用白名单而不是黑名单：黑名单只能挡住"已知的错"，白名单能挡住"没想到的错"。
func TestConfigTopLevelSectionsAreKnown(t *testing.T) {
	// 每项都要能说清"为什么与上游无关"。不能自圆其说的段就该搬走。
	allowed := map[string]string{
		"listen":         "进程监听地址",
		"api_key":        "网关鉴权，入口侧",
		"auth_dir":       "凭证目录位置（两个上游共用同一目录，靠前缀区分）",
		"state_file":     "池状态文件位置",
		"dns_servers":    "出站解析，传输层",
		"server":         "网关对外服务的入口参数",
		"upstream":       "传输层超时（两个上游共用）",
		"pool":           "账号池调度策略（分域字段是路由维度，不是上游维度）",
		"cooldown":       "通用退避时长，与上游无关",
		"schedule":       "排程是所有上游共用的（见架构手册「排程是单点的」）",
		"prompt":         "网关注入的提示词",
		"models":         "对外暴露哪些模型，是网关展示策略",
		"session_sticky": "会话粘性，跨上游统一",
		"logging":        "可观测性",
		"panel":          "面板展示",
		"upstash":        "状态镜像存储",
	}

	c := Default()
	if err := c.normalize(); err != nil {
		t.Fatalf("normalize: %v", err)
	}
	raw, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	var root map[string]json.RawMessage
	if err := json.Unmarshal(raw, &root); err != nil {
		t.Fatal(err)
	}
	for key := range root {
		if _, ok := allowed[key]; !ok {
			var why []string
			for k, v := range allowed {
				why = append(why, k+"="+v)
			}
			t.Errorf("config.json 出现未审视的顶层段 %q。\n"+
				"请确认：这个段只对某一个上游有意义吗？\n"+
				"  - 是 → 放进该上游的 codebuddy.json / trae.json\n"+
				"  - 否 → 加进本测试的白名单，并写清为什么与上游无关\n"+
				"已审视的段：%s", key, strings.Join(why, ", "))
		}
	}
}

// TestUpstreamConfigStructsDoNotLeakIntoMain 上游配置结构不得被主 Config 引用。
//
// 结构体级别再挡一道：即便有人给某个上游字段加了 `json:"-"`，只要它出现在
// cmd/server.Config 的字段里，就说明心智上仍把它当主配置的一部分。
func TestUpstreamConfigStructsDoNotLeakIntoMain(t *testing.T) {
	typ := reflect.TypeOf(Config{})
	for i := 0; i < typ.NumField(); i++ {
		f := typ.Field(i)
		name := f.Type.Name()
		// 上游配置类型（codebuddy.Config / trae.Config）不应作为字段类型出现。
		if strings.Contains(name, "codebuddy") || strings.HasPrefix(f.Name, "Trae") ||
			strings.HasPrefix(f.Name, "CodeBuddy") {
			t.Errorf("cmd/server.Config 字段 %q（类型 %s）看起来是上游配置。"+
				"上游专有配置应放在各自文件中，由 load*Config 加载。", f.Name, f.Type)
		}
	}
}
