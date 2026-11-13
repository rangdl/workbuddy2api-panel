// trae_ops.go 面板的 Trae 运维接口：签到（全部 / 单账号）、积分查询、签到结果查询。
//
// 与 trae_config.go 的分工：那边管配置与凭证，这边管对上游的主动操作。
package panel

import (
	"context"
	"net/http"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/trae"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
	"log"
	"strings"
	"sync"
)

// traeOpTimeout 单次运维操作的超时（签到含 status 预检 + claim，两个上游往返）。
const traeOpTimeout = 60 * time.Second

// traeCheckinAllTimeout 全量签到的总超时（账号间限速 800ms，账号多时耗时线性增长）。
const traeCheckinAllTimeout = 10 * time.Minute

// traeCheckinAll 触发全部账号签到（同步执行，返回逐账号结果）。
func (p *Panel) traeCheckinAll(w http.ResponseWriter, r *http.Request) {
	if p.cfg.TraeScheduler == nil {
		writeErr(w, http.StatusNotImplemented, "trae scheduler not available")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), traeCheckinAllTimeout)
	defer cancel()
	outcomes := p.cfg.TraeScheduler.RunOnce(ctx)
	p.invalidateCheckinCache() // 签到后状态已变，下次拉配置必须重新查
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "results": outcomes})
}

// traeAccountCheckin 单账号签到。
func (p *Panel) traeAccountCheckin(w http.ResponseWriter, r *http.Request) {
	if p.cfg.TraeClient == nil || p.cfg.TraePath == "" {
		writeErr(w, http.StatusNotImplemented, "trae client not available")
		return
	}
	cfg, err := trae.Load(p.cfg.TraePath, p.cfg.AuthDir)
	if err != nil || cfg == nil {
		writeErr(w, http.StatusBadRequest, "trae.json 不存在或无法读取")
		return
	}
	uid := r.PathValue("uid")
	acc := cfg.AccountByUID(uid)
	if acc == nil {
		writeErr(w, http.StatusNotFound, "账号不存在: "+uid)
		return
	}
	// 与调度器走同一条链路：先按需续期，再签到（含 9004/9095 识别与回查确认）。
	ctx, cancel := context.WithTimeout(r.Context(), traeOpTimeout)
	defer cancel()
	if cfg.RefreshBeforeCheckinEnabled() {
		if _, rerr := p.cfg.TraeClient.EnsureTokenFresh(ctx, cfg, acc, 2*time.Hour); rerr != nil {
			writeJSON(w, http.StatusOK, map[string]any{
				"ok": false, "uid": uid, "error_type": "session_dead",
				"message": "签到前续期失败: " + rerr.Error(),
			})
			return
		}
	}
	res, err := p.cfg.TraeClient.Checkin(ctx, cfg, acc)
	if err != nil {
		// 错误分类后一并回传，便于面板给出可操作提示。
		if apiErr, ok := err.(*trae.APIError); ok {
			cls := apiErr.Classification()
			if cls.Already {
				// 9004 / 9095 不是失败：按「今日已签」回传。
				p.invalidateCheckinCache()
				writeJSON(w, http.StatusOK, map[string]any{
					"ok": true, "uid": uid, "already": true,
					"device_already": cls.Code == "9095" || cls.Code == "9004",
					"message":        cls.Message,
				})
				return
			}
			writeJSON(w, http.StatusOK, map[string]any{
				"ok": false, "uid": uid, "error_type": string(cls.Kind),
				"message": cls.Message, "disable": cls.Disable,
			})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "uid": uid, "message": err.Error()})
		return
	}
	if p.cfg.TraeCooldowns != nil && !res.Already {
		p.cfg.TraeCooldowns.Clear(uid) // 签到成功即清冷却
	}
	p.invalidateCheckinCache()
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "uid": uid, "already": res.Already,
		"device_already": res.DeviceAlready, "unverified": res.Unverified,
		"reward": res.Reward, "message": res.Message,
	})
}

// traeAccountCredits 查询单账号积分包明细与汇总。
func (p *Panel) traeAccountCredits(w http.ResponseWriter, r *http.Request) {
	if p.cfg.TraeClient == nil || p.cfg.TraePath == "" {
		writeErr(w, http.StatusNotImplemented, "trae client not available")
		return
	}
	cfg, err := trae.Load(p.cfg.TraePath, p.cfg.AuthDir)
	if err != nil || cfg == nil {
		writeErr(w, http.StatusBadRequest, "trae.json 不存在或无法读取")
		return
	}
	uid := r.PathValue("uid")
	acc := cfg.AccountByUID(uid)
	if acc == nil {
		writeErr(w, http.StatusNotFound, "账号不存在: "+uid)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), traeOpTimeout)
	defer cancel()
	sum, err := p.cfg.TraeClient.QueryCredits(ctx, cfg, acc)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "uid": uid, "message": err.Error()})
		return
	}
	// 回写两处：
	//   1) TraeStats —— 面板「Trae 签到」区块的积分列；
	//   2) pool —— 账号池页面的积分列与选号权重都读它。
	// 此前只写了前者，于是账号池里 Trae 账号的积分恒为 0。
	if sum != nil {
		if p.cfg.TraeStats != nil {
			// 先取旧值算余额差（校准用），再写入新值。
			if prev := p.cfg.TraeStats.Get(uid); prev.HasCredits && prev.CreditsAt != nil {
				if delta := prev.Credits - sum.General; delta > 0 {
					log.Printf("[trae] 余额差 acct=%s 上次=%.2f 当前=%.2f 消耗=%.2f",
						uid, prev.Credits, sum.General, delta)
				}
			}
			p.cfg.TraeStats.NoteCredits(uid, sum.General, sum.Work, sum.Total, sum.Limit)
		}
		// 与 CodeBuddy 同口径：credits 存"可用于对话的额度"。
		// Trae 必须用 General（product_id != 209），不能用 Total——
		// Total 含 Work 积分，而对话只消耗 IDE 额度，会让快耗尽的号看起来满额。
		if p.cfg.Pool != nil {
			p.cfg.Pool.SetCredits(uid, int64(sum.General), int64(sum.Limit))
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "uid": uid, "credits": sum})
}

// traeCheckinStatus 返回最近一次（自动或手动）全量签到的结果。
func (p *Panel) traeCheckinStatus(w http.ResponseWriter, r *http.Request) {
	if p.cfg.TraeScheduler == nil {
		writeErr(w, http.StatusNotImplemented, "trae scheduler not available")
		return
	}
	outcomes, at := p.cfg.TraeScheduler.LastOutcomes()
	var lastRun string
	if !at.IsZero() {
		lastRun = at.Format(time.RFC3339)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "last_run": lastRun, "results": outcomes,
	})
}

// traeSyncProfiles 批量从上游拉取昵称并落盘。
//
// 为什么需要：Trae 的 JWT 里没有用户名字段，昵称只能来自 GetUserInfo。
// 登录时已经自动拉过一次，但这个接口用于两类场景：
//   - 手工导入/历史遗留的账号（当初没有昵称）；
//   - 用户在上游改了昵称（与 CodeBuddy 的「昵称同步」同语义）。
//
// 与 CodeBuddy 的实现对齐：并发受限、失败不影响其它账号、只在变化时写盘。
func (p *Panel) traeSyncProfiles(w http.ResponseWriter, r *http.Request) {
	if p.cfg.TraeClient == nil || p.cfg.TraePath == "" {
		writeErr(w, http.StatusNotImplemented, "trae client not available")
		return
	}
	cfg, err := trae.Load(p.cfg.TraePath, p.cfg.AuthDir)
	if err != nil || cfg == nil {
		writeErr(w, http.StatusBadRequest, "trae.json 不存在或无法读取")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), traeOpTimeout)
	defer cancel()

	var (
		mu      sync.Mutex
		wg      sync.WaitGroup
		updated int
		failed  int
	)
	sem := make(chan struct{}, 4)
	for i := range cfg.Accounts {
		acc := &cfg.Accounts[i]
		if acc.Disabled || acc.AccessToken == "" {
			continue
		}
		wg.Add(1)
		go func(a *trae.Account) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			info, ierr := p.cfg.TraeClient.GetUserInfo(ctx, cfg, a)
			if ierr != nil {
				mu.Lock()
				failed++
				mu.Unlock()
				return
			}
			name := strings.TrimSpace(info.Nickname)
			if name == "" || name == a.Name {
				return // 取不到或未变化：不写盘
			}
			a.Name = name
			if serr := trae.SaveAccount(cfg.AuthDir, a); serr != nil {
				log.Printf("WARN: [trae] 昵称落盘失败 %s: %v", a.UID, serr)
				mu.Lock()
				failed++
				mu.Unlock()
				return
			}
			mu.Lock()
			updated++
			mu.Unlock()
		}(acc)
	}
	wg.Wait()
	if updated > 0 || failed > 0 {
		log.Printf("[trae] 昵称同步：更新 %d 个，失败 %d 个（未变化不计数）", updated, failed)
		p.reloadTrae()
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "updated": updated, "failed": failed})
}

// syncTraeCreditsToPool 把 TraeStats 里缓存的积分同步进池。
//
// 为什么需要：池的 credits 字段是选号权重的第一因子，也是账号池页面积分列的
// 数据源。CodeBuddy 由 scheduler 的余额刷新写入，Trae 的积分只能来自
// GetCredits 接口（chat 流里没有扣费字段，只有 notify_usage 的剩余额度）。
//
// 刻意**不发上游请求**：本函数在每次 overview 轮询（5 秒）时被调用，
// 打上游会白白消耗额度。真正刷新由「余额」按钮或对话中的 notify_usage 触发。
func (p *Panel) syncTraeCreditsToPool() {
	if p.cfg.TraeStats == nil || p.cfg.Pool == nil {
		return
	}
	for _, st := range p.cfg.TraeStats.Snapshot() {
		if !st.HasCredits {
			continue // 从未查过：保持池里原值（可能是上次进程的持久化数据）
		}
		p.cfg.Pool.SetCredits(st.UID, int64(st.Credits), int64(st.CreditsLimit))
	}
}

// traeCreditTTL 积分自动刷新的最小间隔。
//
// 目的：让账号池页面首次打开就能看到 Trae 积分，同时避免每 5 秒轮询都打上游。
// 积分变化是分钟级的（对话扣费），5 分钟一次足够，且与 CodeBuddy 的余额
// 后台刷新间隔同量级。
const traeCreditTTL = 5 * time.Minute

// traeCreditCache 逐账号积分刷新时刻（防止 overview 高频轮询打爆上游）。
type traeCreditCache struct {
	mu sync.Mutex
	at map[string]time.Time
}

// refreshTraeCredits 在后台按需刷新 Trae 账号积分（非阻塞）。
//
// 由 overview 调用：过期的账号异步查一次，本次响应仍用缓存值（首次会显示 0，
// 下一个 5 秒轮询就有了）。这样面板加载不会因为等上游而变慢。
func (p *Panel) refreshTraeCredits() {
	if p.cfg.TraeClient == nil || p.cfg.TraePath == "" || p.cfg.Pool == nil {
		return
	}
	cfg, err := trae.Load(p.cfg.TraePath, p.cfg.AuthDir)
	if err != nil || cfg == nil || !cfg.Enabled {
		return
	}
	now := time.Now()
	p.traeCredit.mu.Lock()
	if p.traeCredit.at == nil {
		p.traeCredit.at = make(map[string]time.Time)
	}
	var due []*trae.Account
	for i := range cfg.Accounts {
		a := &cfg.Accounts[i]
		if a.Disabled || a.AccessToken == "" {
			continue
		}
		if now.Sub(p.traeCredit.at[a.UID]) < traeCreditTTL {
			continue
		}
		p.traeCredit.at[a.UID] = now // 先占位，避免并发重复查询
		due = append(due, a)
	}
	p.traeCredit.mu.Unlock()
	if len(due) == 0 {
		return
	}

	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), traeOpTimeout)
		defer cancel()
		var wg sync.WaitGroup
		sem := make(chan struct{}, 3)
		for _, a := range due {
			wg.Add(1)
			go func(acc *trae.Account) {
				defer wg.Done()
				sem <- struct{}{}
				defer func() { <-sem }()
				sum, qerr := p.cfg.TraeClient.QueryCredits(ctx, cfg, acc)
				if qerr != nil || sum == nil {
					// 失败：清掉占位让下次重试（可能 token 过期需人工处理）。
					p.traeCredit.mu.Lock()
					delete(p.traeCredit.at, acc.UID)
					p.traeCredit.mu.Unlock()
					return
				}
				if p.cfg.TraeStats != nil {
					p.cfg.TraeStats.NoteCredits(acc.UID, sum.General, sum.Work, sum.Total, sum.Limit)
				}
				p.cfg.Pool.SetCredits(acc.UID, int64(sum.General), int64(sum.Limit))
			}(a)
		}
		wg.Wait()
	}()
}

// fillTraePackages 把 Trae 的积分包填进「积分构成」的行。
//
// 与 CodeBuddy 的差异：
//   - 包按 product_id 分两类（208/其余 = IDE 通用积分，209 = Work 积分），
//     面板上分开展示比混成一个数更有用；
//   - 包是**周期制**（start_time/end_time），到期即失效，与一次性发放的包不同；
//   - 上游给的是「已用」（usage.credits_amount），剩余 = 额度 - 已用。
//
// 失败只标 error 不影响其它账号——与本接口既有语义一致。
func (p *Panel) fillTraePackages(it *packagesRow, uid string) {
	if p.cfg.TraeClient == nil || p.cfg.TraePath == "" {
		it.Error = "trae client not available"
		return
	}
	cfg, err := trae.Load(p.cfg.TraePath, p.cfg.AuthDir)
	if err != nil || cfg == nil {
		it.Error = "trae.json 不存在或无法读取"
		return
	}
	acc := cfg.AccountByUID(uid)
	if acc == nil {
		it.Error = "account not found in trae.json"
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), traeOpTimeout)
	defer cancel()
	sum, qerr := p.cfg.TraeClient.QueryCredits(ctx, cfg, acc)
	if qerr != nil {
		it.Error = qerr.Error()
		return
	}
	packs := make([]upstream.CreditPackage, 0, len(sum.Packs))
	for _, pk := range sum.Packs {
		name := pk.GroupName
		if name == "" {
			name = "积分包"
		}
		// 标明 IDE / Work 归属：两者不可互换（对话只消耗 IDE 额度）。
		if pk.IsWork() {
			name += "（Work）"
		} else {
			name += "（通用）"
		}
		packs = append(packs, upstream.CreditPackage{
			Name:   name,
			Remain: int64(pk.Remaining()),
			Used:   int64(pk.CreditsUsed),
			Size:   int64(pk.CreditsLimit),
			// EndTime 上游是 "2026-09-14 15:52:38" 形态，前端取日期部分展示。
			EndTime: pk.EndTime,
		})
	}
	it.Packages = packs
	it.Remain = int64(sum.General) // 可用余额 = 通用积分（不含 Work）
	it.Size = int64(sum.Limit)
}
