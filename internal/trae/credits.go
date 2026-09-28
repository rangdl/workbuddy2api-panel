package trae

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// credits.go Trae 签到与积分查询。
//
// 端点（tech-framework §6.1）：
//   - POST {api_base}/trae/api/v2/ug/checkin_credits/status   签到状态（只读预检）
//   - POST {api_base}/trae/api/v2/ug/checkin_credits/claim    执行签到
//   - POST {api_base}/trae/api/v2/pay/ide_user_ent_usage      积分包查询
//
// 与 chat 链路的关键差异：这三条接口用 **api.trae.cn**（不是 chat 的 mchost.guru），
// 且请求头族不同（VSCode UA + market/session 头）。body 均为 `{}`（积分查询带参数）。
//
// 宽容解析：响应字段可能被 data/result/resp/response/info 信封包裹（上游形态多变），
// 故 checked_in/credits 等业务字段一律深挖；但 **code 只看顶层**——
// 深挖会误伤嵌套业务对象里的同名 code。

// API 端点与常量。
const (
	// DefaultAPITraeBase 签到/积分接口 base。
	DefaultAPITraeBase = "https://api.trae.cn"
	// EndpointCheckinStatus 签到状态（只读）。
	EndpointCheckinStatus = "/trae/api/v2/ug/checkin_credits/status"
	// EndpointCheckinClaim 执行签到。
	EndpointCheckinClaim = "/trae/api/v2/ug/checkin_credits/claim"
	// EndpointEntUsage 积分包查询。
	EndpointEntUsage = "/trae/api/v2/pay/ide_user_ent_usage"
	// checkinUserAgent 签到接口的 UA（对齐真实客户端形态）。
	checkinUserAgent = "VSCode 1.107.1 (TRAE SOLO CN)"
	// checkinAppVersion 签到接口的 app-version（实测值，与 chat 的 IDE 版本不同）。
	checkinAppVersion = "0.1.45"
	// unwrapMaxDepth 信封深挖的最大层数。
	unwrapMaxDepth = 8
)

// creditRewardKeys claim 响应中疑似「本次奖励」的候选字段（按优先级）。
// delta 优先取 claim 响应自身的奖励字段——status 的 credits 无法离线确证是
// 「签到后余额」还是「可领奖励额度」。
var creditRewardKeys = []string{
	"reward", "reward_credits", "claim_credits", "checkin_credits",
	"delta", "increase", "obtain", "gained", "credits", "amount",
}

// payloadWrapperKeys 业务字段可能被这些键包裹。
var payloadWrapperKeys = []string{"data", "result", "resp", "response", "info"}

// CheckinStatus 签到状态预检结果。
type CheckinStatus struct {
	// CheckedIn 今日是否已签到（上游未给出该字段时为 nil）。
	CheckedIn *bool
	// Credits 当前积分（上游未给出时为 0）。
	Credits int64
	// Message 上游 message（原文）。
	Message string
}

// CheckinResult 一次签到的结果。
type CheckinResult struct {
	// Already 今日已签到（预检命中或上游按"已签"语义返回）。
	Already bool
	// Reward 本次奖励积分（上游返回奖励字段时 > 0）。
	Reward int64
	// Message 上游 message（原文优先）。
	Message string
}

// CreditPack 一个积分包。
type CreditPack struct {
	// ProductID 208 = 通用积分（IDE 使用）、209 = Work 积分（SOLO Agent）、其余（如 221）归通用。
	ProductID int64
	// CreditsLimit 本周期总额度。
	CreditsLimit float64
	// CreditsUsed 已用（usage.credits_amount；缺失按 0 计）。
	CreditsUsed float64
	// StartTime / EndTime 周期起止（上游原样字符串，如 "2026-09-14 15:52:38"）。
	StartTime string
	EndTime   string
	// GroupName 分组名（如"会员积分"）。
	GroupName string
	// NextBillingTime 会员套餐下次扣款时间（Unix 秒；0 = 无）。
	NextBillingTime int64
}

// Remaining 该包剩余额度。
func (p CreditPack) Remaining() float64 {
	r := p.CreditsLimit - p.CreditsUsed
	if r < 0 {
		return 0
	}
	return r
}

// IsWork 是否 Work 积分包（product_id == 209）。
// 签到积分归属会变动（2026-09-04 前发 209，之后改发 208），故按 product_id 动态判定，
// 不可写死来源。
func (p CreditPack) IsWork() bool { return p.ProductID == 209 }

// CreditsSummary 积分汇总。
type CreditsSummary struct {
	// General 通用积分剩余（product_id != 209 的包合计）。
	General float64
	// Work Work 积分剩余（product_id == 209 的包合计）。
	Work float64
	// Total 全部包剩余合计。
	Total float64
	// Limit 全部包额度合计。
	Limit float64
	// Packs 明细。
	Packs []CreditPack
}

// Checkin 执行一次签到：先 status 预检（已签则跳过），否则 claim。
//
// 预检失败（网络/鉴权）直接返回错误，不盲目 claim——避免在凭证失效时
// 反复打上游（与参考实现同取舍）。
func (c *Client) Checkin(ctx context.Context, cfg *Config, acc *Account) (*CheckinResult, error) {
	st, err := c.CheckinStatus(ctx, cfg, acc)
	if err != nil {
		return nil, err
	}
	if st.CheckedIn != nil && *st.CheckedIn {
		return &CheckinResult{Already: true, Message: orDefault(st.Message, "今日已签到")}, nil
	}
	return c.CheckinClaim(ctx, cfg, acc)
}

// CheckinStatus 查询今日签到状态（只读，可用于探活凭证）。
func (c *Client) CheckinStatus(ctx context.Context, cfg *Config, acc *Account) (*CheckinStatus, error) {
	body, err := c.apiPost(ctx, cfg, acc, EndpointCheckinStatus, nil)
	if err != nil {
		return nil, err
	}
	scopes := unwrapScopes(body)
	out := &CheckinStatus{Message: str(body["message"])}
	if v, ok := findPayloadField(scopes, "checked_in"); ok {
		switch t := v.(type) {
		case bool:
			out.CheckedIn = &t
		case float64:
			b := t != 0
			out.CheckedIn = &b
		}
	}
	if v, ok := findPayloadField(scopes, "credits"); ok {
		out.Credits, _ = asIntTolerant(v)
	}
	return out, nil
}

// CheckinClaim 执行签到并解析奖励。
func (c *Client) CheckinClaim(ctx context.Context, cfg *Config, acc *Account) (*CheckinResult, error) {
	body, err := c.apiPost(ctx, cfg, acc, EndpointCheckinClaim, nil)
	if err != nil {
		return nil, err
	}
	out := &CheckinResult{Message: str(body["message"])}
	// 奖励字段：优先 claim 响应自身；取不到再回落到包裹层。
	scopes := unwrapScopes(body)
	for _, key := range creditRewardKeys {
		if v, ok := findPayloadField(scopes, key); ok {
			if n, ok := asIntTolerant(v); ok && n > 0 {
				out.Reward = n
				break
			}
		}
	}
	if out.Message == "" {
		out.Message = "签到成功"
	}
	return out, nil
}

// QueryCredits 查询积分包明细与汇总。
func (c *Client) QueryCredits(ctx context.Context, cfg *Config, acc *Account) (*CreditsSummary, error) {
	payload := map[string]any{"require_usage": true, "req_source": 2}
	body, err := c.apiPost(ctx, cfg, acc, EndpointEntUsage, payload)
	if err != nil {
		return nil, err
	}
	packsRaw, ok := findPayloadField(unwrapScopes(body), "user_entitlement_pack_list")
	if !ok {
		return nil, fmt.Errorf("响应中缺少 user_entitlement_pack_list")
	}
	list, ok := packsRaw.([]any)
	if !ok {
		return nil, fmt.Errorf("user_entitlement_pack_list 不是数组")
	}
	sum := &CreditsSummary{Packs: make([]CreditPack, 0, len(list))}
	for _, raw := range list {
		pack, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		p := parseCreditPack(pack)
		if p.CreditsLimit <= 0 {
			continue // 无额度的包（如纯权益包）不计入统计
		}
		sum.Packs = append(sum.Packs, p)
		sum.Limit += p.CreditsLimit
		sum.Total += p.Remaining()
		if p.IsWork() {
			sum.Work += p.Remaining()
		} else {
			sum.General += p.Remaining()
		}
	}
	return sum, nil
}

// parseCreditPack 解析单个积分包（字段全部宽容取值）。
func parseCreditPack(pack map[string]any) CreditPack {
	var p CreditPack
	base, _ := pack["entitlement_base_info"].(map[string]any)
	if base == nil {
		base = pack // 部分形态字段直接在顶层
	}
	p.ProductID = intOf(base["product_id"])
	p.CreditsLimit = floatOf(base["credits_limit"])
	p.StartTime = str(base["start_time"])
	p.EndTime = firstNonEmpty(str(base["end_time"]), str(base["expire_time"]))
	p.GroupName = str(base["group_name"])
	p.NextBillingTime = intOf(base["next_billing_time"])
	// usage 在 pack 顶层，不在 entitlement_base_info 内。
	if usage, ok := pack["usage"].(map[string]any); ok {
		p.CreditsUsed = floatOf(usage["credits_amount"])
	}
	return p
}

// apiPost 向 api.trae.cn 发一次 POST（签到/积分接口共用）。
// payload 为 nil 时发 `{}`。
func (c *Client) apiPost(ctx context.Context, cfg *Config, acc *Account, path string, payload map[string]any) (map[string]any, error) {
	if cfg == nil || acc == nil {
		return nil, fmt.Errorf("trae: nil config or account")
	}
	token := acc.AccessTokenHeader()
	if token == "" {
		return nil, fmt.Errorf("trae: 账号 %s 无 access_token", acc.UID)
	}
	var bodyBytes []byte
	if payload == nil {
		bodyBytes = []byte("{}")
	} else {
		b, err := json.Marshal(payload)
		if err != nil {
			return nil, fmt.Errorf("请求体序列化失败: %w", err)
		}
		bodyBytes = b
	}
	url := cfg.apiBase() + path
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(string(bodyBytes)))
	if err != nil {
		return nil, err
	}
	applyCheckinHeaders(req, cfg, acc, token)

	// 非流式短请求：按 provider 超时（复用 jsonHTTP 但带请求级超时）。
	timeout := time.Duration(cfg.TimeoutSeconds) * time.Second
	if timeout <= 0 {
		timeout = DefaultTimeoutSeconds * time.Second
	}
	ctx2, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req = req.WithContext(ctx2)

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
	// code 只看顶层（深挖会误伤嵌套业务对象的同名 code）。
	// 服务端对吊销 JWT 也可能回 200 + 顶层通用鉴权失败码 1001。
	if code := intOf(parsed["code"]); code != 0 {
		msg := firstNonEmpty(str(parsed["message"]), fmt.Sprintf("业务错误 code=%d", code))
		return parsed, &APIError{StatusCode: resp.StatusCode, Code: code, Message: msg}
	}
	if resp.StatusCode >= 400 {
		return parsed, &APIError{
			StatusCode: resp.StatusCode,
			Message:    orDefault(strings.TrimSpace(string(raw)), fmt.Sprintf("HTTP %d", resp.StatusCode)),
		}
	}
	return parsed, nil
}

// APIError 签到/积分接口的错误（带 HTTP 状态与业务码，供分类）。
type APIError struct {
	StatusCode int
	Code       int64
	Message    string
}

func (e *APIError) Error() string {
	if e.Code != 0 {
		return fmt.Sprintf("HTTP %d code=%d: %s", e.StatusCode, e.Code, e.Message)
	}
	return fmt.Sprintf("HTTP %d: %s", e.StatusCode, e.Message)
}

// Classification 把 APIError 映射为统一错误分类（复用 errors.go 的码表）。
func (e *APIError) Classification() Classification {
	return Classify(e.StatusCode, nil, e.Code, e.Message)
}

// applyCheckinHeaders 签到/积分接口的头族（与 chat 链路不同：VSCode UA + market/session 头）。
func applyCheckinHeaders(req *http.Request, cfg *Config, acc *Account, tokenWithPrefix string) {
	h := req.Header
	h.Set("Accept", "*/*")
	h.Set("Accept-Language", "zh-CN")
	h.Set("Authorization", tokenWithPrefix)
	h.Set("Content-Type", "application/json")
	h.Set("User-Agent", checkinUserAgent)
	h.Set("X-Market-Client-Id", "VSCode 1.107.1")
	h.Set("X-Market-User-Id", acc.UID)
	h.Set("X-User-Region", "CN")
	h.Set("X-Device-Id", acc.DeviceID)
	h.Set("X-Lgw-Req-Sdk-Type", "3")
	h.Set("Package-Type", "stable_cn")
	h.Set("X-Lscbd-Aid", "787976")
	h.Set("X-Lscbd-Platform", "windows")
	h.Set("App-Version", checkinAppVersion)
	h.Set("Sec-Fetch-Dest", "empty")
	h.Set("Sec-Fetch-Mode", "no-cors")
	h.Set("Sec-Fetch-Site", "none")
	if reqID, err := RandomHex(32); err == nil {
		h.Set("X-Request-Id", reqID)
	}
	if trace, err := RandomHex(16); err == nil {
		h.Set("X-Tt-Trace-Id", "00-"+trace+"-01")
	}
	// vscode-sessionid 用设备 id 兜底（无独立 session 时上游不校验其内容）。
	h.Set("Vscode-Sessionid", acc.DeviceID)
}

// apiBase 生效的签到/积分接口 base。
func (c *Config) apiBase() string {
	if strings.TrimSpace(c.APITraeBase) != "" {
		return strings.TrimRight(strings.TrimSpace(c.APITraeBase), "/")
	}
	return DefaultAPITraeBase
}

// ---- 宽容解析 ----

// unwrapScopes 收集所有可能的字段作用域：顶层 + 逐层包裹键（限深）。
func unwrapScopes(body map[string]any) []map[string]any {
	scopes := make([]map[string]any, 0, 4)
	scopes = append(scopes, body)
	var walk func(m map[string]any, depth int)
	walk = func(m map[string]any, depth int) {
		if depth >= unwrapMaxDepth {
			return
		}
		for _, k := range payloadWrapperKeys {
			child, ok := m[k].(map[string]any)
			if !ok {
				continue
			}
			scopes = append(scopes, child)
			walk(child, depth+1)
		}
	}
	walk(body, 0)
	return scopes
}

// findPayloadField 按作用域顺序查找字段（顶层优先）。
func findPayloadField(scopes []map[string]any, key string) (any, bool) {
	for _, s := range scopes {
		if v, ok := s[key]; ok && v != nil {
			return v, true
		}
	}
	return nil, false
}

// asIntTolerant 宽容取整（float64 / json.Number / 数字字符串）。
func asIntTolerant(v any) (int64, bool) {
	switch n := v.(type) {
	case float64:
		return int64(n), true
	case int64:
		return n, true
	case int:
		return int64(n), true
	case json.Number:
		i, err := n.Int64()
		return i, err == nil
	case string:
		s := strings.TrimSpace(n)
		if s == "" {
			return 0, false
		}
		var f float64
		if _, err := fmt.Sscanf(s, "%g", &f); err != nil {
			return 0, false
		}
		return int64(f), true
	default:
		return 0, false
	}
}

// floatOf 宽容取浮点。
func floatOf(v any) float64 {
	switch n := v.(type) {
	case float64:
		return n
	case int64:
		return float64(n)
	case int:
		return float64(n)
	case json.Number:
		f, _ := n.Float64()
		return f
	case string:
		var f float64
		if _, err := fmt.Sscanf(strings.TrimSpace(n), "%g", &f); err == nil {
			return f
		}
		return 0
	default:
		return 0
	}
}
