package trae

import (
	"context"
	"encoding/json"
	"errors"
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
//   - POST {api_base}/trae/api/v2/pay/web_user_ent_usage  积分包查询
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
	//
	// 必须是 **web_user_ent_usage**：dsh-connect-trae 的实测文档
	// （docs/USAGE_API_RESEARCH.md，2026-09）里，能读到「总可用额度/积分来源」
	// 的只有 web_user_ent_usage（别名 user_current_entitlement_list）；
	// 早先用的 ide_user_ent_usage **不在可用列表里**——它返回 200 但不含
	// user_entitlement_pack_list，于是所有数值都算成 0。
	EndpointEntUsage = "/trae/api/v2/pay/web_user_ent_usage"
	// EndpointEntUsageAlias 同上接口的别名，主端点异常时兜底。
	EndpointEntUsageAlias = "/trae/api/v2/pay/user_current_entitlement_list"
	// checkinUserAgent 签到接口的 UA（对齐真实客户端形态）。
	//
	// 2026-11 客户端合并（TraeWork + TraeCode → 统一 TRAE，桌面端改名 Trae CN）后，
	// 新版客户端（Trae CN 1.107.x / IDE 构建 0.1.52）的 UA 是 `Trae/<ide_version>`，
	// 不再是旧版的 `VSCode 1.107.1 (TRAE SOLO CN)`。旧值保留在
	// LegacyCheckinUserAgent，可用 Config.CheckinUserAgent 一键回退。
	checkinUserAgent = "Trae/0.1.52"
	// LegacyCheckinUserAgent 合并前的旧 UA（回退用）。
	LegacyCheckinUserAgent = "VSCode 1.107.1 (TRAE SOLO CN)"
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
	// DeviceAlready 该设备今日已签（did_checked_in=true 或 9095）。
	// 与 CheckedIn 的区别：CheckedIn 是账号维度，DeviceAlready 是设备维度。
	DeviceAlready bool
}

// CheckinResult 一次签到的结果。
type CheckinResult struct {
	// Already 今日已签到（预检命中或上游按"已签"语义返回 9004/9095）。
	Already bool
	// Reward 本次奖励积分（上游返回奖励字段时 > 0）。
	Reward int64
	// Message 上游 message（原文优先）。
	Message string
	// DeviceAlready 该**设备**今日已签（9095）。限额按设备算，多账号共用
	// 同一设备号时只有第一个能签成；调用方据此停止本批后续账号。
	DeviceAlready bool
	// Unverified claim 返回成功但同设备号回查未确认（可能是"假成功"）。
	// 见 Config.VerifyClaim。
	Unverified bool
}

// CreditPack 一个积分包。
type CreditPack struct {
	// ProductID 208 = 通用积分（IDE 使用）、209 = Work 积分（SOLO Agent）、其余（如 221）归通用。
	ProductID int64 `json:"product_id"`
	// CreditsLimit 本周期总额度。
	CreditsLimit float64 `json:"credits_limit"`
	// CreditsUsed 已用（usage.credits_amount；缺失按 0 计）。
	CreditsUsed float64 `json:"credits_used"`
	// StartTime / EndTime 周期起止（上游原样字符串，如 "2026-09-14 15:52:38"）。
	StartTime string `json:"start_time"`
	EndTime   string `json:"end_time"`
	// GroupName 分组名（如"会员积分"）。
	GroupName string `json:"group_name"`
	// NextBillingTime 会员套餐下次扣款时间（Unix 秒；0 = 无）。
	NextBillingTime int64 `json:"next_billing_time"`
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
//
// JSON tag 必须显式写出：默认序列化用字段名（General/Work/…），
// 前端读的是小写（c.general），漏了就会显示 NaN。
type CreditsSummary struct {
	// General 通用积分剩余（product_id != 209 的包合计）。
	General float64 `json:"general"`
	// Work Work 积分剩余（product_id == 209 的包合计）。
	Work float64 `json:"work"`
	// Total 全部包剩余合计。
	Total float64 `json:"total"`
	// Limit 全部包额度合计。
	Limit float64 `json:"limit"`
	// Packs 明细。
	Packs []CreditPack `json:"packs"`
	// TotalFromSummary true = Limit/Total 取自上游 usage_summary；
	// false = 上游没给 usage_summary，退回逐包求和。
	TotalFromSummary bool `json:"total_from_summary"`
}

// Checkin 执行一次签到：先 status 预检（已签则跳过），否则 claim。
//
// 预检失败（网络/鉴权）直接返回错误，不盲目 claim——避免在凭证失效时
// 反复打上游（与参考实现同取舍）。
//
// 「今日已签」有两条来源，都必须当成功分支而不是失败：
//   - 预检 checked_in / did_checked_in 为真；
//   - claim 返回业务码 9004（缺设备头）或 9095（该设备今日已签）。
func (c *Client) Checkin(ctx context.Context, cfg *Config, acc *Account) (*CheckinResult, error) {
	st, err := c.CheckinStatus(ctx, cfg, acc)
	if err != nil {
		return nil, err
	}
	if st.CheckedIn != nil && *st.CheckedIn {
		return &CheckinResult{
			Already:       true,
			DeviceAlready: st.DeviceAlready,
			Message:       orDefault(st.Message, "今日已签到"),
		}, nil
	}
	res, err := c.CheckinClaim(ctx, cfg, acc)
	if err != nil {
		// 9004 / 9095：上游按「已签」语义拒绝领取，不是失败。
		if cls := classifyAPIError(err); cls.Already {
			return &CheckinResult{
				Already:       true,
				DeviceAlready: cls.Code == "9095" || cls.Code == "9004",
				Message:       cls.Message,
			}, nil
		}
		return nil, err
	}
	// 回查确认：claim 返回 code=0 可能是「假成功」——当日已签过的账号对
	// 任何 device_id 都会回 0（L0NE-6 实测）。以同设备号回查 checked_in
	// 翻转才算真到账；明确未翻转才标 Unverified（字段缺失时无法判断，按成功处理）。
	if cfg != nil && cfg.VerifyClaimEnabled() {
		if again, aerr := c.CheckinStatus(ctx, cfg, acc); aerr == nil &&
			again.CheckedIn != nil && !*again.CheckedIn {
			res.Unverified = true
			res.Message = "领取接口返回成功，但同设备号回查未确认（可能未到账，下轮重试）"
		}
	}
	return res, nil
}

// CheckinStatus 查询今日签到状态（只读，可用于探活凭证）。
func (c *Client) CheckinStatus(ctx context.Context, cfg *Config, acc *Account) (*CheckinStatus, error) {
	body, err := c.apiPost(ctx, cfg, acc, EndpointCheckinStatus, checkinBody(cfg))
	if err != nil {
		// 9004/9095 在预检接口上等价于"已签"，不能当失败。
		if cls := classifyAPIError(err); cls.Already {
			return &CheckinStatus{CheckedIn: boolPtr(true), Message: cls.Message}, nil
		}
		return nil, err
	}
	scopes := unwrapScopes(body)
	out := &CheckinStatus{Message: str(body["message"])}
	// checked_in 与 did_checked_in 都要看：前者是账号维度，后者是**设备**维度。
	// 两者任一为真即视为今日已签，避免"账号没签过但设备签过了"被误判为可签。
	for _, key := range []string{"checked_in", "did_checked_in"} {
		if v, ok := findPayloadField(scopes, key); ok {
			if b := asBoolTolerant(v); b != nil && *b {
				out.CheckedIn = boolPtr(true)
				out.DeviceAlready = key == "did_checked_in" || out.DeviceAlready
				break
			}
		}
	}
	if out.CheckedIn == nil {
		if v, ok := findPayloadField(scopes, "checked_in"); ok {
			out.CheckedIn = asBoolTolerant(v)
		}
	}
	if v, ok := findPayloadField(scopes, "credits"); ok {
		out.Credits, _ = asIntTolerant(v)
	}
	// credits=0 但 extra_credits>0 时展示值应取后者（两者并列，绝不相加）。
	if out.Credits <= 0 {
		if v, ok := findPayloadField(scopes, "extra_credits"); ok {
			if n, ok := asIntTolerant(v); ok && n > 0 {
				out.Credits = n
			}
		}
	}
	return out, nil
}

// CheckinClaim 执行签到并解析奖励。
func (c *Client) CheckinClaim(ctx context.Context, cfg *Config, acc *Account) (*CheckinResult, error) {
	body, err := c.apiPost(ctx, cfg, acc, EndpointCheckinClaim, checkinBody(cfg))
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
//
// 端点顺序：web_user_ent_usage → user_current_entitlement_list（别名）。
// 两者结构相同，任一个返回了 pack 列表即可。
func (c *Client) QueryCredits(ctx context.Context, cfg *Config, acc *Account) (*CreditsSummary, error) {
	payload := map[string]any{"require_usage": true}
	var lastErr error
	for _, endpoint := range []string{EndpointEntUsage, EndpointEntUsageAlias} {
		body, err := c.apiPost(ctx, cfg, acc, endpoint, payload)
		if err != nil {
			lastErr = err
			continue
		}
		sum, err := summarizeCredits(body)
		if err != nil {
			lastErr = err
			continue
		}
		return sum, nil
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("积分接口无可用响应")
	}
	return nil, lastErr
}

// summarizeCredits 把一次积分接口响应转成汇总。
func summarizeCredits(body map[string]any) (*CreditsSummary, error) {
	packsRaw, ok := findPayloadField(unwrapScopes(body), "user_entitlement_pack_list")
	if !ok {
		return nil, fmt.Errorf("响应中缺少 user_entitlement_pack_list")
	}
	list, ok := packsRaw.([]any)
	if !ok {
		return nil, fmt.Errorf("user_entitlement_pack_list 不是数组")
	}
	sum := &CreditsSummary{
		Packs: make([]CreditPack, 0, len(list)),
		// 上游给的总额/已用（usage_summary）优先作为合计口径：
		// 逐包求和会漏掉没有 credits_limit 的包（免费包等）。
		TotalFromSummary: true,
	}
	if ss, ok := findPayloadField(unwrapScopes(body), "usage_summary"); ok {
		if m, ok := ss.(map[string]any); ok {
			total := floatOf(m["total_amount"])
			used := floatOf(m["consumed_amount"])
			if total > 0 {
				sum.Limit = total
				sum.Total = maxFloat(total-used, 0)
			}
		}
	}
	packTotal, packLimit := 0.0, 0.0
	for _, raw := range list {
		pack, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		p := parseCreditPack(pack)
		if p.CreditsLimit <= 0 && p.CreditsUsed <= 0 {
			continue // 纯权益包（无额度）不计入
		}
		sum.Packs = append(sum.Packs, p)
		packLimit += p.CreditsLimit
		packTotal += p.Remaining()
		if p.IsWork() {
			sum.Work += p.Remaining()
		} else {
			sum.General += p.Remaining()
		}
	}
	// usage_summary 缺失时退回逐包求和。
	if sum.Limit <= 0 {
		sum.Limit = packLimit
		sum.Total = packTotal
		sum.TotalFromSummary = false
	}
	return sum, nil
}

// maxFloat 取较大值（避免为一次比较引入 math）。
func maxFloat(a, b float64) float64 {
	if a > b {
		return a
	}
	return b
}

// parseCreditPack 解析单个积分包（字段全部宽容取值）。
//
// 字段位置以 L0NE-6 / dsh 的实测结构为准，credits_limit 有**三处**可能：
//
//	entitlement_base_info.product_extra.package_extra.quota.credits_limit（会员包）
//	entitlement_base_info.quota.credits_limit（常规包）
//	entitlement_base_info.credits_limit（旧形态兜底）
//
// 早先只读最后一种，于是所有包都算成 0 额度、整页显示 0。
func parseCreditPack(pack map[string]any) CreditPack {
	var p CreditPack
	base, _ := pack["entitlement_base_info"].(map[string]any)
	if base == nil {
		base = pack // 部分形态字段直接在顶层
	}
	quota, _ := base["quota"].(map[string]any)
	if quota == nil {
		if q, ok := pack["quota"].(map[string]any); ok {
			quota = q
		} else {
			quota = map[string]any{}
		}
	}
	var packageQuota map[string]any
	if pe, ok := base["product_extra"].(map[string]any); ok {
		if pkg, ok := pe["package_extra"].(map[string]any); ok {
			packageQuota, _ = pkg["quota"].(map[string]any)
		}
	}
	p.ProductID = intOf(firstNonNil(base["product_id"], quota["product_id"]))
	p.CreditsLimit = firstPositive(
		floatOf(packageQuota["credits_limit"]),
		floatOf(quota["credits_limit"]),
		floatOf(base["credits_limit"]),
	)
	// usage.credits_amount 的语义是「该包已消耗」，剩余 = limit - used。
	// 注意它**不是**剩余量（dsh 的 USAGE_API_RESEARCH 记录里"剩余 179.63"
	// 是 limit-usage 的结果，字段本身是消耗量）。
	//
	// 取值位置：真实响应把 usage 放在 entitlement_base_info 下（与 quota 同级），
	// 但也有形态放在包顶层。先 base 后 pack——只查 pack 会让 CreditsUsed 恒为 0，
	// 于是「剩余」被算成「总额度」，快耗尽的账号看起来满额。
	usedFrom := base
	if _, ok := usedFrom["usage"].(map[string]any); !ok {
		usedFrom = pack
	}
	if usage, ok := usedFrom["usage"].(map[string]any); ok {
		p.CreditsUsed = floatOf(usage["credits_amount"])
	}
	p.StartTime = str(base["start_time"])
	p.EndTime = firstNonEmpty(str(base["end_time"]), str(base["expire_time"]),
		str(pack["expire_time"]))
	p.GroupName = firstNonEmpty(str(pack["display_desc"]), str(base["group_name"]))
	p.NextBillingTime = intOf(base["next_billing_time"])
	return p
}

// firstNonNil 返回第一个非 nil 值。
func firstNonNil(vals ...any) any {
	for _, v := range vals {
		if v != nil {
			return v
		}
	}
	return nil
}

// firstPositive 返回第一个 > 0 的值（全为 0 时返回 0）。
func firstPositive(vals ...float64) float64 {
	for _, v := range vals {
		if v > 0 {
			return v
		}
	}
	return 0
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

// classifyAPIError 尽力把任意错误转成 Classification（非 APIError 返回空分类）。
func classifyAPIError(err error) Classification {
	if err == nil {
		return Classification{}
	}
	// 直接类型断言优先：APIError 只以 *APIError 形式产生，
	// 用 errors.As 反而会漏（*APIError 与 error 接口的匹配在个别包装下不稳定）。
	if e, ok := err.(*APIError); ok {
		return e.Classification()
	}
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		return apiErr.Classification()
	}
	return Classification{}
}

// boolPtr 取 bool 指针（CheckinStatus.CheckedIn 用 nil 表示"上游没给这个字段"）。
func boolPtr(b bool) *bool { return &b }

// asBoolTolerant 宽容读取 bool（上游可能给 bool / 数字 / "true"）。
func asBoolTolerant(v any) *bool {
	switch t := v.(type) {
	case bool:
		return boolPtr(t)
	case float64:
		return boolPtr(t != 0)
	case string:
		switch strings.ToLower(strings.TrimSpace(t)) {
		case "true", "1", "yes":
			return boolPtr(true)
		case "false", "0", "no":
			return boolPtr(false)
		}
	}
	return nil
}

// applyCheckinHeaders 签到/积分接口的头族（与 chat 链路不同：market/session 头）。
//
// 只把 UA 换成新版合并客户端的 `Trae/<ide_version>` 形态——这是唯一有实证来源的
// 变化（Trae CN 1.107.1 / IDE 0.1.52 客户端）。X-Market-Client-Id 保持原有的
// VSCode 形态不动：上游没有证据表明它跟着改名，跟着改反而可能偏离真实指纹。
func applyCheckinHeaders(req *http.Request, cfg *Config, acc *Account, tokenWithPrefix string) {
	ua := checkinUserAgent
	if cfg != nil {
		if v := strings.TrimSpace(cfg.CheckinUserAgent); v != "" {
			ua = v
		}
	}
	h := req.Header
	h.Set("Accept", "*/*")
	h.Set("Accept-Language", "zh-CN")
	h.Set("Authorization", tokenWithPrefix)
	h.Set("Content-Type", "application/json")
	h.Set("User-Agent", ua)
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

// checkinBody 签到类接口的请求体。
//
// 缺省 `{}`（本项目实测可用）；Config.CheckinReqSource 非 0 时改为
// `{"req_source":N}`——两个参考实现对 N 取值矛盾（1 / 2），故做成可配置，
// 不把任何一方的结论硬编码成默认。
func checkinBody(cfg *Config) map[string]any {
	if cfg != nil && cfg.CheckinReqSource != 0 {
		return map[string]any{"req_source": cfg.CheckinReqSource}
	}
	return nil
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
