package thirdparty

import (
	"os"
	"path/filepath"
	"testing"
)

// TestJoinAPIPath 覆盖 base 含/不含版本段的四种用户写法：
// 配错一层 /v1 是最常见的 404 来源，两种形态都必须落到正确端点。
func TestJoinAPIPath(t *testing.T) {
	cases := []struct {
		name string
		base string
		path string
		want string
	}{
		{"base 含 /v1", "https://api.deepseek.com/v1", "/chat/completions",
			"https://api.deepseek.com/v1/chat/completions"},
		{"base 不含版本段", "https://api.openai.com", "/chat/completions",
			"https://api.openai.com/v1/chat/completions"},
		{"base 含 /v1beta", "https://host.example/v1beta", "/chat/completions",
			"https://host.example/v1beta/chat/completions"},
		{"多级路径含版本段", "https://host.example/openai/v1", "/chat/completions",
			"https://host.example/openai/v1/chat/completions"},
		{"自定义 path", "https://host.example/v1", "/custom",
			"https://host.example/v1/custom"},
		{"path 缺前导斜杠", "https://host.example/v1", "chat/completions",
			"https://host.example/v1/chat/completions"},
		{"域名本身以 v 开头但非版本段", "https://v1.example.com", "/chat/completions",
			"https://v1.example.com/v1/chat/completions"},
		{"本地端口含 /v1", "http://127.0.0.1:11434/v1", "/chat/completions",
			"http://127.0.0.1:11434/v1/chat/completions"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := joinAPIPath(c.base, c.path); got != c.want {
				t.Fatalf("joinAPIPath(%q, %q) = %q, want %q", c.base, c.path, got, c.want)
			}
		})
	}
}

// TestProviderForModel 覆盖三级选择：精确匹配 → 兜底 provider → 未命中。
func TestProviderForModel(t *testing.T) {
	c := &Config{Enabled: true, Providers: []Provider{
		{Name: "a", BaseURL: "https://a.example/v1", Models: []string{"Model-A", "model-b"}},
		{Name: "b", BaseURL: "https://b.example/v1", Models: []string{"model-c"}},
		{Name: "fallback", BaseURL: "https://f.example/v1", Models: nil},
	}}
	cases := []struct {
		model string
		want  string // 期望的 provider name；空 = 期望 nil
	}{
		{"model-a", "a"}, // 大小写不敏感
		{"MODEL-B", "a"}, // 声明侧大小写混合
		{"model-c", "b"}, // 命中第二个 provider
		{"unknown", "fallback"},
	}
	for _, c2 := range cases {
		got := c.ProviderForModel(c2.model)
		if c2.want == "" {
			if got != nil {
				t.Fatalf("ProviderForModel(%q) = %v, want nil", c2.model, got.Name)
			}
			continue
		}
		if got == nil || got.Name != c2.want {
			t.Fatalf("ProviderForModel(%q) = %v, want %q", c2.model, got, c2.want)
		}
	}

	// 无兜底 provider 时未命中返回 nil（调用方据此回 404）。
	noFallback := &Config{Enabled: true, Providers: []Provider{
		{Name: "a", BaseURL: "https://a.example/v1", Models: []string{"model-a"}},
	}}
	if got := noFallback.ProviderForModel("unknown"); got != nil {
		t.Fatalf("未命中且无兜底时应返回 nil，得到 %v", got.Name)
	}
	// Enabled=false 恒不命中（路由本就不注册，双保险）。
	off := &Config{Enabled: false, Providers: []Provider{
		{Name: "a", BaseURL: "https://a.example/v1", Models: nil},
	}}
	if got := off.ProviderForModel("any"); got != nil {
		t.Fatalf("Enabled=false 应返回 nil，得到 %v", got.Name)
	}
}

// TestValidate 配置错误必须 fail fast——静默忽略一条 provider 会表现为请求 404，
// 比启动期报错难排查得多。
func TestValidate(t *testing.T) {
	cases := []struct {
		name    string
		cfg     Config
		wantErr bool
	}{
		{"合法", Config{Providers: []Provider{{Name: "a", BaseURL: "https://a.example/v1"}}}, false},
		{"name 为空", Config{Providers: []Provider{{BaseURL: "https://a.example"}}}, true},
		{"name 重复", Config{Providers: []Provider{
			{Name: "a", BaseURL: "https://a.example"},
			{Name: "a", BaseURL: "https://b.example"},
		}}, true},
		{"base_url 为空", Config{Providers: []Provider{{Name: "a"}}}, true},
		{"base_url 无 scheme", Config{Providers: []Provider{{Name: "a", BaseURL: "api.example.com"}}}, true},
		{"协议不支持", Config{Providers: []Provider{
			{Name: "a", BaseURL: "https://a.example", Protocol: "anthropic"},
		}}, true},
		{"空 providers 合法", Config{}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			c.cfg.normalize()
			err := c.cfg.validate()
			if (err != nil) != c.wantErr {
				t.Fatalf("validate() err = %v, wantErr = %v", err, c.wantErr)
			}
		})
	}
}

// TestNormalize 缺省值补齐 + 大小写归一。
func TestNormalize(t *testing.T) {
	c := Config{Providers: []Provider{{
		Name: "  a  ", BaseURL: "https://a.example/v1/", Protocol: "OpenAI",
	}}}
	c.normalize()
	p := c.Providers[0]
	if p.Name != "a" {
		t.Fatalf("name 未 trim: %q", p.Name)
	}
	if p.BaseURL != "https://a.example/v1" {
		t.Fatalf("base_url 未去尾斜杠: %q", p.BaseURL)
	}
	if p.Protocol != ProtocolOpenAI {
		t.Fatalf("protocol 未归一: %q", p.Protocol)
	}
	if p.ChatPath != defaultChatPath || p.ModelsPath != defaultModelsPath {
		t.Fatalf("路径缺省未补齐: %q / %q", p.ChatPath, p.ModelsPath)
	}
	if p.TimeoutSeconds != defaultTimeoutSeconds {
		t.Fatalf("超时缺省未补齐: %d", p.TimeoutSeconds)
	}
}

// TestLoadMissingFile 文件不存在 → (nil, nil)：调用方据此判定"未启用第三方"，
// 与"配置损坏"（返回 error）区分开。
func TestLoadMissingFile(t *testing.T) {
	cfg, err := Load(filepath.Join(t.TempDir(), "nope.json"))
	if err != nil {
		t.Fatalf("文件缺失不应返回 error，得到 %v", err)
	}
	if cfg != nil {
		t.Fatalf("文件缺失应返回 nil 配置，得到 %+v", cfg)
	}
}

// TestLoadRoundTrip 落盘→读回，覆盖 JSON 字段名与缺省补齐。
func TestLoadRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "third_party.json")
	raw := `{"enabled":true,"providers":[{"name":"x","base_url":"https://x.example/v1","api_key":"sk-1","models":["m1"]}]}`
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Enabled || len(cfg.Providers) != 1 {
		t.Fatalf("加载结果不符: %+v", cfg)
	}
	p := cfg.Providers[0]
	if p.ChatURL() != "https://x.example/v1/chat/completions" {
		t.Fatalf("ChatURL = %q", p.ChatURL())
	}
	if p.TimeoutSeconds != defaultTimeoutSeconds {
		t.Fatalf("超时应补缺省，得到 %d", p.TimeoutSeconds)
	}
}

// TestAllModels 汇总去重；兜底 provider（models 为空）不贡献条目。
func TestAllModels(t *testing.T) {
	c := &Config{Enabled: true, Providers: []Provider{
		{Name: "a", Models: []string{"m1", "m2"}},
		{Name: "b", Models: []string{"M2", "m3"}}, // M2 与 m2 视为重复
		{Name: "fallback", Models: nil},
	}}
	got := c.AllModels()
	want := []string{"m1", "m2", "m3"}
	if len(got) != len(want) {
		t.Fatalf("AllModels() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("AllModels() = %v, want %v", got, want)
		}
	}
}
