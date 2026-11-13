// Package trae 实现 Trae（trae.cn / trae.com.cn）上游接入。
//
// 与 internal/upstream（CodeBuddy）和 internal/thirdparty（OpenAI 兼容）并列的
// 第三个上游 provider，走独立前缀 /trae/v1/*，与现有链路互不影响。
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
	// DefaultChatBase 对话上游（通用积分 product_id 208）。
	DefaultChatBase = "https://trae-api-cn.mchost.guru"
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
	DefaultClientID     = "ono9krqynydwx5"
	DefaultClientSecret = "-"

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

// Config trae.json 结构。
type Config struct {
	// Enabled 总开关；false 时 /trae/v1/* 一律 404。
	Enabled bool `json:"enabled"`
	// ChatBase 对话上游 base（缺省 DefaultChatBase）。
	ChatBase string `json:"chat_base"`
	// APITraeBase 签到/积分接口 base（缺省 DefaultAPITraeBase = api.trae.cn）。
	// 与 chat_base 不同域：对话走 mchost.guru，签到/积分走 api.trae.cn。
	APITraeBase string `json:"api_trae_base"`
	// IDEVersion / IDEVersionCode / AppID 出站头覆盖（缺省用内置默认）。
	IDEVersion     string `json:"ide_version"`
	IDEVersionCode string `json:"ide_version_code"`
	AppID          string `json:"app_id"`
	// TimeoutSeconds 非流式总超时；流式只约束首字节。
	TimeoutSeconds int `json:"timeout_seconds"`
	// AuthDir Trae 账号凭证目录（缺省 ./auths，与 CodeBuddy 共用目录但文件名前缀区分）。
	AuthDir string `json:"auth_dir"`
	// OAuthClientID / OAuthClientSecret 覆盖内置公开常量（上游更换凭证时免发版）。
	OAuthClientID     string `json:"oauth_client_id"`
	OAuthClientSecret string `json:"oauth_client_secret"`
	// OAuthHost ExchangeToken 的 API 域（缺省 api.trae.com.cn；授权页回传的 host 优先）。
	OAuthHost string `json:"oauth_host"`
	// OAuthCallbackURL 授权回调地址（缺省本机回环 http://127.0.0.1:17388/authorize）。
	// 服务端部署时可改为公网回调端点，或保持缺省并在面板里手工粘贴回调 URL。
	OAuthCallbackURL string `json:"oauth_callback_url"`
	// Accounts 账号列表。凭证字段（access_token / refresh_token / 设备私钥）
	// 建议通过面板导入而非手写在本文件里——本文件不加密。
	Accounts []Account `json:"accounts"`
	// Schedule 签到排程（缺省不启用；由 trae.Scheduler 消费）。
	Schedule CheckinSchedule `json:"schedule"`

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

	// SaveHook 续期成功后把轮换后的凭证落盘的钩子（由启动方注入，不参与 JSON）。
	//
	// 为什么需要：Trae 的 refreshToken 是**轮换链**——每次续期产生新值、旧值作废。
	// 若只在内存里更新，进程重启后拿旧值续期会直接失败（参考实现都强调这点）。
	// trae 包不直接写配置文件，是为了不把"哪个文件、什么权限"的知识搬进 provider。
	SaveHook func(*Config) error `json:"-"`
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
	// AppVersion 客户端版本（DeviceInfo.ClientVersion）。
	AppVersion string `json:"app_version"`
	// Disabled 手动禁用（保留在列表但不参与轮转）。
	Disabled bool `json:"disabled"`
	// DisabledReason 禁用原因（展示用）。
	DisabledReason string `json:"disabled_reason"`
}

// Normalize 补齐缺省值。
func (c *Config) Normalize() {
	if strings.TrimSpace(c.ChatBase) == "" {
		c.ChatBase = DefaultChatBase
	}
	c.ChatBase = strings.TrimRight(strings.TrimSpace(c.ChatBase), "/")
	if c.IDEVersion == "" {
		c.IDEVersion = DefaultIDEVersion
	}
	if c.IDEVersionCode == "" {
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
}

// Validate 校验配置自洽性（与 Load 同口径，供面板保存前调用）。
func (c *Config) Validate() error {
	c.Normalize()
	if !strings.HasPrefix(c.ChatBase, "http://") && !strings.HasPrefix(c.ChatBase, "https://") {
		return fmt.Errorf("chat_base 必须以 http:// 或 https:// 开头")
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

// Load 读取 trae.json。文件缺失返回 (nil, nil)——调用方据此判定"未启用 Trae"。
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
	if err := c.Validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &c, nil
}

// ConfigPath 返回与 config.json 同目录的 trae.json 路径。
func ConfigPath(configPath string) string {
	return filepath.Join(filepath.Dir(configPath), "trae.json")
}

// LogSummary 启动时打印一行摘要。
func (c *Config) LogSummary() {
	if c == nil || !c.Enabled {
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
