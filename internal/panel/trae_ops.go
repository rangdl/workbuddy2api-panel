// trae_ops.go 面板的 Trae 运维接口：签到（全部 / 单账号）、积分查询、签到结果查询。
//
// 与 trae_config.go 的分工：那边管配置与凭证，这边管对上游的主动操作。
package panel

import (
	"context"
	"net/http"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/trae"
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
	// 回写运行期统计：面板的积分列优先用这份权威数据（含额度上限），
	// 直到下一次对话的 notify_usage 覆盖它。
	if p.cfg.TraeStats != nil && sum != nil {
		p.cfg.TraeStats.NoteCredits(uid, sum.General, sum.Work, sum.Total, sum.Limit)
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
