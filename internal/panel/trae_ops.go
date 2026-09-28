// trae_ops.go 面板的 Trae 运维接口：签到（全部 / 单账号）、积分查询、签到结果查询。
//
// 与 trae_config.go 的分工：那边管配置与凭证，这边管对上游的主动操作。
package panel

import (
	"context"
	"net/http"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/trae"
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
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "results": outcomes})
}

// traeAccountCheckin 单账号签到。
func (p *Panel) traeAccountCheckin(w http.ResponseWriter, r *http.Request) {
	if p.cfg.TraeClient == nil || p.cfg.TraePath == "" {
		writeErr(w, http.StatusNotImplemented, "trae client not available")
		return
	}
	cfg, err := trae.Load(p.cfg.TraePath)
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
	res, err := p.cfg.TraeClient.Checkin(ctx, cfg, acc)
	if err != nil {
		// 错误分类后一并回传，便于面板给出可操作提示。
		if apiErr, ok := err.(*trae.APIError); ok {
			cls := apiErr.Classification()
			writeJSON(w, http.StatusOK, map[string]any{
				"ok": false, "uid": uid, "error_type": string(cls.Kind),
				"message": cls.Message, "disable": cls.Disable,
			})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "uid": uid, "message": err.Error()})
		return
	}
	if p.cfg.TraeCooldowns != nil {
		p.cfg.TraeCooldowns.Clear(uid) // 签到成功即清冷却
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "uid": uid, "already": res.Already,
		"reward": res.Reward, "message": res.Message,
	})
}

// traeAccountCredits 查询单账号积分包明细与汇总。
func (p *Panel) traeAccountCredits(w http.ResponseWriter, r *http.Request) {
	if p.cfg.TraeClient == nil || p.cfg.TraePath == "" {
		writeErr(w, http.StatusNotImplemented, "trae client not available")
		return
	}
	cfg, err := trae.Load(p.cfg.TraePath)
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
