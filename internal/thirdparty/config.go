// Package thirdparty 第三方 OpenAI 兼容上游的转发层。
//
// 与 internal/upstream（CodeBuddy 专用）的差异：
//   - 无账号池 / 无冷却 / 无熔断 / 无粘性——单上游单 Key，失败即错误直返；
//   - 无 CodeBuddy 特化（effort 降级、thinking 注入、指纹脱敏、提示词改写）——
//     第三方上游讲标准 OpenAI 协议，多做一层改写只会制造非法参数；
//   - 配置独立成文件（third_party.json），不并入 config.json。
//
// 为什么独立成包而不扩展现有 upstream：本 fork 需跟随上游更新，而
// internal/upstream/ 与 server.chatCompletions 都是上游活跃文件（两轮同步均被触及）。
// 第三方能力全部落在新文件里，现有链路零改动——唯一交集是 handler.go 的
// 3 行路由注册（见 docs/third-party-upstream-plan.md §9.4 的冲突面说明）。
package thirdparty

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
)

// 协议取值。阶段一仅 openai（与上游同为 Chat Completions，无需额外转换）；
// anthropic / gemini 需各自新增请求体与响应转换，不在本阶段。
const ProtocolOpenAI = "openai"

// 缺省路径与超时。
const (
	defaultChatPath       = "/chat/completions"
	defaultModelsPath     = "/models"
	defaultTimeoutSeconds = 120
)

// Provider 单个第三方上游。
type Provider struct {
	// Name 供日志与用量视图展示（用量记账时作为 uid 维度），必填且全局唯一。
	Name string `json:"name"`
	// Protocol 上游协议，缺省 openai。
	Protocol string `json:"protocol"`
	// BaseURL 形如 https://api.deepseek.com/v1（含或不含 /v1 均可）。
	BaseURL string `json:"base_url"`
	// ChatPath 缺省 /chat/completions；BaseURL 已含 /v1 时直接拼接，
	// 否则由 ChatURL() 补 /v1（两种形态都能对上，避免用户配错 404）。
	ChatPath string `json:"chat_path"`
	// ModelsPath 缺省 /models（仅 /tp/v1/models 与按需校验时使用）。
	ModelsPath string `json:"models_path"`
	// APIKey 空 = 不发出站 Authorization 头（本地无鉴权上游）。
	APIKey string `json:"api_key"`
	// Models 该 provider 承接的模型名列表（精确匹配，大小写不敏感）。
	// 空 = 该 provider 承接全部未命中其他 provider 的模型（兜底 provider）。
	Models []string `json:"models"`
	// TimeoutSeconds 非流式请求的总超时；流式请求只约束首字节（见 client.go）。
	TimeoutSeconds int `json:"timeout_seconds"`
	// Headers 额外出站头（覆盖同名默认头）。少数中转要求自定义鉴权头时用。
	Headers map[string]string `json:"headers"`
}

// Config third_party.json 结构。
type Config struct {
	// Enabled false 时不注册 /tp/ 路由（handler 侧据此跳过注册）。
	Enabled bool `json:"enabled"`
	// Providers 按顺序匹配；首个 models 命中的 provider 胜出。
	Providers []Provider `json:"providers"`
}

// Load 读取配置文件。文件缺失返回 (nil, nil)——调用方据此判定"未启用第三方"，
// 与"配置损坏"区分开（后者返回 error，由调用方决定是否 fail fast）。
func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	var c Config
	if err := json.Unmarshal(raw, &c); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	c.normalize()
	if err := c.validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &c, nil
}

// ConfigPath 返回与 config.json 同目录的 third_party.json 路径。
func ConfigPath(configPath string) string {
	return filepath.Join(filepath.Dir(configPath), "third_party.json")
}

// Validate 归一化并校验配置，供面板保存前调用（与 Load 内同一套规则，
// 避免面板与启动期两处校验口径漂移）。就地修改接收者。
func (c *Config) Validate() error {
	c.normalize()
	return c.validate()
}

// normalize 补齐缺省值并归一化大小写。就地修改，Load 内调用一次。
func (c *Config) normalize() {
	for i := range c.Providers {
		p := &c.Providers[i]
		if p.Protocol == "" {
			p.Protocol = ProtocolOpenAI
		}
		p.Protocol = strings.ToLower(strings.TrimSpace(p.Protocol))
		p.BaseURL = strings.TrimRight(strings.TrimSpace(p.BaseURL), "/")
		if p.ChatPath == "" {
			p.ChatPath = defaultChatPath
		}
		if p.ModelsPath == "" {
			p.ModelsPath = defaultModelsPath
		}
		if p.TimeoutSeconds <= 0 {
			p.TimeoutSeconds = defaultTimeoutSeconds
		}
		p.Name = strings.TrimSpace(p.Name)
	}
}

// validate 校验配置自洽性：name 非空且唯一、base_url 非空、协议受支持。
// 校验失败即 fail fast——静默忽略一条 provider 会让请求 404，比启动报错更难排查。
func (c *Config) validate() error {
	seen := make(map[string]bool, len(c.Providers))
	for i, p := range c.Providers {
		if p.Name == "" {
			return fmt.Errorf("providers[%d]: name 不能为空", i)
		}
		if seen[p.Name] {
			return fmt.Errorf("providers[%d]: name %q 重复", i, p.Name)
		}
		seen[p.Name] = true
		if p.BaseURL == "" {
			return fmt.Errorf("providers[%d] (%s): base_url 不能为空", i, p.Name)
		}
		if !strings.HasPrefix(p.BaseURL, "http://") && !strings.HasPrefix(p.BaseURL, "https://") {
			return fmt.Errorf("providers[%d] (%s): base_url 必须以 http:// 或 https:// 开头", i, p.Name)
		}
		if p.Protocol != ProtocolOpenAI {
			return fmt.Errorf("providers[%d] (%s): 暂不支持的 protocol %q（当前仅 %s）",
				i, p.Name, p.Protocol, ProtocolOpenAI)
		}
	}
	return nil
}

// ProviderForModel 按模型名选 provider：先精确匹配 models 列表（大小写不敏感），
// 再回落首个 models 为空的兜底 provider；都不命中返回 nil（调用方回 404）。
func (c *Config) ProviderForModel(model string) *Provider {
	if c == nil || !c.Enabled {
		return nil
	}
	m := strings.ToLower(strings.TrimSpace(model))
	var fallback *Provider
	for i := range c.Providers {
		p := &c.Providers[i]
		if len(p.Models) == 0 {
			if fallback == nil {
				fallback = p
			}
			continue
		}
		for _, name := range p.Models {
			if strings.ToLower(strings.TrimSpace(name)) == m {
				return p
			}
		}
	}
	return fallback
}

// AllModels 汇总所有 provider 声明的模型名（去重，保持声明顺序）。
// 兜底 provider（models 为空）不贡献条目——它承接的是任意模型名，
// 无法在 /tp/v1/models 里枚举（面板可另显示其名称作为提示）。
func (c *Config) AllModels() []string {
	if c == nil {
		return nil
	}
	seen := make(map[string]bool)
	out := make([]string, 0)
	for _, p := range c.Providers {
		for _, m := range p.Models {
			m = strings.TrimSpace(m)
			if m == "" || seen[strings.ToLower(m)] {
				continue
			}
			seen[strings.ToLower(m)] = true
			out = append(out, m)
		}
	}
	return out
}

// ChatURL 拼出 chat 端点：base 以 /v1 结尾时直接拼 chat_path，否则补 /v1。
// 两种用户写法（https://host/v1 与 https://host）都能落到正确端点。
//
// ChatPath 为空时回落缺省值——不依赖调用方先跑 normalize：直接构造 Config 的
// 代码路径（测试、面板热改）漏掉缺省补齐时，空 path 会拼出 base+"/" 这种
// 指向站点根的错误端点，表现为上游 404 而极难定位。
func (p *Provider) ChatURL() string {
	path := p.ChatPath
	if path == "" {
		path = defaultChatPath
	}
	return joinAPIPath(p.BaseURL, path)
}

// ModelsURL 拼出模型列表端点，规则同 ChatURL。
func (p *Provider) ModelsURL() string {
	path := p.ModelsPath
	if path == "" {
		path = defaultModelsPath
	}
	return joinAPIPath(p.BaseURL, path)
}

// joinAPIPath 把 path 拼到 base 上：base 已含 /v1（或任意以版本段结尾的路径）时
// 直接拼接，否则补一层 /v1。判定用"base 最后一段是否形如 v<数字>"，
// 兼容 /v1、/v1beta、/openai/v1 等形态。
func joinAPIPath(base, path string) string {
	if base == "" {
		return path
	}
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	if hasVersionSuffix(base) {
		return base + path
	}
	return base + "/v1" + path
}

// hasVersionSuffix 判断 URL 路径最后一段是否为版本段（v1 / v2 / v1beta …）。
func hasVersionSuffix(base string) bool {
	trimmed := strings.TrimRight(base, "/")
	idx := strings.LastIndex(trimmed, "/")
	if idx < 0 {
		return false
	}
	seg := trimmed[idx+1:]
	if len(seg) < 2 || seg[0] != 'v' && seg[0] != 'V' {
		return false
	}
	for _, r := range seg[1:] {
		if r < '0' || r > '9' {
			// v1beta 这类：数字后跟字母，仍算版本段。
			if r >= 'a' && r <= 'z' {
				continue
			}
			return false
		}
	}
	return true
}

// LogSummary 启动时打印一行摘要，便于运维确认实际生效的第三方配置。
func (c *Config) LogSummary() {
	if c == nil || !c.Enabled {
		return
	}
	for _, p := range c.Providers {
		models := "（兜底：承接未命中其他 provider 的模型）"
		if len(p.Models) > 0 {
			models = strings.Join(p.Models, ",")
		}
		log.Printf("[thirdparty] provider=%s protocol=%s chat=%s models=%s",
			p.Name, p.Protocol, p.ChatURL(), models)
	}
}
