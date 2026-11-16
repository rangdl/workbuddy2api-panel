// Package trae 实现 Trae（trae.cn / trae.com.cn）上游接入。
//
// 与 internal/upstream（CodeBuddy）并列的上游 provider（第三方上游已移除）。
// 对话经 /v1/* 统一调度（模型名带 "trae:" 前缀指定），
// 签到/积分等运维能力由面板直接调用本包。
//
// 协议要点（蓝本：TraeWorkAssistant 的 api_server/，2026-09 实测结论）：
//   - 对话端点 POST {chat_base}/api/agent/v3/llm_utils_chat，**明文 JSON 可行**
//     （TTNet/aha 传输层加密仅对 create_agent_task 等富上下文端点强制）；
//   - 鉴权 Authorization: Cloud-IDE-JWT <token> + X-Cloudide-Token / X-Ide-Token /
//     X-App-Id / X-Device-Id 等头；
//   - 响应是**自定义 SSE**（event: metadata / timing_cost / output / extra_info /
//     token_usage / done / error），不是标准 OpenAI SSE，需转换；
//   - token 续期（ExchangeToken）在 refreshToken 场景要求 DeviceProof 签名
//     （ECDSA P-256 + SHA-256，签名原文 5 个 \n 连接），私钥来自客户端本地文件。
package trae

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
)

// 上游常量（与 TraeWorkAssistant 的 api_server/mod.rs 同源，2026-09 实测值）。
const (
	// DefaultChatBase 国内版对话上游（通用积分 product_id 208）。
	DefaultChatBase = "https://trae-api-cn.mchost.guru"
	// DefaultChatBaseGlobal 国际版对话上游。
	//
	// 国际版两个 App（Trae 桌面 / TRAE SOLO）**共用**这一个网关，它是国内
	// mchost.guru + solo.trae.cn 的 SG 合并等价物。SG 侧还有分片
	// （api16/api22-normal-alisg.mchost.guru）但内容相同，统一走 coresg。
	DefaultChatBaseGlobal = "https://coresg-normal.trae.ai"
	// DefaultAPITraeBaseGlobal 国际版签到/用量接口的缺省 base。
	//
	// **注意**：国际版的用量接口挂在**凭证自带的 host** 上（实测有
	// api-sg-central / growsg-normal 两种），所以这个只是无 host 信息时的
	// 缺省值；有 host 时优先用 host。
	DefaultAPITraeBaseGlobal = "https://growsg-normal.trae.ai"
	// EndpointLLMChat 核心对话端点（HTTP + SSE）。
	EndpointLLMChat = "/api/agent/v3/llm_utils_chat"
	// EndpointGetDetailParam 模型列表端点。
	EndpointGetDetailParam = "/api/ide/v1/get_detail_param"

	// DefaultIDEVersion / DefaultIDEVersionCode 出站 X-Ide-Version 等头。
	DefaultIDEVersion     = "0.1.50"
	DefaultIDEVersionCode = "20260811"
	// DefaultAppID 客户端应用 id（X-App-Id）。
	DefaultAppID = "6eefa01c-1036-4c7e-9ca5-d891f63bfcd8"

	// DefaultClientID / DefaultClientSecret OAuth 公开常量（授权页与 ExchangeToken 用）。
	//
	// **桌面包**的 ClientID。SOLO 包是另一个（DefaultClientIDSolo）——这个分叉
	// 属于 OAuth client，不属于 endpoint 或域，见 docs/trae-intl.md §6。
	DefaultClientID     = "ono9krqynydwx5"
	DefaultClientSecret = "-"
	// DefaultClientIDSolo SOLO 包（含 SOLO CN 与 SOLO 国际）的 ClientID。
	//
	// 实测（2026-10-11）：用桌面 ClientID 给 SOLO 账号续期会被回
	// `10101 无效参数`；而 SOLO 包用它对旧 /cloudide 路径也有效。历史事故：
	// 早期实现在旧路径上写死桌面 id，导致国际 SOLO 账号一旦过期就刷不回来。
	DefaultClientIDSolo = "en1oxy7wnw8j9n"

	// DefaultWorkspaceID llm_utils_chat 请求体的固定字段（实测值）。
	DefaultWorkspaceID = "e04cdd"
	// DefaultPromptMaxTokens 请求体 prompt_max_tokens（实测值）。
	DefaultPromptMaxTokens = 168000
	// DefaultMaxTokens 客户端未给 max_tokens 时的兜底（上游必需字段）。
	DefaultMaxTokens = 4096

	// DefaultTimeoutSeconds 非流式请求总超时。
	DefaultTimeoutSeconds = 120
	// MaxRotate 单请求最多换号次数。
	MaxRotate = 3
)

// TraeRealm 单个域的配置（与 codebuddy 的 Realm 同构）。
//
// 两个字段都是 base 覆盖：空 = 回落内置默认。Trae 的对话域与签到域是**不同域名**
// （mchost.guru vs api.trae.cn），不能合成一个 base。
type TraeRealm struct {
	// ChatBase 对话上游 base；空 = 该域的内置默认。
	ChatBase string `json:"chat_base"`
	// APIBase 签到/积分接口 base；空 = 该域的内置默认。
	APIBase string `json:"api_base"`
}

// GlobalRealm 国际域配置（比 CN 多一个启用开关）。
//
// Enabled 放在 global 段内而不是顶层：它是**域开关**（是否路由到 global），
// 不是「上游开关」。与 codebuddy 的 GlobalRealm 同名字段同语义。
type GlobalRealm struct {
	TraeRealm
	// Enabled global 域路由开关。缺省 true。
	//
	// 显式 false = 逃生门（纯 CN 锁定）：即便账号凭证声明的是国际版，
	// 也不路由到 global 域。纯 CN 部署行为不变。
	//
	// **用指针**：codebuddy 那边是 bool + Default() 里显式置 true，但 Trae 的
	// 默认配置是空对象（configs.go 写 `{}`），bool 零值 false 会让升级后所有
	// 国际账号静默停用。指针能区分「没写」与「显式 false」，缺省按 true 处理。
	Enabled *bool `json:"enabled,omitempty"`
}

// GlobalEnabled 报告 global 域是否允许路由（未配置视为 true）。
func (c *Config) GlobalEnabled() bool {
	return c == nil || c.Global.Enabled == nil || *c.Global.Enabled
}

// Config trae.json 结构。
type Config struct {
	// Enabled 总开关；false 时该上游不参与调度。
	Enabled bool `json:"enabled"`
	// CN 国内域配置。
	//
	// **域维度是刻意的**：CodeBuddy 已有 cn/global 两域，Trae 与国际版对齐后
	// 同样是两域。形状与 codebuddy.json 的 cn/global 两段一致。
	CN TraeRealm `json:"cn"`
	// Global 国际域配置（Trae 国际版 = trae.ai）。
	//
	// 比 CN 多一个 Enabled 域开关，形状与 codebuddy.json 的 global 段一致。
	// 它是**域开关**（是否允许路由到 global），不是「启用 Trae 上游」——
	// 上游平权后不存在「启用某个上游」这回事，有账号即参与调度。
	Global GlobalRealm `json:"global"`

	// ChatBase 对话上游 base（缺省 DefaultChatBase）。
	//
	// **兼容字段**：已并入 CN.ChatBase。保留是因为旧 trae.json 都写在这一层，
	// Load 时若发现顶层有值而 CN.ChatBase 为空，会自动搬进 CN 段（见 Normalize）。
	// 新写入只写 CN 段。
	ChatBase string `json:"chat_base,omitempty"`
	// APITraeBase 签到/积分接口 base（缺省 DefaultAPITraeBase = api.trae.cn）。
	//
	// **兼容字段**：同 ChatBase，已并入 CN.APIBase。与 chat_base 不同域：
	// 对话走 mchost.guru，签到/积分走 api.trae.cn。
	APITraeBase string `json:"api_trae_base,omitempty"`
	// IDEVersion / IDEVersionCode / AppID 出站头覆盖（缺省用内置默认）。
	IDEVersion     string `json:"ide_version"`
	IDEVersionCode string `json:"ide_version_code"`
	AppID          string `json:"app_id"`
	// TimeoutSeconds 非流式总超时；流式只约束首字节。
	TimeoutSeconds int `json:"timeout_seconds"`
	// AuthDir Trae 账号凭证目录（缺省取 trae.json 同级的 auths/）。
	// 一账号一文件：trae-<uid>.json（见 accounts.go）。
	AuthDir string `json:"auth_dir"`
	// OAuthClientID / OAuthClientSecret 覆盖内置公开常量（上游更换凭证时免发版）。
	OAuthClientID     string `json:"oauth_client_id"`
	OAuthClientSecret string `json:"oauth_client_secret"`
	// OAuthHost ExchangeToken 的 API 域（缺省 api.trae.com.cn；授权页回传的 host 优先）。
	OAuthHost string `json:"oauth_host"`
	// OAuthCallbackURL 授权回调地址（缺省本机回环 http://127.0.0.1:17388/authorize）。
	// 服务端部署时可改为公网回调端点，或保持缺省并在面板里手工粘贴回调 URL。
	OAuthCallbackURL string `json:"oauth_callback_url"`
	// Accounts 账号列表。
	//
	// **不落盘到 trae.json**（`json:"-"`）：账号存在 auth_dir 下的一账号一文件里，
	// 由 AuthDir 指向（见 accounts.go）。这里保留字段是为了让运行期代码
	// 继续按 `cfg.Accounts` 访问，Load 时从目录装配。
	Accounts []Account `json:"-"`
	// Schedule 签到排程的**遗留字段**，已不参与调度。
	//
	// 签到统一由 internal/scheduler 按整点触发，开关在 config.json 的
	// schedule.checkin_backends（按上游）+ schedule.checkin_enabled（总开关）。
	// 保留字段只为让旧 trae.json 仍能正常反序列化（未知字段会被静默忽略，
	// 但显式保留能避免「字段消失 → 用户以为配置被吃掉」的困惑）。
	//
	// 不要在这里读 Enabled/Hours 做任何决策：那会让「同一个签到有两个开关」
	// 的分裂状态复活。见 docs/architecture.md 的排程单点。
	Schedule CheckinSchedule `json:"schedule,omitempty"`

	// ---- 签到链路可调项（全部有安全缺省，一般不需要动）----

	// VerifyClaim claim 成功后是否同设备号回查确认（缺省 true）。
	//
	// 背景：claim 返回 code=0 可能是"假成功"——当日已签过的账号对任何
	// device_id 都回 0（L0NE-6 实测）。开着它多一次只读请求，但能避免
	// "面板亮绿灯、实际没到账"。用指针区分"未配置"与显式 false。
	VerifyClaim *bool `json:"verify_claim"`
	// RefreshBeforeCheckin 签到前是否先按需续期 access_token（缺省 true）。
	//
	// 签到链路此前完全没有续期入口：token 过期会被分类成 ErrSessionDead
	// 直接摘号 24h，而实际上刷新一下就能签。开着它会先看 NeedsRefresh，
	// 需要时走 ExchangeRefresh 并立刻把轮换后的 refreshToken 落盘。
	RefreshBeforeCheckin *bool `json:"refresh_before_checkin"`
	// CheckinUserAgent 签到/积分接口的 UA（缺省 Trae/0.1.52，新版合并客户端形态）。
	// 需要回退旧版形态时填 LegacyCheckinUserAgent（"VSCode 1.107.1 (TRAE SOLO CN)"）。
	CheckinUserAgent string `json:"checkin_user_agent"`
	// CheckinReqSource 签到请求体的 req_source（缺省 0 = 发 `{}`）。
	// 参考实现互相矛盾（1 / 2），故可配置；发 `{}` 是本项目已实测可用的形态。
	CheckinReqSource int `json:"checkin_req_source"`

	// SaveAccount 续期成功后把**单个账号**落盘的钩子（由启动方注入，不参与 JSON）。
	//
	// 为什么需要：Trae 的 refreshToken 是**轮换链**——每次续期产生新值、旧值作废。
	// 若只在内存里更新，进程重启后拿旧值续期会直接失败（参考实现都强调这点）。
	// trae 包不直接写文件，是为了不把"目录在哪、什么权限"的知识搬进 provider。
	SaveAccount saveAccountFunc `json:"-"`
}

// Account 一个 Trae 账号。
//
// 与 auth.Auth（CodeBuddy）的关键差异：Trae 的 token 续期需要设备私钥签名，
// 故凭证里多出 DeviceID / MachineID / PrivateKeyPEM 三项。
type Account struct {
	// UID 账号唯一标识（JWT payload 的 user_id）。
	UID string `json:"uid"`
	// Name 展示名（可空）。
	Name string `json:"name"`
	// AccessToken 形如 "Cloud-IDE-JWT eyJ..." 或裸 JWT（发送时统一补前缀）。
	AccessToken string `json:"access_token"`
	// RefreshToken 续期凭据（ExchangeToken 用）。
	RefreshToken string `json:"refresh_token"`
	// ExpiresAt access token 过期时间（Unix 秒；0 = 未知）。
	ExpiresAt int64 `json:"expires_at"`
	// DeviceID / MachineID 设备标识（DeviceProof 签名与出站头用）。
	DeviceID  string `json:"device_id"`
	MachineID string `json:"machine_id"`
	// PrivateKeyPEM 设备 EC P-256 私钥（PEM）。空 = 无 DeviceProof 能力，
	// refresh 只能走无 proof 的兜底变体（上游可能拒绝）。
	PrivateKeyPEM string `json:"private_key_pem"`
	// PublicKeyPEM 设备 EC P-256 公钥（PEM）。
	//
	// AuthCode 兑换只需要公钥（服务端校验 DeviceInfo.DevicePublicKey 与登录时
	// 登记的指纹一致），不需要私钥——所以它要能独立于私钥存在。
	PublicKeyPEM string `json:"public_key_pem,omitempty"`
	// AppVersion 客户端版本（DeviceInfo.ClientVersion）。
	AppVersion string `json:"app_version"`
	// Host 凭证自带的 API host（如 https://api.trae.cn / https://growsg-normal.trae.ai）。
	//
	// 域判定与「国际版用量接口打哪个 host」都要用它。老账号文件没有这个字段，
	// 此时 Region() 回落到配置 base 判定（零回归）。
	Host string `json:"host,omitempty"`
	// UserRegion 凭证的 userRegion 声明（"CN" / "SG" / "US" 等原始字符串）。
	//
	// 两种实测形态都可能被写进来：storage.json 里是对象
	// {"region":"CN","_aiRegion":"CN"}，App 日志里是裸小写 "sg"。
	// 判定在 RegionOfUserRegion 里做（大小写不敏感），这里只存原始值。
	UserRegion string `json:"user_region,omitempty"`
	// PackageType 客户端包类型（"solo" = SOLO 包，空 = 桌面包）。
	//
	// 只影响**续期用哪个 OAuth ClientID**：SOLO 包必须用
	// DefaultClientIDSolo，否则上游回 10101（"无效参数"），而那个报错
	// 完全不指向 ClientID，极难排查。
	PackageType string `json:"package_type,omitempty"`
	// Disabled 手动禁用（保留在列表但不参与轮转）。
	Disabled bool `json:"disabled"`
	// DisabledReason 禁用原因（展示用）。
	DisabledReason string `json:"disabled_reason"`
}

// Normalize 补齐缺省值。
func (c *Config) Normalize() {
	// 旧字段 → 域段的一次性搬迁（内存内，不改磁盘）。
	// 只在 CN 段为空时搬，避免覆盖用户已经写在新结构里的值。
	if strings.TrimSpace(c.CN.ChatBase) == "" && strings.TrimSpace(c.ChatBase) != "" {
		c.CN.ChatBase = c.ChatBase
	}
	if strings.TrimSpace(c.CN.APIBase) == "" && strings.TrimSpace(c.APITraeBase) != "" {
		c.CN.APIBase = c.APITraeBase
	}
	if strings.TrimSpace(c.CN.ChatBase) == "" {
		c.CN.ChatBase = DefaultChatBase
	}
	c.CN.ChatBase = strings.TrimRight(strings.TrimSpace(c.CN.ChatBase), "/")
	if strings.TrimSpace(c.CN.APIBase) == "" {
		c.CN.APIBase = DefaultAPITraeBase
	}
	c.CN.APIBase = strings.TrimRight(strings.TrimSpace(c.CN.APIBase), "/")
	// global 段：空 = 用内置默认（与该域 CN 段同口径）。
	c.Global.ChatBase = strings.TrimRight(strings.TrimSpace(c.Global.ChatBase), "/")
	c.Global.APIBase = strings.TrimRight(strings.TrimSpace(c.Global.APIBase), "/")
	if c.Global.ChatBase == "" {
		c.Global.ChatBase = DefaultChatBaseGlobal
	}
	if c.Global.APIBase == "" {
		c.Global.APIBase = DefaultAPITraeBaseGlobal
	}
	// Enabled 用指针，缺省在 GlobalEnabled() 里按 true 处理（零值 false
	// 会让升级后所有国际账号静默停用）。这里不写它。
	// 反向同步：让仍在读顶层字段的老代码路径拿到生效值（读取方逐步迁移到
	// ChatBaseFor/APIBaseFor，在两个都改完之前这层映射保证不出现两套值）。
	c.ChatBase = c.CN.ChatBase
	c.APITraeBase = c.CN.APIBase
	if c.IDEVersion == "" {
		c.IDEVersion = DefaultIDEVersion
	}
	if c.IDEVersionCode == "" {
		c.IDEVersionCode = DefaultIDEVersionCode
	}
	// 版本码必须是纯数字：上游把 x-app-version-code / x-ide-version-code 绑定为
	// 数字类型，填点分串（如 "0.1.52"）会被回 `4001 parameter type does not
	// match binding data`，且报错完全不指向版本头，极难排查（dsh 实测）。
	if !isAllDigits(c.IDEVersionCode) {
		log.Printf("WARN: [trae] ide_version_code = %q 不是纯数字，"+
			"上游会回 4001 parameter type does not match binding data；已回退到内置值 %s",
			c.IDEVersionCode, DefaultIDEVersionCode)
		c.IDEVersionCode = DefaultIDEVersionCode
	}
	if c.AppID == "" {
		c.AppID = DefaultAppID
	}
	if c.TimeoutSeconds <= 0 {
		c.TimeoutSeconds = DefaultTimeoutSeconds
	}
	// 签到排程缺省值：时点 [9]，账号间隔 800ms（防连打触发风控）。
	if len(c.Schedule.Hours) == 0 {
		c.Schedule.Hours = []int{9}
	}
	if c.Schedule.GapMS <= 0 {
		c.Schedule.GapMS = 800
	}
	// 签到链路的两个布尔开关缺省为 true（用指针区分"未配置"与显式 false）。
	if c.VerifyClaim == nil {
		c.VerifyClaim = boolPtr(true)
	}
	if c.RefreshBeforeCheckin == nil {
		c.RefreshBeforeCheckin = boolPtr(true)
	}
	c.CheckinUserAgent = strings.TrimSpace(c.CheckinUserAgent)
	for i := range c.Accounts {
		c.Accounts[i].normalize()
	}
}

// VerifyClaimEnabled / RefreshBeforeCheckinEnabled 读布尔开关（nil 视为 true）。
func (c *Config) VerifyClaimEnabled() bool {
	return c == nil || c.VerifyClaim == nil || *c.VerifyClaim
}

// RefreshBeforeCheckinEnabled 签到前是否先续期（nil 视为 true）。
func (c *Config) RefreshBeforeCheckinEnabled() bool {
	return c == nil || c.RefreshBeforeCheckin == nil || *c.RefreshBeforeCheckin
}

func (a *Account) normalize() {
	a.UID = strings.TrimSpace(a.UID)
	a.Name = strings.TrimSpace(a.Name)
	a.AccessToken = strings.TrimSpace(a.AccessToken)
	a.RefreshToken = strings.TrimSpace(a.RefreshToken)
	a.DeviceID = strings.TrimSpace(a.DeviceID)
	a.MachineID = strings.TrimSpace(a.MachineID)
	a.PrivateKeyPEM = strings.TrimSpace(a.PrivateKeyPEM)
	a.AppVersion = strings.TrimSpace(a.AppVersion)
	a.Host = strings.TrimRight(strings.TrimSpace(a.Host), "/")
	a.UserRegion = strings.TrimSpace(a.UserRegion)
}

// Region 判定该账号所属的服务区域。
//
// 判定顺序（与 docs/trae-intl.md §2 一致）：
//
//	账号的 userRegion 声明  →  账号的 host  →  配置里该域是否启用
//
// 前两级来自账号自身的凭证，是权威判据。都缺失时才回落 cn——这是改造前的
// 行为（老账号文件没有那两个字段），保留它是为了零回归。
//
// cfg 用于读取 global 域的启用开关；传 nil 表示不设逃生门（视为 enabled）。
func (a *Account) Region(cfg *Config) ServiceRegion {
	if a == nil {
		return RegionCN
	}
	if r, ok := RegionOfCredential(a.Host, a.UserRegion); ok {
		return r
	}
	return RegionCN
}

// RegionOf 同 Region，但额外应用 global 域的逃生门。
//
// 逃生门（cfg.Global.Enabled=false）把国际账号降级成 cn 而不是丢弃：
// 纯 CN 部署下它们会像改造前一样走国内网关（多数会 401，但那是有账号
// 但锁了域时的既有行为，比静默摘号更容易排查）。
func (a *Account) RegionOf(cfg *Config) ServiceRegion {
	r := a.Region(cfg)
	if r == RegionGlobal && !cfg.GlobalEnabled() {
		return RegionCN
	}
	return r
}

// Validate 校验配置自洽性（与 Load 同口径，供面板保存前调用）。
func (c *Config) Validate() error {
	c.Normalize()
	if !strings.HasPrefix(c.ChatBase, "http://") && !strings.HasPrefix(c.ChatBase, "https://") {
		return fmt.Errorf("chat_base 必须以 http:// 或 https:// 开头")
	}
	// global 段的两条 base 同样校验：它们与 CN 段是平行的网关入口，
	// 写坏了会在国际账号发起请求时才失败（且表现为连接错误）。
	for name, base := range map[string]string{
		"global.chat_base": c.Global.ChatBase,
		"global.api_base":  c.Global.APIBase,
	} {
		if base == "" {
			continue // 空 = 内置默认
		}
		if !strings.HasPrefix(base, "http://") && !strings.HasPrefix(base, "https://") {
			return fmt.Errorf("%s 必须以 http:// 或 https:// 开头（得到 %q）", name, base)
		}
	}
	seen := make(map[string]bool, len(c.Accounts))
	for i, a := range c.Accounts {
		if a.UID == "" {
			return fmt.Errorf("accounts[%d]: uid 不能为空", i)
		}
		if seen[a.UID] {
			return fmt.Errorf("accounts[%d]: uid %q 重复", i, a.UID)
		}
		seen[a.UID] = true
		if a.AccessToken == "" && a.RefreshToken == "" {
			return fmt.Errorf("accounts[%d] (%s): access_token 与 refresh_token 至少要有一个", i, a.UID)
		}
		if a.PrivateKeyPEM != "" {
			if _, err := ParseECPrivateKey(a.PrivateKeyPEM); err != nil {
				return fmt.Errorf("accounts[%d] (%s): 设备私钥无法解析: %w", i, a.UID, err)
			}
		}
	}
	return nil
}

// legacyAccounts 兼容旧版 trae.json 里内嵌的 accounts 数组（迁移用）。
//
// 旧版本把所有账号塞在 trae.json 的同名字段里。现在改为 auth_dir 下的一账号
// 一文件，由 MigrateLegacyAccounts 在启动时拆出去。这里用独立的解码目标
// 而不是给 Config.Accounts 加 tag，是为了让"从文件读"与"运行期装配"分开。
type legacyConfig struct {
	Accounts []Account `json:"accounts"`
}

// Load 读取 trae.json 与账号目录。
//
// 文件缺失返回 (nil, nil)——调用方据此判定"未启用 Trae"。
//
// 账号目录优先级：trae.json 的 auth_dir > defaultAuthDir 参数。
// 传空则回落到 DefaultAuthDir（trae.json 同级），但生产路径**必须**传主配置
// 的 auth_dir——否则会去 config/auths/ 找账号，而 workbuddy 账号在根目录
// auths/ 下，两边分家。
//
// 注意：本函数**不迁移**旧版内嵌账号，迁移在 MigrateLegacyAccounts 里显式做，
// 免得"每分钟都在写的调度器读路径"顺手改磁盘。
func Load(path, defaultAuthDir string) (*Config, error) {
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
	if strings.TrimSpace(c.AuthDir) == "" {
		if strings.TrimSpace(defaultAuthDir) != "" {
			c.AuthDir = defaultAuthDir
		} else {
			c.AuthDir = DefaultAuthDir(path)
		}
	}
	accounts, err := LoadAccounts(c.AuthDir)
	if err != nil {
		return nil, fmt.Errorf("加载账号目录 %s: %w", c.AuthDir, err)
	}
	c.Accounts = accounts
	if err := c.Validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &c, nil
}

// DefaultAuthDir 兜底账号目录（trae.json 同级）。
//
// 仅作**最后兜底**：标准布局里配置在 config/、账号在根 auths/，两者不同级，
// 所以生产路径应显式传主配置的 auth_dir 给 Load。
func DefaultAuthDir(configPath string) string {
	return filepath.Join(filepath.Dir(configPath), "auths")
}

// MigrateLegacyAccounts 把旧版 trae.json 内嵌的 accounts 拆成一账号一文件。
//
// 迁移是**一次性且幂等**的：写完全部账号文件后把 trae.json 的 accounts 清空。
// 单个账号写失败不影响其余账号，但只要还有没写成功的就不清空源数组——
// 宁可下次启动再试，也不能让账号凭空消失。
//
// 返回迁移的账号数（0 表示无需迁移）。
func MigrateLegacyAccounts(configPath, defaultAuthDir string) (int, error) {
	raw, err := os.ReadFile(configPath)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, fmt.Errorf("read %s: %w", configPath, err)
	}
	var legacy legacyConfig
	if err := json.Unmarshal(raw, &legacy); err != nil {
		return 0, fmt.Errorf("parse %s: %w", configPath, err)
	}
	if len(legacy.Accounts) == 0 {
		return 0, nil
	}

	// AuthDir 可能配在同一个文件里，先取出来；否则用调用方给的默认目录
	// （生产路径传主配置的 auth_dir，保证与 workbuddy 账号同目录）。
	var meta struct {
		AuthDir string `json:"auth_dir"`
	}
	_ = json.Unmarshal(raw, &meta)
	dir := strings.TrimSpace(meta.AuthDir)
	if dir == "" {
		if strings.TrimSpace(defaultAuthDir) != "" {
			dir = defaultAuthDir
		} else {
			dir = DefaultAuthDir(configPath)
		}
	}

	migrated := 0
	var failed []string
	for i := range legacy.Accounts {
		a := legacy.Accounts[i]
		a.normalize()
		if a.UID == "" {
			failed = append(failed, fmt.Sprintf("accounts[%d]: 缺少 uid", i))
			continue
		}
		if err := SaveAccount(dir, &a); err != nil {
			failed = append(failed, fmt.Sprintf("%s: %v", a.UID, err))
			continue
		}
		migrated++
	}
	if len(failed) > 0 {
		return migrated, fmt.Errorf("部分账号迁移失败（源配置保持不变，下次启动重试）: %s",
			strings.Join(failed, "; "))
	}

	// 全部成功才清空源数组，其余全局配置原样保留。
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(raw, &doc); err != nil {
		return migrated, fmt.Errorf("parse %s: %w", configPath, err)
	}
	delete(doc, "accounts")
	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return migrated, fmt.Errorf("序列化失败: %w", err)
	}
	out = append(out, '\n')
	tmp := configPath + ".tmp"
	if err := os.WriteFile(tmp, out, 0o600); err != nil {
		return migrated, fmt.Errorf("写入失败: %w", err)
	}
	if err := os.Rename(tmp, configPath); err != nil {
		return migrated, fmt.Errorf("原子替换失败: %w", err)
	}
	return migrated, nil
}

// chatBaseForAccount 该账号应使用的对话 base。
//
// 账号的域优先，配置里该域的 base 覆盖其次，内置默认兜底。
func (c *Config) chatBaseForAccount(a *Account) string {
	if a == nil {
		return c.ChatBaseFor(string(RegionCN))
	}
	return c.ChatBaseFor(string(a.RegionOf(c)))
}

// apiBaseForAccount 该账号应使用的签到/用量 base。
//
// 国际版的用量接口挂在**凭证自带的 host** 上（SG 侧有 api-sg-central 与
// growsg-normal 两种），所以账号 Host 优先于配置。国内版没有这个现象，
// Host 通常就是 api.trae.cn，等价。
func (c *Config) apiBaseForAccount(a *Account) string {
	if a == nil {
		return strings.TrimRight(c.APIBaseFor(string(RegionCN)), "/")
	}
	r := a.RegionOf(c)
	base := c.APIBaseFor(string(r))
	if r == RegionGlobal && strings.TrimSpace(a.Host) != "" {
		base = strings.TrimRight(strings.TrimSpace(a.Host), "/")
	}
	return strings.TrimRight(base, "/")
}

// ConfigPath 返回与 config.json 同目录的 trae.json 路径。
// ChatBaseFor 取某域的对话 base（空则回落该域内置默认）。
//
// 域参数**必须真正参与分派**：改造前这个函数忽略 realm 直接返 CN 的 base，
// 于是无论账号声明哪个域，国际账号都会被打到国内网关。国际版与国内版的
// 聊天网关是两个完全不同的域名（mchost.guru vs coresg-normal.trae.ai）。
//
// 未知域回落 cn：与 realm 枚举的缺省口径一致（gateway.DefaultRealm）。
func (c *Config) ChatBaseFor(realm string) string {
	if c == nil {
		return DefaultChatBase
	}
	if realm == string(RegionGlobal) {
		if c.Global.ChatBase != "" {
			return c.Global.ChatBase
		}
		return DefaultChatBaseGlobal
	}
	if c.CN.ChatBase != "" {
		return c.CN.ChatBase
	}
	// 兼容：未走 Normalize 的调用方（测试、PoC）只填顶层 chat_base，
	// 而顶层字段的语义历来就是 CN 的 base（Normalize 也是这么反向同步的）。
	if strings.TrimSpace(c.ChatBase) != "" {
		return strings.TrimRight(strings.TrimSpace(c.ChatBase), "/")
	}
	return DefaultChatBase
}

// APIBaseFor 取某域的签到/积分 base（空则回落该域内置默认）。
//
// 国际版的用量接口挂在**凭证自带 host** 上（SG 侧有 api-sg-central /
// growsg-normal 两种），所以调用方应优先用账号的 Host 覆盖这里的返回值。
func (c *Config) APIBaseFor(realm string) string {
	if c == nil {
		return DefaultAPITraeBase
	}
	if realm == string(RegionGlobal) {
		if c.Global.APIBase != "" {
			return c.Global.APIBase
		}
		return DefaultAPITraeBaseGlobal
	}
	if c.CN.APIBase != "" {
		return c.CN.APIBase
	}
	// 兼容：同 ChatBaseFor 的顶层字段兜底。
	if strings.TrimSpace(c.APITraeBase) != "" {
		return strings.TrimRight(strings.TrimSpace(c.APITraeBase), "/")
	}
	return DefaultAPITraeBase
}

func ConfigPath(configPath string) string {
	return filepath.Join(filepath.Dir(configPath), "trae.json")
}

// LogSummary 启动时打印一行摘要。
func (c *Config) LogSummary() {
	if c == nil {
		return
	}
	usable := 0
	proof := 0
	for _, a := range c.Accounts {
		if a.Disabled {
			continue
		}
		usable++
		if a.PrivateKeyPEM != "" {
			proof++
		}
	}
	log.Printf("[trae] 已启用：chat=%s 账号=%d（可用 %d，其中 %d 具备 DeviceProof 能力）",
		c.ChatBase, len(c.Accounts), usable, proof)
	if usable > 0 && proof == 0 {
		log.Printf("[trae] WARN: 无账号具备设备私钥，refresh 只能走无 proof 兜底变体（上游可能拒绝）")
	}
	// 设备号形态提示（只在启动/热重载打一次，不放进 Validate——调度器每分钟都读配置）。
	for _, a := range c.Accounts {
		if a.Disabled || a.DeviceID == "" || IsValidDeviceID(a.DeviceID) {
			continue
		}
		log.Printf("[trae] WARN: 账号 %s 的 device_id %q 不像客户端注册过的十进制设备号，"+
			"签到大概率返回 9074；请从 storage.json 的 iCubeAuthInfo://icube-dc:<数字> 键名重新导入",
			a.UID, a.DeviceID)
	}
	// 多账号共用同一设备号时，一天只有第一个能签成（9095 的主语是设备）。
	devOwner := make(map[string]string, len(c.Accounts))
	for _, a := range c.Accounts {
		if a.Disabled || a.DeviceID == "" {
			continue
		}
		if prev, ok := devOwner[a.DeviceID]; ok {
			log.Printf("[trae] WARN: 账号 %s 与 %s 共用设备号 %s——签到限额按设备算，"+
				"一天只有第一个能签成，其余会返回 9095", prev, a.UID, a.DeviceID)
			continue
		}
		devOwner[a.DeviceID] = a.UID
	}
}

// AccountByUID 按 uid 查账号（返回副本，避免调用方误改配置）。
func (c *Config) AccountByUID(uid string) *Account {
	if c == nil {
		return nil
	}
	for i := range c.Accounts {
		if c.Accounts[i].UID == uid {
			a := c.Accounts[i]
			return &a
		}
	}
	return nil
}

// AccessTokenHeader 返回可直接用于 Authorization 头的 token 值// （裸 JWT 自动补 "Cloud-IDE-JWT " 前缀）。
func (a *Account) AccessTokenHeader() string {
	t := strings.TrimSpace(a.AccessToken)
	if t == "" {
		return ""
	}
	if strings.HasPrefix(t, "Cloud-IDE-JWT ") {
		return t
	}
	return "Cloud-IDE-JWT " + t
}

// isAllDigits 报告字符串是否全为 ASCII 数字且非空。
//
// 用途：上游把版本码头绑定为数字类型，非纯数字会被 4001 拒绝且报错不指向该头。
func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}
