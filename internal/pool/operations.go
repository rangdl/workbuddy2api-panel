// operations.go 账号池的批量能力调度。
//
// 设计原则：**池遍历池里的账号对象，逐个调它自己的方法**。
//
// 池不认识「CodeBuddy」「Trae」这些上游概念，也不知道某家协议怎么发请求——
// 那些都关在各自的 gateway.Account 实现里。新增一个上游时，只要它的账号
// 对象实现了 Credits / Checkin / Keepalive，就自动进入这里的全部流程，
// 池与调用方（调度器、面板）一行都不用改。
//
// 不支持某能力的账号返回 gateway.ErrUnsupported，池直接跳过——这是「这个
// 上游没这个概念」，不是失败：不进冷却、不记错误。
package pool

import (
	"context"
	"errors"
	"log"
	"sync"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/gateway"
	"github.com/linguo2625469/workbuddy2api-panel/internal/logfmt"
)

// opConcurrency 批量操作的并发上限（上游风控友好）。
const opConcurrency = 4

// ErrNoSuchAccount 池里没有这个账号。
var ErrNoSuchAccount = errors.New("pool: account not found")

// opOptions 批量操作的遍历过滤条件。
//
// 用一个小结构而不是一长串 bool：调用方（调度器）只需要表达「谁该参与」，
// 不该把池的内部字段名铺在参数表上。
type opOptions struct {
	// IncludeDisabled 已禁用的账号是否参与（禁用只关选号，不停保号任务）。
	IncludeDisabled bool
	// OnlyUIDs 非空时只处理这些账号（nil = 全部）。
	OnlyUIDs map[string]bool
	// SkipGlobal 跳过 global 域账号（CN 的签到/任务端点对它们无效，
	// 上游会拒或静默失败，发起调用只是白白消耗风控额度）。
	SkipGlobal bool
	// Backend 非空时只处理该上游的账号（面板的按上游批量操作用）。
	Backend string
}

func (o opOptions) admits(e *entry) bool {
	if e.acc == nil {
		return false
	}
	if e.disabled && !o.IncludeDisabled {
		return false
	}
	if o.SkipGlobal && e.acc.Realm() == "global" {
		return false
	}
	if o.Backend != "" && e.acc.Backend() != o.Backend {
		return false
	}
	if o.OnlyUIDs != nil && !o.OnlyUIDs[e.acc.UID()] {
		return false
	}
	return true
}

// accountsLocked 取出满足条件的账号对象快照。调用方必须持有 p.mu。
//
// 返回的是副本切片：后续的上游调用在锁外进行，不能持有池锁等网络。
func (p *Pool) accountsLocked(opts opOptions) []gateway.Account {
	out := make([]gateway.Account, 0, len(p.byUID))
	for _, e := range p.byUID {
		if opts.admits(e) {
			out = append(out, e.acc)
		}
	}
	return out
}

// RefreshCredits 刷新池内所有账号的余额（并发受限）。
//
// 这是「积分」的唯一刷新入口：调度器的后台刷新、面板的手动刷新都调它。
// 每个账号的余额怎么查由账号对象自己决定，池只负责遍历与写回。
func (p *Pool) RefreshCredits(ctx context.Context) {
	p.refreshCredits(ctx, opOptions{})
}

// RefreshCreditsIncludingDisabled 同 RefreshCredits，但包含已禁用账号
// （轮换用法下据此判断下一个该启用谁）。
func (p *Pool) RefreshCreditsIncludingDisabled(ctx context.Context) {
	p.refreshCredits(ctx, opOptions{IncludeDisabled: true})
}

// RefreshCreditsForTasks 签到之后的余额刷新：跳过 global 域。
//
// global 账号没有 CN 的签到/任务体系，查余额只会白打上游。
// 后台周期刷新（RefreshCredits）不跳过——它的作用是让积分观测量保持新鲜，
// 全球版账号的积分同样要展示。
func (p *Pool) RefreshCreditsForTasks(ctx context.Context, includeDisabled bool) {
	p.refreshCredits(ctx, opOptions{IncludeDisabled: includeDisabled, SkipGlobal: true})
}

func (p *Pool) refreshCredits(ctx context.Context, opts opOptions) {
	p.mu.RLock()
	accs := p.accountsLocked(opts)
	p.mu.RUnlock()

	var wg sync.WaitGroup
	sem := make(chan struct{}, opConcurrency)
	for _, acc := range accs {
		wg.Add(1)
		go func(a gateway.Account) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				return
			}
			credits, err := a.Credits(ctx)
			if err != nil {
				if errors.Is(err, gateway.ErrUnsupported) || errors.Is(err, gateway.ErrCreditsUnsupported) {
					return // 该上游没有余额概念：跳过，不算失败
				}
				log.Printf("balance %s: %v", logfmt.Label(a.UID(), a.Nickname()), err)
				return
			}
			if credits == nil {
				return
			}
			snap := CreditsSnapshot{
				Remain:            int64(credits.Available),
				Total:             int64(credits.Limit),
				Expiring:          int64(credits.Expiring),
				EarliestAt:        credits.Earliest,
				EarliestRemaining: earliestBucketRemain(credits.Buckets),
				ExpiryBuckets:     gatewayBuckets(credits.Buckets),
			}
			// 快过期子集由池按当前窗口统一计算：这是调度策略，不是上游能力，
			// 因此不在各 backend 的 Credits 里各算一遍（口径会漂移）。
			if w := p.ExpiringSoonWindow(); w > 0 {
				snap.Expiring = expiringWithin(snap.ExpiryBuckets, w, time.Now())
			}
			// 写回统一走池：解冻判定与快照落地都在这里，账号对象不管状态。
			p.ReenableIfCredits(a.UID(), snap.Remain, snap.Total)
			p.SetCreditsSnapshot(a.UID(), snap)
		}(acc)
	}
	wg.Wait()
}

// CheckinAll 对池内所有账号执行一次签到（并发受限）。
//
// global 域账号跳过：CN 的签到端点对它们无效，发起调用只是白耗风控额度。
// 企业版等「这类账号不参与签到」的情况由账号对象返回 Skipped，池正常记账；
// 只有真正的失败才记日志。
func (p *Pool) CheckinAll(ctx context.Context, includeDisabled bool) {
	p.checkinAll(ctx, opOptions{IncludeDisabled: includeDisabled, SkipGlobal: true})
}

// CheckinBackend 只对指定上游的账号执行签到（面板的「Trae 全部签到」等入口）。
//
// SkipGlobal 与 CheckinAll 同口径：CN 的签到端点对 global 域账号无效，发起调用
// 只是白耗风控额度。此前只有 CheckinAll 带这个旗标，而调度器改为按上游分批后
// 走的是本方法——漏带会让每次自动签到都去撞一遍国际版账号（回归点，见
// TestCheckinStillSkipsGlobal）。
func (p *Pool) CheckinBackend(ctx context.Context, backend string, includeDisabled bool) {
	p.checkinAll(ctx, opOptions{IncludeDisabled: includeDisabled, SkipGlobal: true, Backend: backend})
}

func (p *Pool) checkinAll(ctx context.Context, opts opOptions) {
	p.mu.RLock()
	accs := p.accountsLocked(opts)
	p.mu.RUnlock()

	var wg sync.WaitGroup
	sem := make(chan struct{}, opConcurrency)
	for _, acc := range accs {
		wg.Add(1)
		go func(a gateway.Account) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				return
			}
			res, err := a.Checkin(ctx)
			if err != nil {
				if errors.Is(err, gateway.ErrUnsupported) {
					return
				}
				log.Printf("checkin %s: %v", logfmt.Label(a.UID(), a.Nickname()), err)
				return
			}
			if res == nil || res.Skipped {
				return
			}
			// 只有「本账号确实签成」才标记今日已签。三种情形要排除：
			//   - DeviceAlready：设备维度限额（多账号共用设备号时只有第一个能签），
			//     本账号未必签过；
			//   - Unverified：claim 说成功但回查未翻转，可能是假成功。
			// 把它们标成已签会让按钮谎报「已签」，且次日也不会重试。
			if !res.Unverified && !(res.Already && res.DeviceAlready) {
				p.NoteCheckinDone(a.UID())
			}
			switch {
			case res.DeviceAlready:
				log.Printf("checkin %s: 设备今日已签，本账号未签到", logfmt.Label(a.UID(), a.Nickname()))
			case res.Unverified:
				// 回查未确认：不标已签，交下轮重试。
				log.Printf("checkin %s: %s", logfmt.Label(a.UID(), a.Nickname()), res.Message)
			case res.Already:
				log.Printf("checkin %s: 今日已签到（幂等）", logfmt.Label(a.UID(), a.Nickname()))
			default:
				log.Printf("checkin %s: 签到成功 +%d", logfmt.Label(a.UID(), a.Nickname()), res.Reward)
			}
		}(acc)
	}
	wg.Wait()
}

// KeepaliveAll 对池内所有账号刷新凭证（并发受限）。
//
// session 死亡的账号交给调用方处理：池只报告错误，不做禁用决策——
// 那是策略，不该埋在遍历循环里。
func (p *Pool) KeepaliveAll(ctx context.Context, includeDisabled bool) map[string]error {
	p.mu.RLock()
	accs := p.accountsLocked(opOptions{IncludeDisabled: includeDisabled})
	p.mu.RUnlock()

	var (
		mu   sync.Mutex
		wg   sync.WaitGroup
		errs = make(map[string]error, len(accs))
	)
	sem := make(chan struct{}, opConcurrency)
	for _, acc := range accs {
		wg.Add(1)
		go func(a gateway.Account) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				return
			}
			if err := a.Keepalive(ctx); err != nil && !errors.Is(err, gateway.ErrUnsupported) {
				mu.Lock()
				errs[a.UID()] = err
				mu.Unlock()
			}
		}(acc)
	}
	wg.Wait()
	return errs
}

// Account 按 uid 取账号对象（不看健康状态：运维接口要能操作冷却/禁用的号）。
// 未找到返回 nil。
func (p *Pool) Account(uid string) gateway.Account {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if e, ok := p.lookupLocked(uid); ok {
		return e.acc
	}
	return nil
}

// CheckinOne 对单个账号执行签到（面板「签到」按钮）。
//
// 与 CheckinAll 共用同一套记账逻辑，只是范围限定为一个账号——包括
// DeviceAlready / Unverified 不标「今日已签」的语义（见 checkinAll 的注释）。
func (p *Pool) CheckinOne(ctx context.Context, uid string) (*gateway.CheckinResult, error) {
	acc := p.Account(uid)
	if acc == nil {
		return nil, ErrNoSuchAccount
	}
	res, err := acc.Checkin(ctx)
	if err != nil {
		return nil, err
	}
	if res != nil && !res.Skipped && !res.Unverified && !(res.Already && res.DeviceAlready) {
		p.NoteCheckinDone(uid)
	}
	return res, nil
}

// RefreshCreditsOne 刷新单个账号余额（面板「余额/积分」按钮）。
func (p *Pool) RefreshCreditsOne(ctx context.Context, uid string) (*gateway.Credits, error) {
	acc := p.Account(uid)
	if acc == nil {
		return nil, ErrNoSuchAccount
	}
	credits, err := acc.Credits(ctx)
	if err != nil {
		return nil, err
	}
	if credits == nil {
		return nil, nil
	}
	snap := CreditsSnapshot{
		Remain:            int64(credits.Available),
		Total:             int64(credits.Limit),
		Expiring:          int64(credits.Expiring),
		EarliestAt:        credits.Earliest,
		EarliestRemaining: earliestBucketRemain(credits.Buckets),
		ExpiryBuckets:     gatewayBuckets(credits.Buckets),
	}
	if w := p.ExpiringSoonWindow(); w > 0 {
		snap.Expiring = expiringWithin(snap.ExpiryBuckets, w, time.Now())
	}
	p.ReenableIfCredits(uid, snap.Remain, snap.Total)
	p.SetCreditsSnapshot(uid, snap)
	return credits, nil
}

// SetExpiringSoonWindow 设置「快过期」窗口（<=0 表示不标记）。
//
// 由调度器在配置加载/热重载时注入，池在 RefreshCredits 里据此计算 Expiring。
func (p *Pool) SetExpiringSoonWindow(d time.Duration) {
	p.expiringSoonWindow.Store(int64(d))
}

// ExpiringSoonWindow 当前快过期窗口。
func (p *Pool) ExpiringSoonWindow() time.Duration {
	return time.Duration(p.expiringSoonWindow.Load())
}

// expiringWithin 汇总 now+window 内到期的批次额度。
func expiringWithin(buckets []ExpiryBucket, window time.Duration, now time.Time) int64 {
	if window <= 0 {
		return 0
	}
	deadline := now.Add(window)
	var sum int64
	for _, b := range buckets {
		if b.ExpireAt.IsZero() || b.ExpireAt.After(deadline) {
			continue
		}
		sum += b.Remain
	}
	return sum
}

// gatewayBuckets 把 gateway 的到期批次转换成池结构（两包字段同构，
// 手工转换是为了不让 pool 反向依赖上游实现）。
func gatewayBuckets(in []gateway.ExpiryBucket) []ExpiryBucket {
	if len(in) == 0 {
		return nil
	}
	out := make([]ExpiryBucket, len(in))
	for i, b := range in {
		out[i] = ExpiryBucket{Date: b.Date, ExpireAt: b.ExpireAt, Remain: b.Remain}
	}
	return out
}

// earliestBucketRemain 最早一批的剩余量（无批次时 0）。
func earliestBucketRemain(buckets []gateway.ExpiryBucket) int64 {
	if len(buckets) == 0 {
		return 0
	}
	return buckets[0].Remain
}
