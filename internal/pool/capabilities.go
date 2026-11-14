// capabilities.go 池的特色能力批量调度。
//
// 与 operations.go 的常规能力（RefreshCredits / CheckinAll / KeepaliveAll）分工：
//
//	常规能力  ── Account 必备方法，不支持返回 ErrUnsupported（池跳过）
//	特色能力  ── 本文件，gateway 的可选接口（Traveler / Blindboxer / ...）
//	             账号实现了就自动参与，没实现就静默跳过
//
// 共同点：池不认识任何具体上游，也不做「是不是 Trae」这类判断。新增一个上游时
// 只要它的账号对象实现了对应接口，这里的全部流程自动生效，池与调度器一行不改。
//
// 三类调度模式（由各能力自身的上游风控特性决定，不是实现细节）：
//
//	并发（Concurrency>0）── 签到/余额这类轻量 RPC，4 并发无风控压力
//	串行 + 延迟          ── 旅行/连登/补偿，账号间 800ms 防风控
//	串行 + ctx 可中断     ── 活跃上报/夜猫子（长流程，停机要能立刻退出）
package pool

import (
	"context"
	"errors"
	"log"
	"sync"
	"sync/atomic"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/gateway"
	"github.com/linguo2625469/workbuddy2api-panel/internal/logfmt"
)

// capOptions 特色能力批量执行的参数。
//
// 与 MemberFilter 的区别：MemberFilter 决定「谁参与」，capOptions 决定
// 「怎么跑」。分开是因为调用方经常需要「同一个过滤条件、不同的执行节奏」。
type capOptions struct {
	// IncludeDisabled 已禁用账号是否参与。
	//
	// 缺省 false 是安全选择：禁用通常意味着凭证已被上游判定失效（401 / 设备不匹配），
	// 继续发请求只会拿到同一批错误。
	IncludeDisabled bool
	// IncludePaused 暂停选号账号是否参与（缺省 false）。
	//
	// paused 只关选号、不关保号。**纯 RPC** 类特色能力（旅行/盲盒/连登/补偿）
	// 应显式置 true——它们不吃对话配额，与「让位防风控」不冲突；
	// **真实对话**类（夜猫子）保持 false。
	IncludePaused bool
	// Concurrency 并发数。0 或 1 = 串行；>1 = 并发（此时 Delay 无效）。
	Concurrency int
	// Delay 串行模式下账号之间的等待（防风控）。并发模式下忽略。
	Delay time.Duration
	// StopOnCancel 为 true 时，ctx 取消后立即停止派发后续账号
	// （长流程任务用；短 RPC 任务用 false 让已开始的请求跑完）。
	StopOnCancel bool
}

// capableAccounts 取出参与特色能力调度的账号快照（供需要自定义执行模型的调用方）。
func (p *Pool) capableAccounts(opts capOptions) []gateway.Account {
	return accountsOf(p.Members(MemberFilter{
		IncludeDisabled: opts.IncludeDisabled,
		IncludePaused:   opts.IncludePaused,
	}))
}

// forEachCapable 遍历满足条件的账号，对实现了对应能力接口的账号调用 fn。
//
// 三个静默跳过的情形（都不算失败、不记错误）：
//  1. 账号没实现该能力接口 —— 这个上游没有这个玩法；
//  2. fn 返回 gateway.ErrUnsupported —— 这个账号不适用（企业版 / global / 无凭证）；
//  3. 账号在遍历期间被移除 —— 快照里的对象仍然可用，但跳过更安全。
//
// 回调必须自己保证并发安全（并发模式下 fn 会被多个 goroutine 同时调用）。
func (p *Pool) forEachCapable(ctx context.Context, opts capOptions, fn func(gateway.Account)) {
	accs := p.capableAccounts(opts)
	if len(accs) == 0 {
		return
	}
	// 并发模式：固定 worker 数，避免账号多时一次性拉起 N 个 goroutine。
	if opts.Concurrency > 1 {
		sem := make(chan struct{}, opts.Concurrency)
		var wg sync.WaitGroup
		for _, acc := range accs {
			if opts.StopOnCancel && ctx.Err() != nil {
				break
			}
			wg.Add(1)
			go func(a gateway.Account) {
				defer wg.Done()
				select {
				case sem <- struct{}{}:
					defer func() { <-sem }()
				case <-ctx.Done():
					return
				}
				fn(a)
			}(acc)
		}
		wg.Wait()
		return
	}
	// 串行模式：账号间延迟；StopOnCancel 时取消即停。
	for i, acc := range accs {
		if i > 0 && opts.Delay > 0 {
			if opts.StopOnCancel {
				select {
				case <-time.After(opts.Delay):
				case <-ctx.Done():
					return
				}
			} else {
				time.Sleep(opts.Delay)
			}
		}
		if opts.StopOnCancel && ctx.Err() != nil {
			return
		}
		fn(acc)
	}
}

// ── 特色能力批量方法 ────────────────────────────────────────────────
//
// 与上方「常规能力」保持命名风格一致：XxxAll 表示「对池内所有适用账号执行一遍」。
// 返回值都是逐账号结果切片（面板直接渲染），顺序与池的稳定排序一致。

// TravelResult 单账号旅行巡检结果（面板/日志用）。
type TravelResult struct {
	UID      string
	Nickname string
	Outcome  gateway.TravelOutcome
	Err      error
	// Unsupported 该账号不参与（企业版 / global / 无凭证），非失败。
	Unsupported bool
}

// TravelAll 对池内所有适用账号执行一趟旅行巡检（串行 800ms 防风控）。
//
// paused 账号照常参与：旅行是纯 RPC（状态/派出/领奖 + 领养前置上报），
// 不发模型对话，与「让位防风控」不冲突。
//
// 当日去重：账号返回 RetryTomorrow 时记入当日台账，当天剩余巡检跳过**领养**动作
// （仍查有无猫与行程状态——猫可能已被领养、行程可能已到站需要领奖）。
func (p *Pool) TravelAll(ctx context.Context) []TravelResult {
	return p.TravelAllWith(ctx, gateway.TravelOptions{})
}

// TravelAllWith 同 TravelAll，但带上调度策略（盲盒开关 / 次数上限）。
//
// 这些是用户配置的调度策略，池与账号都不该自己去读配置——由调用方传进来。
func (p *Pool) TravelAllWith(ctx context.Context, base gateway.TravelOptions) []TravelResult {
	var out []TravelResult
	var mu sync.Mutex
	today := p.travelDay(time.Now())
	p.forEachCapable(ctx, capOptions{
		IncludePaused: true,
		Concurrency:   1,
		Delay:         capDelay(ctx),
		StopOnCancel:  true,
	}, func(acc gateway.Account) {
		r := TravelResult{UID: acc.UID(), Nickname: acc.Nickname()}
		t, ok := acc.(gateway.Traveler)
		if !ok {
			return // 该上游没有旅行玩法：静默跳过
		}
		opts := base
		opts.SkipAdopt = p.travelRetryTomorrow(acc.UID(), today)
		outcome, err := t.Travel(ctx, opts)
		switch {
		case errors.Is(err, gateway.ErrUnsupported):
			r.Unsupported = true
		case err != nil:
			r.Err = err
			log.Printf("travel %s: %v", logfmt.Label(r.UID, r.Nickname), err)
		default:
			r.Outcome = outcome
			if outcome.RetryTomorrow {
				p.markTravelRetryTomorrow(acc.UID(), today)
			}
			logTravelOutcome(r)
		}
		mu.Lock()
		out = append(out, r)
		mu.Unlock()
	})
	p.pruneTravelRetryTomorrow(today)
	return out
}

// travelDay 上游自然日（CST，固定 +8，不依赖容器 tzdata）。
//
// 与 codebuddy 包的同名工具函数口径一致；池不能 import 那个包，故各留一份。
func (p *Pool) travelDay(t time.Time) string {
	return t.In(time.FixedZone("CST", 8*60*60)).Format("2006-01-02")
}

// travelRetryTomorrow 该账号当日是否已被判定「明天再试」。
func (p *Pool) travelRetryTomorrow(uid, day string) bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.travelRetry[uid] == day
}

// markTravelRetryTomorrow 记录该账号当日不再参与旅行巡检。
func (p *Pool) markTravelRetryTomorrow(uid, day string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.travelRetry == nil {
		p.travelRetry = map[string]string{}
	}
	p.travelRetry[uid] = day
}

// pruneTravelRetryTomorrow 清掉过期的当日记录（防账号长期不参与时 map 无限增长）。
func (p *Pool) pruneTravelRetryTomorrow(day string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for uid, d := range p.travelRetry {
		if d != day {
			delete(p.travelRetry, uid)
		}
	}
}

// ResetTravelRetryForTest 把旅行当日台账整体改成指定日期（仅测试用）。
//
// 生产代码不应调用：它破坏了「当日不重试」的语义。测试用它模拟「跨自然日」，
// 避免把系统时钟拨来拨去。
func (p *Pool) ResetTravelRetryForTest(day string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for uid := range p.travelRetry {
		p.travelRetry[uid] = day
	}
}

// logTravelOutcome 把一次旅行结果写成一行日志（保持与改造前同样的可读性）。
func logTravelOutcome(r TravelResult) {
	label := logfmt.Label(r.UID, r.Nickname)
	o := r.Outcome
	switch o.Action {
	case "adopted":
		log.Printf("travel %s: adopt ok", label)
	case "departed":
		log.Printf("travel %s: depart ok", label)
	case "claimed":
		log.Printf("travel %s: claim ok reward=%d", label, o.Reward)
	case "skipped":
		if o.Message != "" {
			log.Printf("travel %s: skip (%s)", label, o.Message)
		}
	default:
		if o.Message != "" {
			log.Printf("travel %s: %s", label, o.Message)
		}
	}
}

// BlindboxResult 单账号盲盒结果（面板直接渲染，字段名是前端契约）。
//
// gateway.BlindboxOutcome 以**嵌入**方式摊平：JSON 序列化后前端拿到的仍是
// travel_state / energy / affordable / cost_per_open / max_open / will_open /
// decision / opened / prize 这些平铺字段（改造前 scheduler 给的就是这个形状）。
// 嵌入而非嵌套，避免前端把 `results[].outcome.will_open` 改成两层。
type BlindboxResult struct {
	UID      string `json:"uid"`
	Nickname string `json:"nickname"`
	gateway.BlindboxOutcome
	Err error `json:"-"`
	// Error 失败原因（面板展示用；成功为空）。
	Error string `json:"error,omitempty"`
}

// BlindboxAll 对所有适用账号执行一轮能量盲盒（串行 800ms）。
//
// apply=false 为 dry-run：只查状态与配额并给出决策，不消耗能量。
// 返回逐账号结果——面板要直接展示「谁、为什么能开/不能开、开了几次」，
// 而不是让用户去日志里翻。
func (p *Pool) BlindboxAll(ctx context.Context, apply bool, maxPerRun int) []BlindboxResult {
	var out []BlindboxResult
	p.forEachCapable(ctx, capOptions{
		IncludePaused: true,
		Concurrency:   1,
		Delay:         capDelay(ctx),
		StopOnCancel:  true,
	}, func(acc gateway.Account) {
		r := BlindboxResult{UID: acc.UID(), Nickname: acc.Nickname()}
		b, ok := acc.(gateway.Blindboxer)
		if !ok {
			return
		}
		outcome, err := b.Blindbox(ctx, apply, maxPerRun)
		if errors.Is(err, gateway.ErrUnsupported) {
			return
		}
		r.BlindboxOutcome, r.Err = outcome, err
		if err != nil {
			r.Error = err.Error()
			log.Printf("blindbox %s: %v", logfmt.Label(r.UID, r.Nickname), err)
		} else {
			log.Printf("blindbox %s: travel=%s balance=%d affordable=%d cost=%d max=%d → %s",
				logfmt.Label(r.UID, r.Nickname), outcome.TravelState, outcome.Energy,
				outcome.Affordable, outcome.CostPerOpen, outcome.MaxOpen, outcome.Decision)
		}
		out = append(out, r) // 串行，无需加锁
	})
	return out
}

// StreakResult 单账号连登管家结果。
type StreakResult struct {
	UID      string
	Nickname string
	Outcome  gateway.StreakOutcome
	Err      error
}

// StreakAll 对所有适用账号执行连登管家（串行 800ms）。
//
// 由签到排程末尾调用（RunCheckinNow → 本方法），也可面板手动触发。
func (p *Pool) StreakAll(ctx context.Context) []StreakResult {
	var out []StreakResult
	p.forEachCapable(ctx, capOptions{
		IncludePaused: true,
		Concurrency:   1,
		Delay:         capDelay(ctx),
		StopOnCancel:  true,
	}, func(acc gateway.Account) {
		r := StreakResult{UID: acc.UID(), Nickname: acc.Nickname()}
		s, ok := acc.(gateway.StreakRunner)
		if !ok {
			return
		}
		outcome, err := s.RunStreak(ctx)
		if errors.Is(err, gateway.ErrUnsupported) {
			return
		}
		r.Outcome, r.Err = outcome, err
		if err != nil {
			log.Printf("streak-bonus %s: %v", logfmt.Label(r.UID, r.Nickname), err)
		}
		out = append(out, r)
	})
	return out
}

// BlackcatResult 单账号夜猫子结果。
type BlackcatResult struct {
	UID      string
	Nickname string
	Outcome  gateway.BlackcatOutcome
	Err      error
}

// blackcatWindowCheck 夜猫子窗口判定（可注入，测试用）。
//
// 判定放在池层而不是账号实现：这是**调度策略**（几点算夜间），与「账号有什么
// 能力」无关；且需要一个可替换的时钟供测试在白天也能验证窗口逻辑。
// 任何新的调用方接入 Pool.BlackcatAll 都会自动受这个守卫保护。
var blackcatWindowCheck = defaultBlackcatWindow

// defaultBlackcatWindow 23:00–08:00 计数窗口。
func defaultBlackcatWindow(now time.Time) bool {
	h := now.Hour()
	return h >= 23 || h < 8
}

// SetBlackcatWindowCheck 替换窗口判定（测试用；传 nil 恢复默认）。
func SetBlackcatWindowCheck(fn func(time.Time) bool) {
	if fn == nil {
		blackcatWindowCheck = defaultBlackcatWindow
		return
	}
	blackcatWindowCheck = fn
}

// BlackcatAll 对所有适用账号执行夜猫子任务（串行 800ms）。
//
// **不包含 paused 账号**：夜猫子是全任务体系里唯一「整任务都是真实模型对话」的
// （逐条 ChatStream 发短对话），与「让位防风控」正面冲突。旅行/连登/补偿都是
// 纯 RPC 或领奖，照常跑。
//
// 窗口外直接返回空：夜猫子只在该时段计分，白天执行纯属白烧对话额度。
func (p *Pool) BlackcatAll(ctx context.Context) []BlackcatResult {
	if !blackcatWindowCheck(time.Now()) {
		return nil
	}
	var out []BlackcatResult
	p.forEachCapable(ctx, capOptions{
		IncludePaused: false,
		Concurrency:   1,
		Delay:         capDelay(ctx),
		StopOnCancel:  true,
	}, func(acc gateway.Account) {
		r := BlackcatResult{UID: acc.UID(), Nickname: acc.Nickname()}
		b, ok := acc.(gateway.BlackcatRunner)
		if !ok {
			return
		}
		outcome, err := b.RunBlackcat(ctx)
		if errors.Is(err, gateway.ErrUnsupported) {
			return
		}
		r.Outcome, r.Err = outcome, err
		if err != nil {
			log.Printf("blackcat %s: %v", logfmt.Label(r.UID, r.Nickname), err)
		} else if outcome.Completed > 0 {
			log.Printf("blackcat %s: 完成 %d 次夜间对话", logfmt.Label(r.UID, r.Nickname), outcome.Completed)
		}
		out = append(out, r)
	})
	return out
}

// CompensationStatusView 补偿状态的**展示形状**（前端字段契约）。
//
// 与 gateway.CompensationOutcome 同信息，但 JSON 标签对齐前端已写死的
// active/claimed/credit_num/end_time。单独定义是因为 pool 不能 import
// upstream（分层：pool 只认 gateway）。
type CompensationStatusView struct {
	Active    bool   `json:"active"`
	Claimed   bool   `json:"claimed"`
	CreditNum int64  `json:"credit_num"`
	EndTime   string `json:"end_time"`
}

// CompensationResult 单账号补偿结果（面板直接渲染，字段名是前端契约）。
//
// Status/Skip/SkipMsg 是**展示层**字段而不是执行结果：Status 供前端按
// active/claimed/credit_num 渲染，Skip 非空表示本次未发起领取及原因
// （面板据此显示「无活动 / 已领 / 额度 0」）。改造前 scheduler 就是这么给的，
// 前端已按这套字段写死，改了会静默渲染成空白。
type CompensationResult struct {
	UID      string                 `json:"uid"`
	Nickname string                 `json:"nickname,omitempty"`
	Status   CompensationStatusView `json:"status"`
	Claimed  bool                   `json:"claimed"`
	Credit   int64                  `json:"credit,omitempty"`
	Skip     string                 `json:"skip,omitempty"`
	SkipMsg  string                 `json:"skip_msg,omitempty"`
	Error    string                 `json:"error,omitempty"`

	Outcome gateway.CompensationOutcome `json:"-"`
	Err     error                       `json:"-"`
	// Unsupported 该账号不参与（企业版 / global / 无凭证）。
	Unsupported bool `json:"-"`
}

// fill 把归一结果铺到展示字段上（所有填充点共用，避免各写一遍映射）。
func (r *CompensationResult) fill(o gateway.CompensationOutcome, err error) {
	r.Outcome = o
	r.Err = err
	r.Status = CompensationStatusView{
		Active:    o.Active,
		Claimed:   o.Claimed,
		CreditNum: o.CreditNum,
		EndTime:   o.EndTime,
	}
	r.Claimed = o.ClaimedNow
	r.Credit = o.Credit
	r.Skip = o.Skip
	if o.Message != "" && o.Skip != "" {
		r.SkipMsg = o.Message
	}
	if err != nil {
		r.Error = err.Error()
	}
}

// CompensationStatusAll 查全部适用账号的补偿状态（只读，不领取）。
//
// **包含已禁用账号**：它们不进上游查询，但必须以 skip 行出现在结果里——
// 面板靠这一行告诉用户「这个号为什么没动」。把它们过滤掉会让账号从表格里
// 凭空消失，用户以为账号丢了（旧 compTargets 正是这么做的）。
//
// 并发 3（与既有 package 遍历口径一致）。**输出顺序与池顺序一致**：并发写入
// 会让「goroutine 完成顺序」决定行序，面板逐行对齐会随机漂移，所以按账号下标
// 预分配切片、各自只写自己那一格。
func (p *Pool) CompensationStatusAll(ctx context.Context) []CompensationResult {
	// members 而非 accounts：需要 Status.Disabled 来判断该不该发起上游查询。
	members := p.Members(MemberFilter{IncludeDisabled: true, IncludePaused: true})
	out := make([]CompensationResult, len(members))
	sem := make(chan struct{}, 3)
	var wg sync.WaitGroup
	for i, m := range members {
		acc := m.Acc
		out[i] = CompensationResult{UID: acc.UID(), Nickname: acc.Nickname()}
		// 禁用号：不查上游，但保留 skip 行。
		if m.Status.Disabled {
			out[i].Skip = CompSkipDisabled
			out[i].SkipMsg = "账号已禁用"
			continue
		}
		c, ok := acc.(gateway.Compensator)
		if !ok {
			continue
		}
		wg.Add(1)
		go func(i int, c gateway.Compensator) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			outcome, err := c.CompensationStatus(ctx)
			switch {
			case errors.Is(err, gateway.ErrUnsupported):
				// 账号判定不适用（企业版 / global / 无凭证）：**保留这一行**并标出
				// 原因。面板靠它显示「这个号为什么不参与」——把它丢掉会让企业号
				// 从表格里凭空消失，用户以为账号丢了。
				out[i].Unsupported = true
				out[i].Skip = compSkipReasonFor(acc)
				out[i].SkipMsg = unsupportedMessage(out[i].Skip)
			case err != nil:
				out[i].fill(gateway.CompensationOutcome{}, err)
			default:
				out[i].fill(outcome, nil)
			}
		}(i, c)
	}
	wg.Wait()
	return out
}

// CompensationClaimAll 逐号「查状态 → 待领才领」（串行）。
//
// 刻意串行：同一活动每号仅一次，重复领会拿到业务错误；串行让「前一个号失败」
// 不影响后一个号的判定，且账号量通常是个位数。
func (p *Pool) CompensationClaimAll(ctx context.Context) []CompensationResult {
	var out []CompensationResult
	// 与 StatusAll 同口径：禁用号保留 skip 行（面板要能显示「为什么没领」）。
	for _, m := range p.Members(MemberFilter{IncludeDisabled: true, IncludePaused: true}) {
		acc := m.Acc
		r := CompensationResult{UID: acc.UID(), Nickname: acc.Nickname()}
		if m.Status.Disabled {
			r.Skip, r.SkipMsg = CompSkipDisabled, "账号已禁用"
			out = append(out, r)
			continue
		}
		c, ok := acc.(gateway.Compensator)
		if !ok {
			continue
		}
		if ctx.Err() != nil {
			break
		}
		outcome, err := c.CompensationClaim(ctx)
		switch {
		case errors.Is(err, gateway.ErrUnsupported):
			r.Unsupported = true
		case err != nil:
			r.Err = err
			log.Printf("compensation %s: %v", logfmt.Label(r.UID, r.Nickname), err)
		default:
			r.fill(outcome, nil)
			if outcome.ClaimedNow {
				log.Printf("compensation %s: 🎊 补偿领取 +%d（截止 %s）",
					logfmt.Label(r.UID, r.Nickname), outcome.Credit, outcome.EndTime)
			}
		}
		out = append(out, r)
	}
	return out
}

// ActivityResult 单账号活跃上报结果。
type ActivityResult struct {
	UID      string
	Nickname string
	Outcome  gateway.ActivityOutcome
	Err      error
	// Unsupported 该账号不参与（企业版）。
	Unsupported bool
}

// ActivityAll 对所有适用账号执行活跃上报（串行 800ms，ctx 可中断）。
//
// 与夜猫子不同，global 账号**也**参与：国际版 /v2/report 实测可用（code=0），
// 跳过会让国际版账号永远点不亮连登。企业版仍跳过——它没有个人成长体系。
func (p *Pool) ActivityAll(ctx context.Context) []ActivityResult {
	var out []ActivityResult
	p.forEachCapable(ctx, capOptions{
		IncludeDisabled: includeDisabledInTasks(ctx),
		IncludePaused:   true,
		Concurrency:     1,
		Delay:           capDelay(ctx),
		StopOnCancel:    true,
	}, func(acc gateway.Account) {
		r := ActivityResult{UID: acc.UID(), Nickname: acc.Nickname()}
		rep, ok := acc.(gateway.ActivityReporter)
		if !ok {
			return
		}
		outcome, err := rep.ReportActivity(ctx)
		switch {
		case errors.Is(err, gateway.ErrUnsupported):
			r.Unsupported = true
		case err != nil:
			r.Err = err
			log.Printf("activity %s: %v", logfmt.Label(r.UID, r.Nickname), err)
		default:
			r.Outcome = outcome
		}
		out = append(out, r)
	})
	return out
}

// 补偿任务的跳过原因（面板据此渲染，勿随意改字面量）。
const (
	CompSkipDisabled   = "disabled"    // 账号已禁用，等人工 revive
	CompSkipNoAuth     = "no_auth"     // 无凭证
	CompSkipGlobal     = "global"      // D4 门控：global 无 CN 任务体系
	CompSkipEnterprise = "enterprise"  // 企业版门控：claim-compensation 返回 10001
	CompSkipInactive   = "inactive"    // 当前无生效中的补偿活动
	CompSkipClaimed    = "claimed"     // 本次活动已领过
	CompSkipZeroCredit = "zero_credit" // 活动在，但额度为 0
)

// CompensationClaimOne 单账号「查状态 → 待领才领」（面板单号按钮）。
//
// 与 CompensationClaimAll 共用同一段账号实现，避免两条路径的分支/日志漂移。
//
// 单号入口自己补 disabled 门控：批量路径由 Members 的 IncludeDisabled 过滤，
// 单号入口拿的是「按 uid 直取」，没有那层过滤——不补就会出现
// 「批量按钮跳过禁用号、手动按钮却能领」的割裂。
func (p *Pool) CompensationClaimOne(ctx context.Context, uid string) (CompensationResult, error) {
	acc := p.Account(uid)
	if acc == nil {
		return CompensationResult{UID: uid}, ErrNoSuchAccount
	}
	res := CompensationResult{UID: acc.UID(), Nickname: acc.Nickname()}
	if st, ok := p.Status(uid); ok && st.Disabled {
		res.Skip, res.SkipMsg = CompSkipDisabled, "账号已禁用"
		return res, nil
	}
	c, ok := acc.(gateway.Compensator)
	if !ok {
		return res, gateway.ErrUnsupported
	}
	outcome, err := c.CompensationClaim(ctx)
	switch {
	case errors.Is(err, gateway.ErrUnsupported):
		// 账号自己判定不适用（企业版 / global / 无凭证）：给出与批量路径一致的
		// 跳过原因，面板才能显示「为什么这个号没动」。
		res.Unsupported = true
		res.Skip, res.SkipMsg = compSkipReasonFor(acc), unsupportedMessage(res.Skip)
		return res, nil
	case err != nil:
		res.fill(gateway.CompensationOutcome{}, err)
		log.Printf("compensation %s: %v", logfmt.Label(res.UID, res.Nickname), err)
	default:
		res.fill(outcome, nil)
		if outcome.ClaimedNow {
			log.Printf("compensation %s: 🎊 补偿领取 +%d（截止 %s）",
				logfmt.Label(res.UID, res.Nickname), outcome.Credit, outcome.EndTime)
		}
	}
	return res, nil
}

// compSkipReasonFor 把「账号不适用」细分成面板可读的原因。
//
// 池不 import auth，只能通过 gateway.Account 暴露的 Enterprise/Realm 判定；
// 「无凭证」这一态由账号实现返回 ErrUnsupported 时无法区分，统一归到 enterprise
// 之外的兜底文案（面板显示「该账号不参与补偿」）。
func compSkipReasonFor(acc gateway.Account) string {
	if acc.Enterprise() {
		return CompSkipEnterprise
	}
	if acc.Realm() == "global" {
		return CompSkipGlobal
	}
	return CompSkipNoAuth
}

// unsupportedMessage 跳过原因的中文说明（与批量路径文案保持一致）。
func unsupportedMessage(skip string) string {
	switch skip {
	case CompSkipGlobal:
		return "国际版无 CN 补偿体系"
	case CompSkipEnterprise:
		return "企业版无补偿体系"
	default:
		return "无凭证"
	}
}

// includeDisabledInTasks 从 ctx 读取「禁用账号是否参与保号任务」。
//
// 用 context value 而不是 Pool 字段：这是**调用方策略**（调度器配置），
// 不是池的固有属性；放进池会让「改配置要重启池」这类耦合出现。
// 调度器在调用 ActivityAll 前用 WithIncludeDisabledInTasks 注入。
func includeDisabledInTasks(ctx context.Context) bool {
	v, _ := ctx.Value(ctxKeyIncludeDisabled).(bool)
	return v
}

type ctxKey string

// ctxKeyIncludeDisabled 禁用账号参与保号任务的 ctx 键。
const ctxKeyIncludeDisabled ctxKey = "pool.includeDisabledInTasks"

// WithIncludeDisabledInTasks 在 ctx 上标注「保号任务是否包含禁用账号」。
//
// 仅 ActivityAll 读取（其它特色能力对禁用号一律跳过——凭证可能已失效）。
func WithIncludeDisabledInTasks(ctx context.Context, v bool) context.Context {
	return context.WithValue(ctx, ctxKeyIncludeDisabled, v)
}

// 特色能力的默认账号间延迟（与改造前的 travelAccountDelay 同值同口径）。
//
// 注意：这里只是**默认值**。真正的延迟由调用方经 ctx 传入（见 WithCapabilityDelay），
// 因为 scheduler 的 runBatch 会**并发**派发多类任务（旅行/活跃/夜猫子同时跑），
// 用一个全局变量会让它们互相覆盖——旅行设 800ms、活跃设 0 时，谁后写谁生效。
var defaultCapabilityDelay atomic.Int64

func init() {
	defaultCapabilityDelay.Store(int64(800 * time.Millisecond))
}

// WithCapabilityDelay 为本次批量调度指定账号间延迟（>=0；0 = 不等待）。
//
// 用 ctx 而不是全局变量：任务之间互不干扰，也不需要调用方在巡检前后成对
// set/reset（那种写法在并发任务下必然串味）。
func WithCapabilityDelay(ctx context.Context, d time.Duration) context.Context {
	if d < 0 {
		d = 0
	}
	return context.WithValue(ctx, ctxKeyCapDelay, d)
}

const ctxKeyCapDelay ctxKey = "pool.capabilityDelay"

// capDelay 本次批量调度生效的延迟：ctx 指定优先，否则用默认值。
func capDelay(ctx context.Context) time.Duration {
	if v, ok := ctx.Value(ctxKeyCapDelay).(time.Duration); ok {
		return v
	}
	return time.Duration(defaultCapabilityDelay.Load())
}

// SetCapabilityDelay 覆盖**默认**账号间延迟（<=0 表示不等待）。
//
// 只影响未通过 WithCapabilityDelay 指定延迟的调用方。保留它是为了兼容既有
// 测试（它们用 `SetCapabilityDelay(0)` 关掉真实等待）。
func SetCapabilityDelay(d time.Duration) {
	if d < 0 {
		d = 0
	}
	defaultCapabilityDelay.Store(int64(d))
}

// CapabilityDelay 当前生效的**默认**延迟（测试断言用）。
func CapabilityDelay() time.Duration {
	return time.Duration(defaultCapabilityDelay.Load())
}
