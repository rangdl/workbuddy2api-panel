// Package trae 实现 Trae（trae.cn / trae.com.cn）上游接入。
//
// 与 internal/upstream（CodeBuddy）和 internal/thirdparty（OpenAI 兼容）并列的
// 第三个上游 provider。对话经 /v1/* 统一调度（模型名带 "trae:" 前缀指定），
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
	// Enabled 总开关；false 时该上游不参与调度。
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
