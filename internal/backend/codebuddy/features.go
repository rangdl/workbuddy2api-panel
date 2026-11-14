// features.go CodeBuddy 账号的特色能力实现（旅行 / 盲盒 / 连登 / 夜猫子 / 补偿 / 活跃）。
//
// 这些玩法全部来自上游 growth 体系，只有 CodeBuddy 有。改造前它们散在
// internal/scheduler 的五个文件里，每个文件都自己遍历 Pool.List() + AuthByUID
// 取凭证，并且各自重写一遍门控（IsGlobal / SupportsPersonalGrowth /
// RefreshTokenValue）。
//
// 现在状态机原样搬到账号对象上：池只负责「找出实现了对应接口的账号并调用」，
// 三段门控收敛到下面的 capabilityGate——**新增一个上游只要实现同名接口即可
// 自动参与，池与调度器一行不用改**。
package codebuddy

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/gateway"
	"github.com/linguo2625469/workbuddy2api-panel/internal/logfmt"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

const (
	// travelLocationID 派出地点固定 4（古镇客栈）：4 个地点收益/时长区间完全相同，无最优解。
	travelLocationID = 4

	// travelStateIdle 空闲可派出；travelStateTraveling 在途；travelStateArrived 到站可领奖。
	travelStateIdle      = "idle"
	travelStateTraveling = "traveling"
	travelStateArrived   = "arrived"

	// blindboxDefaultMaxPerRun 盲盒单趟开盒次数上限的兜底值。
	blindboxDefaultMaxPerRun = 5
)

// 上游节奏参数（改造前在 scheduler 包，现随状态机一起搬过来）。
//
// 保留为包级变量而非常量：测试需要置 0 来避免真实等待。
var (
	// adoptReportGap 领养前置上报后的等待：给上游事件处理留时间再发 buddy/first。
	// 对齐 scripts/task_first_buddy.py 实测的 1.05s 间隔口径。
	adoptReportGap = 1050 * time.Millisecond
	// activityStreakSettle 活跃上报后回读 streak 前的等待。
	//
	// 上游行为事件计分是异步的：report 刚返回 200 就立刻回读，几乎必然读到旧值
	// （线上实测 2026-11-13：日志报「report OK but streak.days=0 (silent drop?)」，
	// 数秒后实查已是 days=1）。等待让服务端落账，避免把成功判成静默丢弃。
	activityStreakSettle = 8 * time.Second
)

// cstZone 上游每日重置按自然日 00:00 CST（Asia/Shanghai）。中国无夏令时，固定 +8 即可，
// 不依赖容器 tzdata。
var cstZone = time.FixedZone("CST", 8*60*60)

// travelDay 返回 t 所属的上游自然日（CST），格式 2006-01-02。
func travelDay(t time.Time) string {
	return t.In(cstZone).Format("2006-01-02")
}

// capabilityGate 判断本账号能否参与「个人成长体系」的特色玩法。
//
// 这是改造前散在 7 个调用点的三段门控，现在只有这一份：
//   - global：无 CN 任务体系（D4 门控），发起调用只会被拒或静默失败；
//   - 企业版：growth 端点一律 403（"growth system is only available for
//     personal users"），执行即零收益；
//   - 凭证缺失：无法续期 / 无法鉴权。
//
// 历史教训（2026-11-13）：曾用 IsEnterpriseSession() 做判定，它会额外把 JWT
// 带 token_source=enterprise_switch 的**个人号**也判成企业号并跳过。实测该章
// 只记录「本次授权走了哪条流程」，个人号走企业切换入口登录时同样携带，且随
// refresh 继承——表现为服务跑一段时间后旅行/盲盒突然全线停摆。所以统一走
// SupportsPersonalGrowth（只认 enterpriseId）。
//
// 凭证判据分两档，与改造前各任务的既有口径保持一致：
//   - 旅行/连登/夜猫子/补偿 看 RefreshTokenValue（它们可能触发 token 续期）；
//   - 活跃上报只看 AccessTokenValue（纯上报，不续期）。
func (x Account) capabilityGate() error {
	return x.gate(requireRefreshToken, false)
}

type credKind int

const (
	requireRefreshToken credKind = iota
	requireAccessToken
)

// gate 能力门控的实现。kind 决定用哪个凭证判据。
//
// allowGlobal 单独作为参数而不是由 kind 推导：**活跃上报是唯一允许 global 的
// 特色能力**（国际版 /v2/report 实测可用），其余（旅行/连登/夜猫子/补偿）在
// global 域没有对应任务体系，发起只会被拒或静默失败。
func (x Account) gate(kind credKind, allowGlobal bool) error {
	if x.client == nil || x.A == nil {
		return gateway.ErrUnsupported
	}
	switch kind {
	case requireRefreshToken:
		if x.A.RefreshTokenValue() == "" {
			return gateway.ErrUnsupported
		}
	default:
		if x.A.AccessTokenValue() == "" {
			return gateway.ErrUnsupported
		}
	}
	if !allowGlobal && x.A.IsGlobal() {
		return gateway.ErrUnsupported
	}
	if !x.A.SupportsPersonalGrowth() {
		return gateway.ErrUnsupported
	}
	return nil
}

// ── 旅行 ────────────────────────────────────────────────────────────

// Travel 一趟旅行巡检：查有无猫 + 查状态 + 至多 claim / 盲盒 / depart 各一次。
//
// 2026-11 改造（线上实证）：原实现每趟只做一个动作，而「派出 → 2h → 到站 → 领奖」
// 需要跨两个 travel_hours 窗口。飞牛 NAS 每天开关机，[9,21] 两个窗口几乎从不
// 同时命中，导致到站奖励挂了 19 天无人领。现在领奖成功后**重查 status**（不重查
// buddy/info：猫档案本趟不会变）——若已回到 idle 且未达当日上限，同趟继续派出。
func (x Account) Travel(ctx context.Context, opts gateway.TravelOptions) (gateway.TravelOutcome, error) {
	if err := x.capabilityGate(); err != nil {
		return gateway.TravelOutcome{}, err
	}
	_ = ctx // 上游调用本身不接受 ctx（既有签名），保留参数以符合接口。
	label := logfmt.Label(x.A.UID, x.A.NicknameValue())

	buddy, err := x.client.BuddyInfo(x.A)
	if err != nil {
		return gateway.TravelOutcome{Action: "none"}, fmt.Errorf("buddy-info: %w", err)
	}
	if buddy == nil {
		if opts.SkipAdopt {
			// 当日已判定门槛未达：跳过领养动作，但这次查询本身仍算有效巡检。
			return gateway.TravelOutcome{Action: "skipped", State: "no_buddy",
				Message: "adopt skipped today (threshold not reached earlier)"}, nil
		}
		out, err := x.travelAdopt(label)
		if out.RetryTomorrow {
			log.Printf("travel %s: %s", label, out.Message)
		}
		return out, err
	}
	ts, err := x.client.TravelStatus(x.A)
	if err != nil {
		return gateway.TravelOutcome{Action: "none", State: "unknown"}, fmt.Errorf("status: %w", err)
	}
	if ts.State == travelStateArrived {
		claimed, reward, ok := x.travelClaim(label, ts)
		if !ok {
			// 缺 record_id / 上游报错：状态未变，不猜下一步。
			return gateway.TravelOutcome{Action: "none", State: ts.State}, nil
		}
		_ = claimed
		// claim 后服务端可能置 idle（可再派出一趟），也可能因当日上限直接锁死；
		// 重新查询而不是假定，避免盲发 depart 撞业务错误。
		next, err := x.client.TravelStatus(x.A)
		if err != nil {
			return gateway.TravelOutcome{Action: "claimed", State: ts.State, Reward: reward},
				fmt.Errorf("status after claim: %w", err)
		}
		ts = next
		if ts.State != travelStateIdle {
			return gateway.TravelOutcome{Action: "claimed", State: ts.State, Reward: reward}, nil
		}
		// 领奖后回到 idle：同趟继续（盲盒 → 派出）。
		out := x.travelIdlePhase(label, ts, opts)
		out.Action = "claimed"
		out.Reward = reward
		return out, nil
	}
	switch ts.State {
	case travelStateIdle:
		return x.travelIdlePhase(label, ts, opts), nil
	case travelStateTraveling:
		return gateway.TravelOutcome{Action: "skipped", State: ts.State,
			Message: fmt.Sprintf("traveling record=%d", ts.RecordID)}, nil
	default:
		return gateway.TravelOutcome{Action: "skipped", State: ts.State,
			Message: fmt.Sprintf("unknown state %q", ts.State)}, nil
	}
}

// TravelState 只读查询当前行程（到站定时器用）。
func (x Account) TravelState(ctx context.Context) (gateway.TravelSnapshot, error) {
	if err := x.capabilityGate(); err != nil {
		return gateway.TravelSnapshot{}, err
	}
	_ = ctx
	ts, err := x.client.TravelStatus(x.A)
	if err != nil {
		return gateway.TravelSnapshot{}, err
	}
	return gateway.TravelSnapshot{
		State:             ts.State,
		RecordID:          ts.RecordID,
		ArriveAt:          ts.ArriveAt,
		ServerNow:         ts.ServerNow,
		DailyLimitReached: ts.DailyLimitReached,
	}, nil
}

// travelIdlePhase 空闲阶段：先开能量盲盒（服务端要求 idle），再派出。
//
// 顺序不能反：派出后猫进入 traveling，盲盒会被上游拒绝；而领奖后回到 idle 的
// 这一刻正是盲盒唯一可用的时机（每日至多一次）。
func (x Account) travelIdlePhase(label string, ts *upstream.TravelState,
	opts gateway.TravelOptions) gateway.TravelOutcome {
	// 盲盒开关与次数上限来自调度策略（opts），账号不自己读配置。
	if opts.BlindboxEnabled {
		max := opts.BlindboxMaxPerRun
		if max <= 0 {
			max = blindboxDefaultMaxPerRun
		}
		x.travelBlindBox(label, ts, max)
	}

	if ts.DailyLimitReached {
		return gateway.TravelOutcome{Action: "skipped", State: ts.State,
			Message: "daily limit reached"}
	}
	if err := x.client.TravelDepart(x.A, travelLocationID); err != nil {
		return gateway.TravelOutcome{Action: "none", State: ts.State,
			Message: fmt.Sprintf("depart: %v", err)}
	}
	return gateway.TravelOutcome{Action: "departed", State: ts.State,
		Message: fmt.Sprintf("location=%d", travelLocationID)}
}

// travelClaim 到站领奖（必须带 record_id）；返回是否领奖成功与奖励额度。
func (x Account) travelClaim(label string, ts *upstream.TravelState) (bool, int64, bool) {
	if ts.RecordID == 0 {
		log.Printf("travel %s: claim skipped (arrived but no record_id)", label)
		return false, 0, false
	}
	reward, err := x.client.TravelClaim(x.A, ts.RecordID)
	if err != nil {
		log.Printf("travel %s: claim record=%d: %v", label, ts.RecordID, err)
		return false, 0, false
	}
	log.Printf("travel %s: claim ok record=%d reward=%d", label, ts.RecordID, reward)
	return true, reward, true
}

// travelBlindBox 开能量盲盒（消耗闲置 energy）。
//
// 门控（照参考实现 + 实测）：
//   - 旅行状态必须 idle：旅行中/到站未领时上游拒绝开盒；
//   - affordable <= 0 表示能量不足，直接跳过（省一次写请求）；
//   - 单次次数 = min(affordable, max_open_count, 配置上限)。
//
// 写请求不重试：服务端可能已扣能量，重试会重复消耗（probe_blindbox.py 同口径）。
func (x Account) travelBlindBox(label string, ts *upstream.TravelState, maxPerRun int) {
	if ts.State != travelStateIdle {
		log.Printf("blindbox %s: skip (travel state=%s, must be idle)", label, ts.State)
		return
	}
	q, err := x.client.BuddyQuota(x.A)
	if err != nil {
		log.Printf("blindbox %s: quota: %v", label, err)
		return
	}
	if q.Affordable <= 0 {
		log.Printf("blindbox %s: skip (energy insufficient, affordable=0)", label)
		return
	}
	count := q.Affordable
	// max_open_count 缺失（0）时按 1 处理：宁少开不多开，剩余额度下一趟再开。
	serverMax := q.MaxOpenCount
	if serverMax <= 0 {
		serverMax = 1
	}
	if count > serverMax {
		count = serverMax
	}
	if maxPerRun > 0 && count > maxPerRun {
		count = maxPerRun
	}
	raw, err := x.client.BuddyOpen(x.A, count)
	if err != nil {
		log.Printf("blindbox %s: open failed: %v", label, err)
		return
	}
	if name := blindboxPrizeName(raw); name != "" {
		log.Printf("blindbox %s: opened %d time(s) → %s", label, count, name)
	} else {
		log.Printf("blindbox %s: opened %d time(s)", label, count)
	}
	if blindboxNeedsAddress(raw) {
		log.Printf("blindbox %s: 实物奖，需到成长中心填写收件地址", label)
	}
}

// blindboxPrizeName 从开盒载荷里尽力提取猫名（形状随活动期变化，取不到就返回空）。
// 兼容 {"buddy":{"name":...}} / {"buddy":"名字"} / {"buddies":[{...}]} / {"name":...}。
func blindboxPrizeName(raw json.RawMessage) string {
	var m map[string]any
	if len(raw) == 0 || json.Unmarshal(raw, &m) != nil {
		return ""
	}
	if v, ok := m["buddy"]; ok {
		switch b := v.(type) {
		case string:
			return b
		case map[string]any:
			if n, ok := b["name"].(string); ok {
				return n
			}
		}
	}
	if list, ok := m["buddies"].([]any); ok && len(list) > 0 {
		if b, ok := list[0].(map[string]any); ok {
			if n, ok := b["name"].(string); ok {
				return n
			}
		}
	}
	if n, ok := m["name"].(string); ok {
		return n
	}
	return ""
}

// blindboxNeedsAddress 载荷是否提示中实物奖（需自行填收件地址）。
func blindboxNeedsAddress(raw json.RawMessage) bool {
	var m map[string]any
	if len(raw) == 0 || json.Unmarshal(raw, &m) != nil {
		return false
	}
	for _, k := range []string{"need_address", "require_address"} {
		if v, ok := m[k].(bool); ok && v {
			return true
		}
	}
	return false
}

// travelAdopt 无猫时领养，链路：report → agreement → buddy/first。
//
// report 必须先跑（scripts/task_first_buddy.py 实测）：一条 chat_request_send 上报
// 点亮 growth 连登并**解锁 first_buddy 任务**；未上报时 buddy/first 会返回
// 400 "first_buddy task not completed yet"——该门槛的真实来源是"当日无活跃上报"，
// 不是账号问题。
//
// conversation 门槛未达标仍属预期行为：本趟静默跳过，不重复重试。
// （改造前用进程内 adoptTried 表做当日去重；现在每次巡检都重新尝试一次
// 前置上报——它是幂等的，多发的成本只是一次 report RPC，换来的是账号对象
// 不再持有跨调用的可变状态。）
func (x Account) travelAdopt(label string) (gateway.TravelOutcome, error) {
	if err := x.client.ReportChatActivity(x.A,
		fmt.Sprintf("wb2api-adopt-%d", time.Now().UnixMilli()), ""); err != nil {
		log.Printf("travel %s: adopt preflight report: %v", label, err)
	} else {
		time.Sleep(adoptReportGap) // 给上游事件处理留时间（对齐脚本实测的 1.05s 间隔口径）
	}
	if err := x.client.BuddyAgreement(x.A); err != nil {
		return gateway.TravelOutcome{Action: "none"}, fmt.Errorf("agreement: %w", err)
	}
	err := x.client.BuddyFirst(x.A)
	switch {
	case err == nil:
		return gateway.TravelOutcome{Action: "adopted", Message: "adopt ok (+300 credits)"}, nil
	case upstream.IsBuddyTaskIncomplete(err):
		return gateway.TravelOutcome{Action: "skipped", RetryTomorrow: true,
			Message: "adopt skipped (conversation threshold not reached, retry tomorrow)"}, nil
	default:
		return gateway.TravelOutcome{Action: "none"}, fmt.Errorf("buddy/first: %w", err)
	}
}

// ── 盲盒（独立入口，供面板手动触发）────────────────────────────────

// Blindbox 单个账号一轮盲盒处理。
//
// apply=false 为 dry-run：只查状态 + quota 并给出决策，不消耗任何能量。
// maxPerRun<=0 时用 blindboxDefaultMaxPerRun。
func (x Account) Blindbox(ctx context.Context, apply bool, maxPerRun int) (gateway.BlindboxOutcome, error) {
	if err := x.capabilityGate(); err != nil {
		return gateway.BlindboxOutcome{}, err
	}
	_ = ctx
	if maxPerRun <= 0 {
		maxPerRun = blindboxDefaultMaxPerRun
	}
	label := logfmt.Label(x.A.UID, x.A.NicknameValue())
	out := gateway.BlindboxOutcome{}

	ts, err := x.client.TravelStatus(x.A)
	if err != nil {
		out.TravelState = "unknown"
		out.Decision = "查询旅行状态失败"
		return out, err
	}
	out.TravelState = ts.State

	q, err := x.client.BuddyQuota(x.A)
	if err != nil {
		out.Decision = "查询能量配额失败"
		return out, err
	}
	out.Energy, out.Affordable = q.Balance, q.Affordable
	out.CostPerOpen, out.MaxOpen = q.CostPerOpen, q.MaxOpenCount

	// 决策（与 travelBlindBox 同一套门控，这里展开成可展示的理由）
	switch {
	case ts.State != travelStateIdle:
		out.Decision = "猫不在空闲状态（" + ts.State + "），需先领奖并结束旅行"
	case q.Affordable <= 0:
		out.Decision = "能量不足（需要 " + itoa(int(q.CostPerOpen)) + " 能量/次）"
	default:
		n := q.Affordable
		if q.MaxOpenCount > 0 && n > q.MaxOpenCount {
			n = q.MaxOpenCount
		}
		if n > maxPerRun {
			n = maxPerRun
		}
		out.WillOpen = n
		out.Decision = "可开 " + itoa(n) + " 次"
		if q.Affordable > n {
			out.Decision += "（受单趟上限钳制，剩余可下次再开）"
		}
	}
	if !apply || out.WillOpen <= 0 {
		return out, nil
	}
	raw, err := x.client.BuddyOpen(x.A, out.WillOpen)
	if err != nil {
		out.Decision += "；开盒失败"
		return out, err
	}
	out.Opened = true
	out.Prize = blindboxPrizeName(raw)
	if out.Prize == "" {
		out.Prize = "（未返回奖品名）"
	}
	if blindboxNeedsAddress(raw) {
		out.Prize += "（实物奖，需到成长中心填写收件地址）"
	}
	log.Printf("blindbox %s: opened %d time(s) → %s", label, out.WillOpen, out.Prize)
	return out, nil
}

// ── 连登管家 ────────────────────────────────────────────────────────

// RunStreak 补签保连登 → 礼包/补偿 → 兑换所有已解锁档位 → 抽完所有 chances。
//
// 幂等：locked/claimed 档位与无次数抽奖自动跳过。
func (x Account) RunStreak(ctx context.Context) (gateway.StreakOutcome, error) {
	if err := x.capabilityGate(); err != nil {
		return gateway.StreakOutcome{}, err
	}
	_ = ctx
	label := logfmt.Label(x.A.UID, x.A.NicknameValue())
	out := gateway.StreakOutcome{}

	// 0. 补签保连登：昨日漏签且有补签卡则补上（连续天数一断就要重攒 7 天）。
	x.makeupYesterday(label)
	// 0.5 礼包/补偿（每号一次，无则业务错误静默跳过）。
	if credit, err := x.client.ClaimGift(x.A); err == nil {
		log.Printf("streak-bonus %s: 🎊 新手礼包 +%dc", label, credit)
	}
	x.claimCompensationQuiet(label)

	full, err := x.client.GrowthStreakFull(x.A)
	if err != nil {
		return out, err
	}
	out.Days = full.Streak.Days
	statuses := map[string]string{
		"7d":  full.RedemptionStatus.Tier7dStatus,
		"14d": full.RedemptionStatus.Tier14dStatus,
		"28d": full.RedemptionStatus.Tier28dStatus,
	}
	for _, tier := range full.RedemptionStatus.Tiers {
		status := statuses[tier.Tier]
		if status == "locked" || status == "claimed" {
			continue
		}
		if err := x.client.GrowthRedeemTier(x.A, tier.Tier); err != nil {
			// 未解锁（403）属预期，静默；其余记日志。
			log.Printf("streak-bonus %s: redeem %s: %v", label, tier.Tier, err)
			continue
		}
		out.RedeemedTiers = append(out.RedeemedTiers, tier.Tier)
		out.Reward += int64(tier.Credit)
		log.Printf("streak-bonus %s: ★ 兑换 %s 档（+%dc +%de 卡×%d 抽奖×%d）",
			label, tier.Tier, tier.Credit, tier.Energy, tier.Cards, tier.Chances)
	}
	// 抽奖：按当前 chances 全抽完（兑换刚发的次数已在服务端累加）。
	chances, err := x.client.LotteryChances(x.A)
	if err != nil {
		return out, fmt.Errorf("lottery summary: %w", err)
	}
	for i := 0; i < chances; i++ {
		raw, err := x.client.LotteryDraw(x.A)
		if err != nil {
			return out, fmt.Errorf("draw: %w", err)
		}
		out.LotteryDraws++
		log.Printf("streak-bonus %s: 🎲 第%d抽 %s", label, i+1, compactJSON(raw))
	}
	if chances > 0 {
		log.Printf("streak-bonus %s: 抽奖完成 %d 次", label, chances)
	}
	return out, nil
}

// makeupYesterday 昨日漏签且有补签卡时自动补签（保住连登连续天数）。
// 无卡 / 无漏签 / 查询失败均静默（不影响主流程）。
func (x Account) makeupYesterday(label string) {
	missed, err := x.client.HeatmapYesterdayMissed(x.A)
	if err != nil || !missed {
		return
	}
	full, err := x.client.GrowthStreakFull(x.A)
	if err != nil || full.MakeupCards.Balance <= 0 {
		return
	}
	yesterday := time.Now().AddDate(0, 0, -1).Format("2006-01-02")
	if err := x.client.UseMakeupCard(x.A, yesterday); err != nil {
		log.Printf("streak-bonus %s: 补签 %s 失败: %v", label, yesterday, err)
		return
	}
	log.Printf("streak-bonus %s: ★ 已用补签卡补签 %s（保连登）", label, yesterday)
}

// compactJSON 裁剪奖品载荷（日志单行可读）。
func compactJSON(raw json.RawMessage) string {
	s := string(raw)
	if len(s) > 220 {
		return s[:220] + "…"
	}
	return s
}

// ── 夜猫子 ──────────────────────────────────────────────────────────

// RunBlackcat 夜间补足对话并上报事件链（black_cat 判据）。
//
// 注意这是特色能力里唯一**会发真实模型对话**的（逐条 ChatStream 发短对话），
// 池因此对 paused 账号跳过本能力（见 pool.BlackcatAll）。
func (x Account) RunBlackcat(ctx context.Context) (gateway.BlackcatOutcome, error) {
	if err := x.capabilityGate(); err != nil {
		return gateway.BlackcatOutcome{}, err
	}
	_ = ctx
	label := logfmt.Label(x.A.UID, x.A.NicknameValue())
	need, err := x.client.BlackcatNeed(x.A)
	if err != nil {
		return gateway.BlackcatOutcome{}, err
	}
	if need <= 0 {
		return gateway.BlackcatOutcome{Need: 0, Message: "无需补足"}, nil
	}
	ok, err := x.client.RunNightChats(x.A, int(need))
	out := gateway.BlackcatOutcome{Need: need, Completed: ok}
	if err != nil {
		return out, fmt.Errorf("%d/%d 完成，中断: %w", ok, need, err)
	}
	_ = label
	return out, nil
}

// ── 补偿 ────────────────────────────────────────────────────────────

// 补偿任务的跳过原因（面板据此渲染，勿随意改字面量）。
const (
	compSkipInactive   = "inactive"    // 当前无生效中的补偿活动
	compSkipClaimed    = "claimed"     // 本次活动已领过
	compSkipZeroCredit = "zero_credit" // 活动在，但额度为 0
)

// CompensationStatus 只读查询补偿状态，不领取。
func (x Account) CompensationStatus(ctx context.Context) (gateway.CompensationOutcome, error) {
	// 补偿是纯 billing 域 RPC，不触发 token 续期，因此只要 accessToken。
	if err := x.gate(requireAccessToken, false); err != nil {
		return gateway.CompensationOutcome{}, err
	}
	_ = ctx
	st, err := x.client.GetCompensationStatus(x.A)
	if err != nil {
		return gateway.CompensationOutcome{}, err
	}
	return toCompensationOutcome(st), nil
}

// CompensationClaim 查状态后「待领才领」（幂等）。
//
// 判据用 upstream 的 Pending()（唯一权威实现），不在这里复刻 active/claimed/
// credit 的与门——此前三处拷贝（scheduler / Pending / 前端）已经开始漂移。
func (x Account) CompensationClaim(ctx context.Context) (gateway.CompensationOutcome, error) {
	if err := x.gate(requireAccessToken, false); err != nil {
		return gateway.CompensationOutcome{}, err
	}
	_ = ctx
	st, err := x.client.GetCompensationStatus(x.A)
	if err != nil {
		return gateway.CompensationOutcome{}, err
	}
	out := toCompensationOutcome(st)
	if !st.Pending() {
		switch {
		case !st.Active:
			out.Skip, out.Message = compSkipInactive, "当前无生效补偿"
		case st.Claimed:
			out.Skip, out.Message = compSkipClaimed, "本次活动已领取"
		default:
			out.Skip, out.Message = compSkipZeroCredit, "活动在但额度为 0"
		}
		return out, nil
	}
	credit, err := x.client.ClaimCompensation(x.A)
	if err != nil {
		return out, err
	}
	out.ClaimedNow, out.Credit = true, credit
	return out, nil
}

// toCompensationOutcome 把上游结构转成跨上游的归一结果。
func toCompensationOutcome(st upstream.CompensationStatus) gateway.CompensationOutcome {
	return gateway.CompensationOutcome{
		Active:    st.Active,
		Claimed:   st.Claimed,
		CreditNum: st.CreditNum,
		EndTime:   st.EndTime,
	}
}

// claimCompensationQuiet 连登流程用：待领才领，无待领不记日志（避免每日刷屏）。
func (x Account) claimCompensationQuiet(label string) {
	st, err := x.client.GetCompensationStatus(x.A)
	if err != nil || !st.Pending() {
		return
	}
	if credit, err := x.client.ClaimCompensation(x.A); err == nil {
		log.Printf("compensation %s: 🎊 补偿领取 +%dc（截止 %s）", label, credit, st.EndTime)
	} else {
		log.Printf("compensation %s: 待领 %dc 但领取失败: %v", label, st.CreditNum, err)
	}
}

// ── 活跃上报 ────────────────────────────────────────────────────────

// ReportActivity 上报一条活跃事件，并在短暂等待后回读连登天数。
//
// 企业版跳过：/v2/report 本身返回 200，但它唯一的作用是点亮 growth 连登天数 /
// 解锁 first_buddy 领养——这两者对企业号都是 403，即**零收益**。该类"静默无效"
// 最易被忽略（日志无异常），故显式跳过。
//
// global **不**跳过：国际版 /v2/report 实测可用（code=0），跳过会让国际版账号
// 永远点不亮连登。
func (x Account) ReportActivity(ctx context.Context) (gateway.ActivityOutcome, error) {
	// 活跃上报只看 accessToken：纯上报，不触发续期，所以不要求 refreshToken。
	// allowGlobal=true：国际版 /v2/report 实测可用（code=0），跳过会让国际版
	// 账号永远点不亮连登（上游 a190252 同口径修复）。
	if err := x.gate(requireAccessToken, true); err != nil {
		return gateway.ActivityOutcome{}, err
	}
	cid := fmt.Sprintf("wb2api-%d", time.Now().UnixMilli())
	if err := x.client.ReportChatActivity(x.A, cid, ""); err != nil {
		return gateway.ActivityOutcome{}, err
	}
	// 上游计分是异步的：立即回读几乎必然读到旧值，先等 settle 让服务端落账。
	select {
	case <-time.After(activityStreakSettle):
	case <-ctx.Done():
		return gateway.ActivityOutcome{Message: "上报成功（ctx 取消，未回读 streak）"}, nil
	}
	days, err := x.client.GrowthStreak(x.A)
	if err != nil {
		return gateway.ActivityOutcome{Message: "report OK, streak check failed"}, nil
	}
	if days == 0 {
		return gateway.ActivityOutcome{StreakDays: 0,
			Message: "report OK but streak.days=0 (silent drop?)"}, nil
	}
	return gateway.ActivityOutcome{StreakDays: days,
		Message: fmt.Sprintf("streak days=%d", days)}, nil
}

// 编译期断言：本包的账号实现全部特色能力接口。
//
// 新增能力时**必须**在这里补一行——否则「实现了但没被池调用」这种最隐蔽的
// 漏接会在运行期静默存在。
var (
	_ gateway.Traveler         = Account{}
	_ gateway.Blindboxer       = Account{}
	_ gateway.StreakRunner     = Account{}
	_ gateway.BlackcatRunner   = Account{}
	_ gateway.Compensator      = Account{}
	_ gateway.ActivityReporter = Account{}
)
