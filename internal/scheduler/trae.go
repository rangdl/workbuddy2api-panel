// trae.go 让 Trae 账号参与**统一签到链路**。
//
// 背景：Trae 账号早已合进同一个调度池（cmd/server/trae_config.go 的
// syncTraeAccountsToPool），选号与面板展示都是同一张表。但签到此前还挂在
// internal/trae 的独立 Scheduler 上——两套循环、两套冷却、两个入口。
// 于是"池里的账号"有一部分实际上不参与池的保号任务。
//
// 为什么不直接把 Trae 塞进 upstream.Client：那个包的每个方法签名都是
// *auth.Auth（CodeBuddy 的凭证形态），Trae 的账号结构与协议完全不同。
// 本文件用窄接口做适配，改动被限制在签到/余额两条链路上；任务、盲盒、
// 连登、旅行这些 CodeBuddy 成长体系专属能力不在此列（Trae 上游没有
// 对应接口，强行接入只会得到一个空壳）。
package scheduler

import (
	"context"
	"log"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/logfmt"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
)

// TraeCheckinResult Trae 单账号签到结果。
type TraeCheckinResult struct {
	// Already 上游确认本账号今日已签（预检命中或 9004/9095 幂等拒绝）。
	Already bool
	// DeviceAlready 该**设备**今日已签（9095/9004）。限额按设备算，
	// 多账号共用同一设备号时只有第一个能签成——此标记为真但 Already 为假时，
	// 本账号今天其实没签成，不能标为已签。
	DeviceAlready bool
	// Unverified claim 返回成功但回查未翻转（可能未到账）。
	Unverified bool
	// Reward 本次奖励积分。
	Reward int64
	// Message 上游原文或可读原因。
	Message string
}

// TraeCheckiner Trae 签到与余额查询能力。
//
// 由 cmd/server 注入（它持有所需的 trae.Config 与 Client）。nil 表示没有
// Trae 上游，池里的 Trae 账号按"无能力"跳过——与未启用该上游时一致。
type TraeCheckiner interface {
	// CheckinTrae 对单个 Trae 账号执行签到（含预检、续期、幂等识别与回查确认）。
	CheckinTrae(ctx context.Context, uid string) (*TraeCheckinResult, error)
	// TraeCredits 查询单个 Trae 账号的余额与按日到期批次，结果已换算成
	// pool 的既有口径（Remain 取 General，不含 Work——对话只消耗 IDE 额度）。
	TraeCredits(ctx context.Context, uid string) (pool.CreditsSnapshot, error)
}

// SetTraeCheckiner 注入 Trae 签到适配（main 在构造后挂载；nil = 不启用）。
func (s *Scheduler) SetTraeCheckiner(t TraeCheckiner) {
	s.cfg.Trae = t
}

// HasTraeAccounts 池里是否有 Trae 账号（供主循环决定是否调 RunTraeCheckinNow）。
func (s *Scheduler) HasTraeAccounts() bool {
	if s.cfg.Pool == nil {
		return false
	}
	for _, st := range s.cfg.Pool.List() {
		if st.Backend == pool.BackendTrae {
			return true
		}
	}
	return false
}

// RunTraeCheckinNow 立即对所有 Trae 账号签到 + 刷新余额。
//
// 与 CodeBuddy 的 RunCheckinNow 同一套外围语义：跳过禁用账号（受
// include_disabled_in_tasks 控制）、跳过冷却中账号、签到后刷新余额并按余额解冻。
func (s *Scheduler) RunTraeCheckinNow(ctx context.Context) {
	if s.cfg.Trae == nil || s.cfg.Pool == nil {
		return
	}
	includeDisabled := s.includeDisabledInTasks()
	expiringSoon := s.ExpiringSoonWindow()
	for _, st := range s.cfg.Pool.List() {
		// 只处理 Trae：CodeBuddy 走 RunCheckinNow，两者各有各的协议链路。
		if st.Backend != pool.BackendTrae {
			continue
		}
		if st.Disabled && !includeDisabled {
			continue
		}
		// 冷却中的账号也参与签到（签到就是为了解冻它们）——与 CodeBuddy 一致。
		// 唯一的例外是禁用已由上面处理，这里不再额外过滤 Cooling。
		if ctx.Err() != nil {
			return
		}

		res, err := s.cfg.Trae.CheckinTrae(ctx, st.UID)
		switch {
		case err != nil:
			log.Printf("checkin trae %s: %v", logfmt.Label(st.UID, st.Nickname), err)
		case res.Unverified:
			// claim 说成功但回查没翻转：不标已签，交下轮重试。
			log.Printf("checkin trae %s: %s", logfmt.Label(st.UID, st.Nickname), res.Message)
		case res.DeviceAlready:
			// 设备维度已签：本账号没签成，不能标已签。
			log.Printf("checkin trae %s: 设备今日已签，本账号未签到", logfmt.Label(st.UID, st.Nickname))
		case res.Already:
			s.cfg.Pool.NoteCheckinDone(st.UID)
			log.Printf("checkin trae %s: 今日已签到（幂等）", logfmt.Label(st.UID, st.Nickname))
		default:
			s.cfg.Pool.NoteCheckinDone(st.UID)
			log.Printf("checkin trae %s: 签到成功 +%d", logfmt.Label(st.UID, st.Nickname), res.Reward)
		}

		// 签到后刷新余额（与 CodeBuddy 同口径：余额是 credit_floor 判定与
		// 面板展示的唯一数据源）。
		snap, cerr := s.cfg.Trae.TraeCredits(ctx, st.UID)
		if cerr != nil {
			log.Printf("trae-credits %s: %v", logfmt.Label(st.UID, st.Nickname), cerr)
			continue
		}
		// 快过期子集按调度器统一的窗口口径计算，避免适配层再写一遍语义。
		snap.Expiring = expiringWithin(snap.ExpiryBuckets, expiringSoon, time.Now())
		s.cfg.Pool.ReenableIfCredits(st.UID, snap.Remain, snap.Total)
		s.cfg.Pool.SetCreditsSnapshot(st.UID, snap)
	}
}

// expiringWithin 汇总 now+window 内到期的批次额度。window<=0 表示不设门槛。
//
// 与 CodeBuddy 侧 poolExpiryBuckets/ExpiringSoonWindow 的口径保持一致：
// 只统计有明确到期时刻且落在窗口内的批次。
func expiringWithin(buckets []pool.ExpiryBucket, window time.Duration, now time.Time) int64 {
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
