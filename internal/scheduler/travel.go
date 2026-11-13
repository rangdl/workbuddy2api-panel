// travel.go 猫猫旅行巡检状态机：随旅行时点（travel_hours，默认 09 点）对池内每个可用账号单趟推进一次。
// 无猫 → 同意协议 + 领养；有猫 → 按 travel/status 分派 派出 / 领奖 / 跳过。
package scheduler

import (
	"encoding/json"
	"fmt"
	"log"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
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

	// blindboxDefaultMaxPerRun 盲盒单趟开盒次数上限的兜底值（配置未设/非法时）。
	// 与上游 max_open_count 的常见值一致；实际次数仍取三者最小值。
	blindboxDefaultMaxPerRun = 5
)

// travelAccountDelay 账号间限速：全量账号约 40s，避免上游风控。测试可置 0。
var travelAccountDelay = 800 * time.Millisecond

// activityAccountDelay 活跃上报账号间限速：与旅行同口径，避免上游风控。测试可置 0。
var activityAccountDelay = 800 * time.Millisecond

// adoptReportGap 领养前置上报后的等待：给上游事件处理留时间再发 buddy/first。
// 对齐 scripts/task_first_buddy.py 实测的 1.05s 间隔口径。测试可置 0。
var adoptReportGap = 1050 * time.Millisecond

// cstZone 上游每日重置按自然日 00:00 CST（Asia/Shanghai）。中国无夏令时，固定 +8 即可，
// 不依赖容器 tzdata。
var cstZone = time.FixedZone("CST", 8*60*60)

// travelDay 返回 t 所属的上游自然日（CST），格式 2006-01-02。
func travelDay(t time.Time) string {
	return t.In(cstZone).Format("2006-01-02")
}

// RunTravelNow 立即对池内所有可用账号执行一趟旅行巡检。
// 禁用账号跳过；暂停选号（paused）账号照常参与——旅行是纯 RPC（状态/派出/
// 领奖 + 领养前置上报），不发模型对话，与「让位防风控」不冲突；
// 401/查询失败只跳过该账号本轮（不强刷 token，交 22:00 keepalive）；
// 账号间限速 travelAccountDelay。
//
// 末尾 poke 到站定时器：本趟可能派出了新行程，定时器需要按新的 arrive_at 重算。
func (s *Scheduler) RunTravelNow() {
	s.travelMu.Lock()
	defer s.travelMu.Unlock()
	first := true
	for _, a := range s.travelAccounts() {
		if !first {
			time.Sleep(travelAccountDelay)
		}
		first = false
		s.travelOne(a)
	}
	s.PokeTravelTimer()
}

// travelAccounts 返回本轮可参与旅行的账号。
// 抽出来供批次巡检与到站定时器共用，避免两处门控漂移。
//
// 门控判据统一走 auth.SupportsPersonalGrowth（= !IsEnterprise）：
// 企业版没有个人成长体系，上游 growth 端点一律 403，执行即零收益。
//
// 历史教训（2026-11-13）：这里曾用 IsEnterpriseSession()，它会额外把
// JWT 带 token_source=enterprise_switch 的**个人号**也判成企业号并跳过。
// 实测该章只记录「本次授权走了哪条流程」，个人号走企业切换入口登录时同样携带，
// 且随 refresh 继承——表现为服务跑一段时间后旅行/盲盒突然全线停摆。
func (s *Scheduler) travelAccounts() []*auth.Auth {
	var out []*auth.Auth
	for _, st := range s.cfg.Pool.List() {
		if st.Disabled {
			continue
		}
		a := s.cfg.Pool.AuthByUID(st.UID)
		if a == nil || a.RefreshTokenValue() == "" {
			continue
		}
		if a.IsGlobal() {
			continue // D4 门控：global 无 CN 任务体系，不发起任何上游调用
		}
		if !a.SupportsPersonalGrowth() {
			continue // 企业版：growth 端点实测 403
		}
		out = append(out, a)
	}
	return out
}

// travelOne 单账号一趟状态机：查有无猫 + 查状态 + 至多 claim / 盲盒 / depart 各一次。
//
// 2026-11 改造（线上实证）：原实现每趟只做一个动作，而「派出 → 2h → 到站 → 领奖」
// 需要跨两个 travel_hours 窗口。飞牛 NAS 每天开关机，[9,21] 两个窗口几乎从不
// 同时命中，导致到站奖励挂了 19 天无人领。现在领奖成功后**重查 status**（不重查
// buddy/info：猫档案本趟不会变）——若已回到 idle 且未达当日上限，同趟继续派出。
func (s *Scheduler) travelOne(a *auth.Auth) {
	label := logfmt.Label(a.UID, a.NicknameValue())
	buddy, err := s.cfg.Upstream.BuddyInfo(a)
	if err != nil {
		log.Printf("travel %s: buddy-info: %v", label, err)
		return
	}
	if buddy == nil {
		s.travelAdopt(a)
		return
	}
	ts, err := s.cfg.Upstream.TravelStatus(a)
	if err != nil {
		log.Printf("travel %s: status: %v", label, err)
		return
	}
	if ts.State == travelStateArrived {
		if !s.travelClaim(a, ts) {
			return // 缺 record_id / 上游报错：状态未变，不猜下一步
		}
		// claim 后服务端可能置 idle（可再派出一趟），也可能因当日上限直接锁死；
		// 重新查询而不是假定，避免盲发 depart 撞业务错误。
		next, err := s.cfg.Upstream.TravelStatus(a)
		if err != nil {
			log.Printf("travel %s: status after claim: %v", label, err)
			return
		}
		ts = next
	}
	switch ts.State {
	case travelStateIdle:
		s.travelIdlePhase(a, ts)
	case travelStateTraveling:
		log.Printf("travel %s: skip (traveling record=%d)", label, ts.RecordID)
	case travelStateArrived:
		log.Printf("travel %s: skip (still arrived after claim, record=%d)", label, ts.RecordID)
	default:
		log.Printf("travel %s: skip (unknown state %q)", label, ts.State)
	}
}

// BlindboxResult 单账号盲盒处理结果（面板直接渲染，不让用户翻日志）。
type BlindboxResult struct {
	UID      string `json:"uid"`
	Nickname string `json:"nickname"`
	// 当前状态
	TravelState string `json:"travel_state"` // idle / traveling / arrived / unknown
	Energy      int64  `json:"energy"`       // 能量余额
	Affordable  int    `json:"affordable"`   // 上游算出的可开次数
	CostPerOpen int64  `json:"cost_per_open"`
	MaxOpen     int    `json:"max_open_count"`
	// 决策
	WillOpen int    `json:"will_open"` // 本次计划开几次（1..n；0 = 不开）
	Decision string `json:"decision"`  // 给用户看的一句话理由
	// 执行结果（apply 时）
	Opened bool   `json:"opened"`
	Prize  string `json:"prize,omitempty"`
	Err    string `json:"error,omitempty"`
}

// RunBlindboxNow 手动触发一轮能量盲盒（面板按钮）。
//
// 同步执行并返回逐账号结果——面板要直接展示「谁、为什么能开/不能开、开了几次」，
// 而不是让用户去日志里翻。账号间仍按 travelAccountDelay 限速，账号多时会慢，
// 但这是用户在面板上主动点的操作，等几秒比拿不到结果好。
//
// apply=false 为 dry-run：只查状态 + quota 并给出决策，不消耗任何能量。
func (s *Scheduler) RunBlindboxNow(apply bool, maxPerRun int) []BlindboxResult {
	if maxPerRun <= 0 {
		maxPerRun = blindboxDefaultMaxPerRun
	}
	s.travelMu.Lock()
	defer s.travelMu.Unlock()

	accounts := s.travelAccounts()
	results := make([]BlindboxResult, 0, len(accounts))
	for i, a := range accounts {
		if i > 0 {
			time.Sleep(travelAccountDelay)
		}
		label := logfmt.Label(a.UID, a.NicknameValue())
		r := BlindboxResult{UID: a.UID, Nickname: a.NicknameValue()}

		ts, err := s.cfg.Upstream.TravelStatus(a)
		if err != nil {
			r.TravelState = "unknown"
			r.Decision = "查询旅行状态失败"
			r.Err = err.Error()
			log.Printf("blindbox %s: status: %v", label, err)
			results = append(results, r)
			continue
		}
		r.TravelState = ts.State

		q, err := s.cfg.Upstream.BuddyQuota(a)
		if err != nil {
			r.Decision = "查询能量配额失败"
			r.Err = err.Error()
			log.Printf("blindbox %s: quota: %v", label, err)
			results = append(results, r)
			continue
		}
		r.Energy, r.Affordable = q.Balance, q.Affordable
		r.CostPerOpen, r.MaxOpen = q.CostPerOpen, q.MaxOpenCount

		// 决策（与 travelBlindBox 同一套门控，这里展开成可展示的理由）
		switch {
		case ts.State != travelStateIdle:
			r.Decision = "猫不在空闲状态（" + ts.State + "），需先领奖并结束旅行"
		case q.Affordable <= 0:
			r.Decision = "能量不足（需要 " + itoa(int(q.CostPerOpen)) + " 能量/次）"
		default:
			n := q.Affordable
			if q.MaxOpenCount > 0 && n > q.MaxOpenCount {
				n = q.MaxOpenCount
			}
			if n > maxPerRun {
				n = maxPerRun
			}
			r.WillOpen = n
			r.Decision = "可开 " + itoa(n) + " 次"
			if q.Affordable > n {
				r.Decision += "（受单趟上限钳制，剩余可下次再开）"
			}
		}

		log.Printf("blindbox %s: travel=%s balance=%d affordable=%d cost=%d max=%d → %s",
			label, r.TravelState, r.Energy, r.Affordable, r.CostPerOpen, r.MaxOpen, r.Decision)

		if !apply || r.WillOpen <= 0 {
			results = append(results, r)
			continue
		}

		raw, err := s.cfg.Upstream.BuddyOpen(a, r.WillOpen)
		if err != nil {
			r.Err = err.Error()
			r.Decision += "；开盒失败"
			log.Printf("blindbox %s: open failed: %v", label, err)
			results = append(results, r)
			continue
		}
		r.Opened = true
		r.Prize = blindboxPrizeName(raw)
		if r.Prize == "" {
			r.Prize = "（未返回奖品名）"
		}
		if blindboxNeedsAddress(raw) {
			r.Prize += "（实物奖，需到成长中心填写收件地址）"
		}
		log.Printf("blindbox %s: opened %d time(s) → %s", label, r.WillOpen, r.Prize)
		results = append(results, r)
	}
	return results
}

// itoa 小工具（避免为几处拼接引入 strconv 依赖到本文件）。
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}

// travelIdlePhase 空闲阶段：先开能量盲盒（服务端要求 idle），再派出。
//
// 顺序不能反：派出后猫进入 traveling，盲盒会被上游拒绝；而领奖后回到 idle 的
// 这一刻正是盲盒唯一可用的时机（每日至多一次）。
func (s *Scheduler) travelIdlePhase(a *auth.Auth, ts *upstream.TravelState) {
	if enabled, maxPerRun := s.travelBlindboxCfg(); enabled {
		s.travelBlindBox(a, ts, maxPerRun)
	}
	s.travelDepart(a, ts)
}

// travelDepart 空闲且未达当日上限时派出（每日 1 次，自然日 00:00 CST 重置）。
func (s *Scheduler) travelDepart(a *auth.Auth, ts *upstream.TravelState) {
	if ts.DailyLimitReached {
		log.Printf("travel %s: skip (daily limit reached)", logfmt.Label(a.UID, a.NicknameValue()))
		return
	}
	if err := s.cfg.Upstream.TravelDepart(a, travelLocationID); err != nil {
		log.Printf("travel %s: depart: %v", logfmt.Label(a.UID, a.NicknameValue()), err)
		return
	}
	log.Printf("travel %s: depart ok location=%d", logfmt.Label(a.UID, a.NicknameValue()), travelLocationID)
}

// travelClaim 到站领奖（必须带 record_id）；返回是否领奖成功（成功才值得重查状态）。
func (s *Scheduler) travelClaim(a *auth.Auth, ts *upstream.TravelState) bool {
	if ts.RecordID == 0 {
		log.Printf("travel %s: claim skipped (arrived but no record_id)", logfmt.Label(a.UID, a.NicknameValue()))
		return false
	}
	reward, err := s.cfg.Upstream.TravelClaim(a, ts.RecordID)
	if err != nil {
		log.Printf("travel %s: claim record=%d: %v", logfmt.Label(a.UID, a.NicknameValue()), ts.RecordID, err)
		return false
	}
	log.Printf("travel %s: claim ok record=%d reward=%d", logfmt.Label(a.UID, a.NicknameValue()), ts.RecordID, reward)
	return true
}

// travelBlindBox 开能量盲盒（消耗闲置 energy）。
//
// 门控（照参考实现 + 实测）：
//   - 旅行状态必须 idle：旅行中/到站未领时上游拒绝开盒；
//   - affordable <= 0 表示能量不足，直接跳过（省一次写请求）；
//   - 单次次数 = min(affordable, max_open_count, 配置上限)。
//
// 写请求不重试：服务端可能已扣能量，重试会重复消耗（probe_blindbox.py 同口径）。
func (s *Scheduler) travelBlindBox(a *auth.Auth, ts *upstream.TravelState, maxPerRun int) {
	label := logfmt.Label(a.UID, a.NicknameValue())
	if ts.State != travelStateIdle {
		log.Printf("blindbox %s: skip (travel state=%s, must be idle)", label, ts.State)
		return
	}
	q, err := s.cfg.Upstream.BuddyQuota(a)
	if err != nil {
		log.Printf("blindbox %s: quota: %v", label, err)
		return
	}
	log.Printf("blindbox %s: quota balance=%d affordable=%d cost=%d max=%d",
		label, q.Balance, q.Affordable, q.CostPerOpen, q.MaxOpenCount)
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
	raw, err := s.cfg.Upstream.BuddyOpen(a, count)
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
// 不是账号问题（report.go 注释亦明确「解锁 first_buddy 任务（领养前置）」）。
// conversation 门槛未达标仍属预期行为，记一次当日已试后静默跳过，不再重试。
func (s *Scheduler) travelAdopt(a *auth.Auth) {
	if s.adoptTriedToday(a.UID) {
		return
	}
	// 前置：解锁 first_buddy 任务（幂等；失败不阻塞，让 buddy/first 按既有错误路径暴露）。
	if err := s.cfg.Upstream.ReportChatActivity(a, fmt.Sprintf("wb2api-adopt-%d", time.Now().UnixMilli()), ""); err != nil {
		log.Printf("travel %s: adopt preflight report: %v", logfmt.Label(a.UID, a.NicknameValue()), err)
	} else {
		time.Sleep(adoptReportGap) // 给上游事件处理留时间（对齐脚本实测的 1.05s 间隔口径）
	}
	if err := s.cfg.Upstream.BuddyAgreement(a); err != nil {
		log.Printf("travel %s: agreement: %v", logfmt.Label(a.UID, a.NicknameValue()), err)
		return
	}
	err := s.cfg.Upstream.BuddyFirst(a)
	switch {
	case err == nil:
		log.Printf("travel %s: adopt ok (+300 credits)", logfmt.Label(a.UID, a.NicknameValue()))
	case upstream.IsBuddyTaskIncomplete(err):
		s.markAdoptTried(a.UID)
		log.Printf("travel %s: adopt skipped (conversation threshold not reached, retry tomorrow)", logfmt.Label(a.UID, a.NicknameValue()))
	default:
		log.Printf("travel %s: adopt: %v", logfmt.Label(a.UID, a.NicknameValue()), err)
	}
}

// adoptTriedToday 该账号当日是否已判定领养门槛未达。
func (s *Scheduler) adoptTriedToday(uid string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.adoptTried[uid] == travelDay(time.Now())
}

// markAdoptTried 记录该账号当日已尝试领养且未过门槛。
func (s *Scheduler) markAdoptTried(uid string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.adoptTried[uid] = travelDay(time.Now())
}
