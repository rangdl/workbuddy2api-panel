package trae

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// oauth.go Trae OAuth 登录与 token 续期。
//
// 协议（2026-09-16 抓包固化，蓝本 TraeWorkAssistant commands/oauth.rs）：
//   - 授权页 `https://www.trae.cn/authorization`，走 native_ide 渠道；
//     授权页前端调 GetPCAuthCode（绑定 PKCE challenge）后 302 回 auth_callback_url，
//     回调参数为 `authCodeInfo`（JSON）而非标准 code；
//   - AuthCode 换 token：`{ClientID, AuthCode, CodeVerifier, DeviceInfo{...}, IDEVersion}`，
//     **该场景不发 DeviceProof**，设备身份由 DeviceInfo（DeviceID + DevicePublicKey）声明；
//   - refreshToken 续期：固化协议要求 **DeviceProof** 签名（私钥来自客户端），
//     无私钥时回落 Legacy 变体（`{ClientID, RefreshToken, ClientSecret}`），
//     该变体是否仍被上游接受需实测（P0 PoC 项）。

// OAuth 常量。
const (
	// OAuthAuthorizeURL 授权页（native_ide 渠道）。
	OAuthAuthorizeURL = "https://www.trae.cn/authorization"
	// OAuthExchangePathNew 固化协议的 ExchangeToken 路径（DeviceProof 签名原文用）。
	OAuthExchangePathNew = "/trae/api/v3/oauth/ExchangeToken"
	// OAuthExchangePathLegacy 旧协议路径（cloudide 前缀）。
	OAuthExchangePathLegacy = "/cloudide/api/v3/trae/oauth/ExchangeToken"
	// OAuthHostDefault ExchangeToken 的 host（授权页回传的 API 域）。
	OAuthHostDefault = "https://api.trae.com.cn"
	// DefaultRedirectURI 缺省回调地址（本机回环；服务端部署可改为公网回调或手工粘贴）。
	DefaultRedirectURI = "http://127.0.0.1:17388/authorize"
	// DefaultPagePluginVersion / DefaultPageAppVersion 授权页参数（抓包实证值）。
	DefaultPagePluginVersion = "2.3.83560"
	DefaultPageAppVersion    = "3.3.100"
	// PlatformCode 设备平台码。
	PlatformCode = "IDE_PC"
)

// LoginSession 一次进行中的登录会话（PKCE + CSRF 绑定）。
type LoginSession struct {
	// State login_trace_id：授权页原样回传为回调的 loginTraceID，用于 CSRF 双向校验。
	State string `json:"state"`
	// PKCEVerifier code_verifier（换 token 时提交）。
	PKCEVerifier string `json:"pkce_verifier"`
	// DeviceID / MachineID 本次登录使用的设备标识（与签名私钥同源）。
	DeviceID  string `json:"device_id"`
	MachineID string `json:"machine_id"`
	// CreatedAt 会话创建时刻（调用方据此判超时）。
	CreatedAt time.Time `json:"created_at"`
}

// TokenPair 一次交换得到的凭据。
type TokenPair struct {
	AccessToken  string
	RefreshToken string
	// ExpiresAt access token 过期时间（Unix 秒；0 = 未知）。
	ExpiresAt int64
}

// PKCEPair 生成 PKCE code_verifier 与 S256 code_challenge（RFC 7636）。
// verifier 用 64 字节 hex（128 字符，落在 43–128 的合规区间）。
func PKCEPair() (verifier, challenge string, err error) {
	verifier, err = RandomHex(64)
	if err != nil {
		return "", "", err
	}
	sum := sha256.Sum256([]byte(verifier))
	challenge = base64.RawURLEncoding.EncodeToString(sum[:])
	return verifier, challenge, nil
}

// LoginURLOptions 授权 URL 构造参数。
type LoginURLOptions struct {
	// ClientID 缺省 DefaultClientID。
	ClientID string
	// CallbackURL 缺省 DefaultRedirectURI。
	CallbackURL string
	// DeviceID / MachineID 设备标识（建议来自已导入的设备凭证，与签名私钥同源——
	// 不一致会导致上游 20403/20405）。
	DeviceID  string
	MachineID string
	// DeviceBrand 设备品牌（缺省 "Web"；真实客户端用主机名）。
	DeviceBrand string
	// PluginVersion / AppVersion 授权页参数（缺省内置抓包实证值）。
	PluginVersion string
	AppVersion    string
}

// BuildLoginURL 生成授权 URL 与对应的登录会话。
//
// 参数形态对齐真实 Trae IDE 登录页（抓包固化）：旧形态（client_secret /
// response_type=code）会让授权页停在 billing status 后不回跳。
func BuildLoginURL(opt LoginURLOptions) (string, *LoginSession, error) {
	if opt.ClientID == "" {
		opt.ClientID = DefaultClientID
	}
	if opt.CallbackURL == "" {
		opt.CallbackURL = DefaultRedirectURI
	}
	if opt.PluginVersion == "" {
		opt.PluginVersion = DefaultPagePluginVersion
	}
	if opt.AppVersion == "" {
		opt.AppVersion = DefaultPageAppVersion
	}
	if opt.DeviceBrand == "" {
		opt.DeviceBrand = "Web"
	}
	if opt.DeviceID == "" || opt.MachineID == "" {
		return "", nil, fmt.Errorf("缺少设备标识（请先导入设备凭证或显式指定 device_id/machine_id）")
	}
	traceID, err := RandomHex(32)
	if err != nil {
		return "", nil, err
	}
	verifier, challenge, err := PKCEPair()
	if err != nil {
		return "", nil, err
	}

	q := url.Values{}
	q.Set("login_version", "1")
	q.Set("auth_from", "trae")
	q.Set("login_channel", "native_ide")
	q.Set("plugin_version", opt.PluginVersion)
	q.Set("auth_type", "local")
	q.Set("client_id", opt.ClientID)
	q.Set("redirect", "0")
	q.Set("login_trace_id", traceID)
	q.Set("auth_callback_url", opt.CallbackURL)
	q.Set("machine_id", opt.MachineID)
	q.Set("device_id", opt.DeviceID)
	q.Set("x_device_id", opt.DeviceID)
	q.Set("x_machine_id", opt.MachineID)
	q.Set("x_device_brand", opt.DeviceBrand)
	q.Set("x_device_type", "windows")
	q.Set("x_os_version", "Windows")
	q.Set("x_env", "")
	q.Set("x_app_version", opt.AppVersion)
	q.Set("x_app_type", "stable")
	q.Set("code_challenge", challenge)
	q.Set("code_challenge_method", "S256")
	q.Set("channel_name", "common")

	sess := &LoginSession{
		State:        traceID,
		PKCEVerifier: verifier,
		DeviceID:     opt.DeviceID,
		MachineID:    opt.MachineID,
		CreatedAt:    time.Now(),
	}
	return OAuthAuthorizeURL + "?" + q.Encode(), sess, nil
}

// ParseAuthCallback 从回调 URL 提取 authCode。
//
// 回调参数形态（实测）：`authCodeInfo` 是一个 JSON 串（内含 AuthCode 等字段），
// 另有 `loginTraceID` 用于 CSRF 校验。兼容 `refreshToken` / `code` 两种旧形态。
func ParseAuthCallback(rawURL string) (authCode, traceID string, err error) {
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil {
		return "", "", fmt.Errorf("回调 URL 解析失败: %w", err)
	}
	q := u.Query()
	traceID = q.Get("loginTraceID")
	if traceID == "" {
		traceID = q.Get("login_trace_id")
	}
	if info := q.Get("authCodeInfo"); info != "" {
		authCode = extractAuthCode(info)
	}
	if authCode == "" {
		authCode = firstNonEmpty(q.Get("code"), q.Get("authCode"), q.Get("AuthCode"))
	}
	if authCode == "" {
		return "", traceID, fmt.Errorf("回调 URL 中未找到 authCodeInfo/code 参数")
	}
	return authCode, traceID, nil
}

// extractAuthCode 从 authCodeInfo 取值：优先当 JSON 解析，失败则当裸串。
func extractAuthCode(info string) string {
	s := strings.TrimSpace(info)
	if strings.HasPrefix(s, "{") {
		var m map[string]any
		if json.Unmarshal([]byte(s), &m) == nil {
			for _, k := range []string{"AuthCode", "authCode", "auth_code", "Code", "code"} {
				if v, ok := m[k].(string); ok && strings.TrimSpace(v) != "" {
					return strings.TrimSpace(v)
				}
			}
		}
	}
	return s
}

// firstNonEmpty 返回第一个非空字符串。
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// ExchangeAuthCode 用授权码换 token。
//
// cred 非空时走**主变体**（DeviceInfo + DevicePublicKey，无 DeviceProof）；
// cred 为空时走无 DeviceInfo 的兜底变体（只带 DeviceID）。
// 两者都失败时返回聚合错误，便于面板展示排查。
func (c *Client) ExchangeAuthCode(ctx context.Context, cfg *Config, authCode, verifier string, cred *DeviceCredential, deviceID, machineID string) (*TokenPair, error) {
	clientID := cfg.oauthClientID()
	host := cfg.oauthHost()
	appVersion := DefaultPageAppVersion

	type variant struct {
		tag     string
		payload map[string]any
		withDev bool // 是否带 x-cloudide-token 空串头
	}
	var variants []variant

	if cred != nil && cred.PrivateKeyPEM != "" {
		priv, err := ParseECPrivateKey(cred.PrivateKeyPEM)
		if err == nil {
			pubPEM, _ := PublicKeyPEM(priv)
			variants = append(variants, variant{
				tag: "AuthCode+DeviceInfo",
				payload: map[string]any{
					"ClientID":     clientID,
					"AuthCode":     authCode,
					"CodeVerifier": verifier,
					"DeviceInfo": map[string]any{
						"DeviceID":        cred.DeviceID,
						"MachineID":       cred.MachineID,
						"PlatformCode":    PlatformCode,
						"DeviceType":      "PC",
						"DeviceName":      "",
						"DeviceModel":     "",
						"ClientVersion":   appVersion,
						"DevicePublicKey": pubPEM,
						"DeviceBrand":     "",
						"DeviceCPU":       "",
						"OSInfo":          "",
						"OSVersion":       "",
					},
					"IDEVersion": appVersion,
				},
				withDev: true,
			})
		}
	}
	// 兜底变体：无 DeviceInfo（本机无客户端凭证时的唯一路径）。
	for _, key := range []string{"AuthCode", "Code"} {
		variants = append(variants, variant{
			tag: "AuthCode/" + key,
			payload: map[string]any{
				"ClientID":     clientID,
				key:            authCode,
				"CodeVerifier": verifier,
				"DeviceID":     deviceID,
				"PlatformCode": PlatformCode,
			},
			withDev: false,
		})
	}

	var errs []string
	for _, v := range variants {
		pair, err := c.exchangeOnce(ctx, cfg, host+OAuthExchangePathNew, v.payload, deviceID, v.withDev)
		if c.Trace != nil {
			c.Trace(v.tag, err == nil, err)
		}
		if err == nil {
			return pair, nil
		}
		errs = append(errs, v.tag+": "+err.Error())
	}
	return nil, fmt.Errorf("全部交换变体失败 → %s", strings.Join(errs, " | "))
}

// ExchangeRefresh 用 refresh_token 换新 token。
//
// 变体链（主 → 兜底）：
//  1. DeviceProof（P1363）+ 固化端点——私钥来自客户端凭证，**唯一有把握的路径**；
//  2. DeviceProof（P1363）+ 旧端点；
//  3. Legacy（ClientSecret，无 proof）——上游可能已关闭，失败属预期。
func (c *Client) ExchangeRefresh(ctx context.Context, cfg *Config, acc *Account) (*TokenPair, error) {
	if strings.TrimSpace(acc.RefreshToken) == "" {
		return nil, fmt.Errorf("账号 %s 无 refresh_token", acc.UID)
	}
	clientID := cfg.oauthClientID()
	host := cfg.oauthHost()

	var variants []struct {
		tag     string
		url     string
		payload map[string]any
		withDev bool
	}

	if acc.PrivateKeyPEM != "" {
		priv, err := ParseECPrivateKey(acc.PrivateKeyPEM)
		if err != nil {
			return nil, fmt.Errorf("设备私钥解析失败: %w", err)
		}
		for _, v := range []struct{ path, url string }{
			{OAuthExchangePathNew, host + OAuthExchangePathNew},
			{OAuthExchangePathLegacy, host + OAuthExchangePathLegacy},
		} {
			nonce, err := RandomHex(32)
			if err != nil {
				return nil, err
			}
			ts := time.Now().Unix()
			proof, err := DeviceProof(priv, v.path, clientID, acc.RefreshToken, ts, nonce)
			if err != nil {
				return nil, err
			}
			variants = append(variants, struct {
				tag     string
				url     string
				payload map[string]any
				withDev bool
			}{
				tag: "Refresh/Proof" + v.path,
				url: v.url,
				payload: map[string]any{
					"ClientID":     clientID,
					"RefreshToken": acc.RefreshToken,
					"DeviceID":     acc.DeviceID,
					"PlatformCode": PlatformCode,
					"DeviceProof":  proof,
				},
				withDev: true,
			})
		}
	}
	// Legacy 兜底：无 proof（无设备私钥时的唯一路径，可用性待实测）。
	variants = append(variants, struct {
		tag     string
		url     string
		payload map[string]any
		withDev bool
	}{
		tag: "Refresh/Legacy",
		url: host + OAuthExchangePathLegacy,
		payload: map[string]any{
			"ClientID":     clientID,
			"RefreshToken": acc.RefreshToken,
			"ClientSecret": cfg.oauthClientSecret(),
			"UserID":       "",
		},
		withDev: false,
	})

	var errs []string
	for _, v := range variants {
		pair, err := c.exchangeOnce(ctx, cfg, v.url, v.payload, acc.DeviceID, v.withDev)
		if c.Trace != nil {
			c.Trace(v.tag, err == nil, err)
		}
		if err == nil {
			return pair, nil
		}
		errs = append(errs, v.tag+": "+err.Error())
	}
	return nil, fmt.Errorf("全部刷新变体失败 → %s", strings.Join(errs, " | "))
}

// exchangeOnce 执行一次 ExchangeToken 请求并宽容提取 token。
func (c *Client) exchangeOnce(ctx context.Context, cfg *Config, endpoint string, payload map[string]any, deviceID string, withDeviceHeader bool) (*TokenPair, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("请求体序列化失败: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(string(body)))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "*/*")
	req.Header.Set("X-Device-Id", deviceID)
	req.Header.Set("X-App-Id", cfg.AppID)
	req.Header.Set("X-Platform-Code", PlatformCode)
	if withDeviceHeader {
		// 实测：带旧 token 报 20405，完全不带该头报 20403——必须为空串。
		req.Header.Set("X-Cloudide-Token", "")
	}
	resp, err := c.jsonHTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBodyBytes))

	var parsed map[string]any
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, fmt.Errorf("响应非 JSON（HTTP %d）: %s", resp.StatusCode, preview(raw, 200))
	}
	if code, msg := volcanoError(parsed); code != "" {
		return nil, fmt.Errorf("code=%s: %s", code, msg)
	}
	if code := intOf(parsed["code"]); code != 0 {
		return nil, fmt.Errorf("code=%d: %s", code, str(parsed["message"]))
	}
	pair := extractTokenPair(parsed)
	if pair.AccessToken == "" {
		return nil, fmt.Errorf("响应中未找到 access token（HTTP %d）: %s", resp.StatusCode, preview(raw, 200))
	}
	return pair, nil
}

// volcanoError 解析火山引擎标准信封 `ResponseMetadata.Error.{Code,Message}`。
func volcanoError(body map[string]any) (code, msg string) {
	meta, _ := body["ResponseMetadata"].(map[string]any)
	if meta == nil {
		return "", ""
	}
	errObj, _ := meta["Error"].(map[string]any)
	if errObj == nil {
		return "", ""
	}
	c := strings.TrimSpace(str(errObj["Code"]))
	if c == "" || c == "0" {
		return "", ""
	}
	m := strings.TrimSpace(str(errObj["Message"]))
	if m == "" {
		m = "未知错误"
	}
	if sc := strings.TrimSpace(str(errObj["StandardCode"])); sc != "" {
		m += " (" + sc + ")"
	}
	return c, m
}

// extractTokenPair 宽容提取 token：Result/data/Data 容器精确键 → 全树深挖。
//
// 候选键必须包含 **"Token"（首字母大写）**：上游 Result 容器里用的正是这个形态
// （实测 `Result.Token` / `Result.RefreshToken`），只列小写变体会静默漏取。
func extractTokenPair(body map[string]any) *TokenPair {
	var container map[string]any
	for _, k := range []string{"Result", "data", "Data"} {
		if m, ok := body[k].(map[string]any); ok {
			container = m
			break
		}
	}
	if container == nil {
		container = body
	}
	pair := &TokenPair{
		AccessToken:  pickString(container, "AccessToken", "access_token", "Token", "token", "Jwt", "JWT"),
		RefreshToken: pickString(container, "RefreshToken", "refresh_token", "Refresh"),
	}
	if pair.AccessToken == "" {
		// 容器层级不符时全树深挖（上游字段名变体多）。
		pair.AccessToken = deepFindAny(container, []string{"accesstoken", "token", "jwt"}, 0)
	}
	if pair.RefreshToken == "" {
		pair.RefreshToken = deepFindAny(container, []string{"refreshtoken", "refreshtokenvalue"}, 0)
	}
	for _, k := range []string{"ExpiresAt", "expires_at", "expiresAtMs", "expires_at_ms"} {
		if v := intOf(container[k]); v > 0 {
			// 毫秒时间戳统一折算为秒。
			if v > 1e12 {
				v /= 1000
			}
			pair.ExpiresAt = v
			break
		}
	}
	if pair.ExpiresAt == 0 {
		pair.ExpiresAt = ExpiryFromJWT(pair.AccessToken)
	}
	return pair
}

// deepFindAny 按候选键依次深挖，返回首个命中。
func deepFindAny(v any, keys []string, depth int) string {
	for _, k := range keys {
		if s := deepFind(v, k, depth); s != "" {
			return s
		}
	}
	return ""
}

// pickString 按候选键取字符串（大小写敏感，按序返回首个非空）。
func pickString(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if v, ok := m[k].(string); ok && strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// deepFind 深度优先查找键名（小写比较，限深 6 层）。
func deepFind(v any, keyLower string, depth int) string {
	if depth > 6 {
		return ""
	}
	switch t := v.(type) {
	case map[string]any:
		for k, val := range t {
			if strings.ToLower(k) == keyLower {
				if s, ok := val.(string); ok && strings.TrimSpace(s) != "" {
					return strings.TrimSpace(s)
				}
			}
		}
		for _, val := range t {
			if s := deepFind(val, keyLower, depth+1); s != "" {
				return s
			}
		}
	case []any:
		for _, val := range t {
			if s := deepFind(val, keyLower, depth+1); s != "" {
				return s
			}
		}
	}
	return ""
}

// preview 截断文本用于错误摘要。
func preview(raw []byte, n int) string {
	s := string(raw)
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// oauthClientID / oauthClientSecret / oauthHost 取生效的 OAuth 参数（配置覆盖 → 内置默认）。
func (c *Config) oauthClientID() string {
	if strings.TrimSpace(c.OAuthClientID) != "" {
		return strings.TrimSpace(c.OAuthClientID)
	}
	return DefaultClientID
}

func (c *Config) oauthClientSecret() string {
	if strings.TrimSpace(c.OAuthClientSecret) != "" {
		return strings.TrimSpace(c.OAuthClientSecret)
	}
	return DefaultClientSecret
}

func (c *Config) oauthHost() string {
	if strings.TrimSpace(c.OAuthHost) != "" {
		return strings.TrimRight(strings.TrimSpace(c.OAuthHost), "/")
	}
	return OAuthHostDefault
}
