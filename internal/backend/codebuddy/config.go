// Package codebuddy 的配置层：codebuddy.json。
//
// 为什么上游配置要独立成文件（而不是留在 config.json）：
// 主配置只该放**与上游无关**的东西（监听、密钥、账号池策略、排程）。一旦上游
// 特有的字段混在里面，加一个上游就要往主配置里长一段，而这段对另一个上游毫无
// 意义——久而久之 config.json 变成三个上游字段的大杂烩，用户分不清哪个字段属于
// 谁，删除上游时也不敢删字段（怕是别人的）。
//
// 一上游一文件后：
//   - 加上游 = 加一个文件，主配置一行不动；
//   - 删上游 = 删一个文件，不留死字段；
//   - 面板按文件分区展示，归属不会看错。
//
// 文件内**保留域维度**（cn / global）：域隔离是本项目的硬约束（见
// docs/architecture.md 的「域隔离失效」历史事故）。Trae 目前只有 cn，但它迟早
// 会有 global，结构上先留好位置，届时只加一段、不改形状。
package codebuddy

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// 内置默认值（上游协议常量，面板不暴露、也不需要用户知道）。
const (
	// DefaultChatBaseCN 国内版对话/目录 base。
	//
	// 注：copilot.tencent.com 对海外出口（Clash 全局 + TUN）的 TLS 握手直接复位，
	// CN 目录与对话全部 EOF；官方桌面端已改用 www.workbuddy.cn，该域海外可达，
	// /v2/chat/completions、/v3/config 与目录路径同构。
	DefaultChatBaseCN = "https://copilot.tencent.com"
	// DefaultChatBaseGlobal 国际版对话 base。
	DefaultChatBaseGlobal = "https://www.workbuddy.ai"
	// DefaultClientVersion 出站 UA 的 `WorkBuddy/<ver>` 段（对齐官方 5.5.4 分发包）。
	DefaultClientVersion = "5.5.4"
	// DefaultCliVersion 出站 UA 的 `CLI/<ver>` 段（对齐官方内置 CLI）。
	DefaultCliVersion = "2.137.1"
)

// Realm 单个上游域的配置。
//
// 每个域可以有独立的 base：CN 与 global 是两套完全不同的服务端点，共用一份
// base 会让「global 账号打到 CN 端点」这种情况无法表达，而跨域回退（issue #135）
// 恰恰依赖两域可分别寻址。
type Realm struct {
	// ChatBase 该域对话/模型目录 base 覆盖；空 = 回落内置默认。
	ChatBase string `json:"chat_base"`
	// BillingBase 该域计费/签到 base 覆盖；空 = 回落内置默认。
	BillingBase string `json:"billing_base"`
}

// Config codebuddy.json 结构。
//
// 分层原则：
//   - cn / global 两段：**域特有**的东西（base、是否启用该域）；
//   - 顶层：**两域共用**的传输层与身份项（超时、UA、版本、设备令牌）。
//
// 超时/UA 这类为什么要共用而不是每域一份：它们是**同一个 HTTP client** 的属性。
// 按域各配一份会让「global 请求用 120s、CN 请求用 300s」成为一种看似可配、
// 实则要维护两套 transport 的能力，收益为零、复杂度翻倍。
type Config struct {
	// CN 国内域。
	CN Realm `json:"cn"`
	// Global 国际域。
	Global GlobalRealm `json:"global"`

	// 注意：**超时参数（timeout/header/idle）不在这里**。
	//
	// 它们属于传输层，所有上游共用同一套语义（每个上游的对话都是 SSE 长连接，
	// 都要约束「首字节前」和「流中空闲」），因此放在 config.json 的 upstream 段。
	// 放进本文件会诱导「CodeBuddy 能配、Trae 不能」的错误认知——而 Trae 的对话
	// 同样是 SSE，两个超时对它同样必要。

	// ---- 两域共用的客户端身份 ----

	// UserAgent 出站 User-Agent 显式覆盖（非空时全路径生效，优先于默认三段式）。
	// 全部出站请求生效：chat/refresh/checkin/balance/report/travel/FetchModels。
	UserAgent string `json:"user_agent"`
	// ClientVersion 出站 UA 的 `WorkBuddy/<ver>` 与归属头 X-IDE-Version。
	ClientVersion string `json:"client_version"`
	// CliVersion 出站 UA 的 `CLI/<ver>` 段。
	CliVersion string `json:"cli_version"`
	// ClientName 用量归属头取值（X-Product / X-IDE-Name / X-IDE-Type / X-IDE-Version）。
	// 空 = 旧行为 X-Product="SaaS" 不设 X-IDE-*；配 "WorkBuddy" 则四头跟随。
	ClientName string `json:"client_name"`
	// DeviceToken 设备风控 Token（X-Device-Token）全局兜底；空 = 不注入。
	// 每号 auth 文件的 device_token 键优先于本项。
	DeviceToken string `json:"device_token"`
	// DeviceTokenFile device token 文件路径兜底（宿主落盘的桌面端 token）。
	DeviceTokenFile string `json:"device_token_file"`
	// PassthroughIP 是否透传客户端 IP 给上游（默认 false，反代安全边界）。
	PassthroughIP bool `json:"passthrough_ip"`

	// ---- 上游专有能力开关 ----

	// SanitizeBlacklistFingerprints 出站请求体黑名单指纹脱敏（默认 true）。
	//
	// 这是 **CodeBuddy 专有**的概念：上游按逐字精确匹配拦截客户端模板句
	// （"You are Claude Code" 等），改一字即可绕过。Trae 没有这套审核，
	// 所以这个开关属于本文件而不是主配置——按「上游专有配置独立成文件」原则。
	SanitizeBlacklistFingerprints bool `json:"sanitize_blacklist_fingerprints"`
}

// GlobalRealm 国际域配置（比 CN 多一个启用开关）。
//
// Enabled 放在 global 段内而不是顶层：它是**域开关**（是否路由到 global），
// 不是「上游开关」。上游平权后不存在「启用 CodeBuddy」这回事——有账号即启用。
type GlobalRealm struct {
	Realm
	// Enabled global realm 路由开关。缺省 true。
	//
	// 显式 false = 逃生门（纯 CN 锁定）：即便 auth 写了 realm=global 也不路由，
	// 这是 auth.Realm() 双保险的第一道闸。纯 CN 部署行为不变。
	Enabled bool `json:"enabled"`
}

// Default 内置默认配置。
func Default() *Config {
	return &Config{
		Global:                        GlobalRealm{Enabled: true},
		SanitizeBlacklistFingerprints: true,
	}
}

// Normalize 补齐缺省值。幂等。
func (c *Config) Normalize() {
	c.CN.ChatBase = strings.TrimRight(strings.TrimSpace(c.CN.ChatBase), "/")
	c.CN.BillingBase = strings.TrimRight(strings.TrimSpace(c.CN.BillingBase), "/")
	c.Global.ChatBase = strings.TrimRight(strings.TrimSpace(c.Global.ChatBase), "/")
	c.Global.BillingBase = strings.TrimRight(strings.TrimSpace(c.Global.BillingBase), "/")
}

// Validate 校验并 Normalize。
func (c *Config) Validate() error {
	c.Normalize()
	for name, base := range map[string]string{
		"cn.chat_base":        c.CN.ChatBase,
		"cn.billing_base":     c.CN.BillingBase,
		"global.chat_base":    c.Global.ChatBase,
		"global.billing_base": c.Global.BillingBase,
	} {
		if base == "" {
			continue // 空 = 用内置默认，合法
		}
		if !strings.HasPrefix(base, "http://") && !strings.HasPrefix(base, "https://") {
			return fmt.Errorf("%s 必须以 http:// 或 https:// 开头（得到 %q）", name, base)
		}
	}
	return nil
}

// ChatBaseFor 取某域的对话 base（空则回落内置默认）。
func (c *Config) ChatBaseFor(realm string) string {
	if c == nil {
		return DefaultChatBaseCN
	}
	if strings.EqualFold(realm, "global") {
		if c.Global.ChatBase != "" {
			return c.Global.ChatBase
		}
		return DefaultChatBaseGlobal
	}
	if c.CN.ChatBase != "" {
		return c.CN.ChatBase
	}
	return DefaultChatBaseCN
}

// BillingBaseFor 取某域的计费 base（空则回落该域 chat base）。
func (c *Config) BillingBaseFor(realm string) string {
	if c == nil {
		return DefaultChatBaseCN
	}
	if strings.EqualFold(realm, "global") {
		if c.Global.BillingBase != "" {
			return c.Global.BillingBase
		}
		if c.Global.ChatBase != "" {
			return c.Global.ChatBase
		}
		return DefaultChatBaseGlobal
	}
	if c.CN.BillingBase != "" {
		return c.CN.BillingBase
	}
	if c.CN.ChatBase != "" {
		return c.CN.ChatBase
	}
	return DefaultChatBaseCN
}

// Path 返回与 config.json 同目录的 codebuddy.json 路径。
func Path(configPath string) string {
	return filepath.Join(filepath.Dir(configPath), "codebuddy.json")
}

// Load 读取配置。文件不存在返回 (Default(), nil)。
//
// 为什么缺失时返回**默认**而不是 nil：主配置 config.json 缺失会自动生成，
// 上游配置同理——第一版只有一个上游文件（trae），用户升级后没有 codebuddy.json
// 属于正常情况，不该因此判定"没有 CodeBuddy 上游"。它总在内置默认值下可用。
func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return Default(), nil
		}
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	c := Default() // 以默认值为基线：文件里缺失的键保留默认
	if err := unmarshalInto(raw, c); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if err := c.Validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return c, nil
}

// Save 原子写盘（权限 0600：含 device_token 等敏感项）。
func Save(path string, c *Config) error {
	if err := c.Validate(); err != nil {
		return err
	}
	out, err := marshalIndent(c)
	if err != nil {
		return err
	}
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("mkdir %s: %w", dir, err)
		}
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, out, 0o600); err != nil {
		return fmt.Errorf("write %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("replace %s: %w", path, err)
	}
	return nil
}

// unmarshalInto 把 JSON 覆盖到已有配置上（缺失键保留现值）。
func unmarshalInto(raw []byte, c *Config) error {
	return json.Unmarshal(raw, c)
}

// marshalIndent 序列化（缩进 2 空格，尾随换行）。
func marshalIndent(c *Config) ([]byte, error) {
	out, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshal config: %w", err)
	}
	return append(out, '\n'), nil
}
