// compensation.go 补偿活动的排程入口（issue #189）。
//
// 状态机已搬到账号对象（backend/codebuddy/features.go 的 Account.CompensationStatus /
// CompensationClaim）：先查状态 → 待领才领，解决三个原本无法区分的情形：
//
//	① 没有补偿        ② 已经领过        ③ 真的领取失败
//
// 旧写法 `if credit, err := ...; err == nil { log }` 让三者同构，补偿带截止日
// （实测 2026-10-16 那次是 10/22），签到停摆就会静默过期。现在「可领未领」
// 在日志和面板上都能显式看见。
package scheduler

import (
	"context"

	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
)

// CompensationStatusAll 查全部适用账号的补偿状态（只读，不领取）。
func (s *Scheduler) CompensationStatusAll() []pool.CompensationResult {
	return s.cfg.Pool.CompensationStatusAll(context.Background())
}

// ClaimCompensationNow 逐号「查状态 → 待领则领」，返回逐号结果。
//
// 供面板手动按钮与签到排程共用；单号失败不影响其余账号。
func (s *Scheduler) ClaimCompensationNow() []pool.CompensationResult {
	return s.cfg.Pool.CompensationClaimAll(context.Background())
}

// ClaimCompensationFor 单号领取补偿（面板单号按钮）；账号不在池中返回 error。
func (s *Scheduler) ClaimCompensationFor(uid string) (pool.CompensationResult, error) {
	return s.cfg.Pool.CompensationClaimOne(context.Background(), uid)
}

// SummarizeCompensation 汇总逐号结果（面板顶部计数卡片用）。
//
// 单独统计 Errored 是为了让调用方能区分「没有可领」与「一个都没查成」：上游故障
// 或 token 全过期时，每个账号都会走 error 分支。若只按 Claimed 计数，面板会把
// 「全部查询失败」渲染成「当前没有待领补偿」——正是本 PR 要消灭的那类静默失败。
type CompensationSummary struct {
	Total   int   `json:"total"`
	Claimed int   `json:"claimed"`
	Skipped int   `json:"skipped"`
	Errored int   `json:"errored"`
	Credit  int64 `json:"credit"`
}

// OK 是否全部账号都拿到了确定结论（无错误）。
func (s CompensationSummary) OK() bool { return s.Errored == 0 }

// SummarizeCompensation 汇总逐号结果。
//
// 以 Error 字符串（展示层字段）判定失败而非 Err（内部字段）：面板与测试都只
// 构造前者，两处口径必须一致，否则会出现「面板显示 2 个失败、汇总说 0 个」。
func SummarizeCompensation(results []pool.CompensationResult) CompensationSummary {
	var s CompensationSummary
	s.Total = len(results)
	for _, r := range results {
		switch {
		case r.Error != "" || r.Err != nil:
			s.Errored++
		case r.Claimed:
			s.Claimed++
			s.Credit += r.Credit
		default:
			s.Skipped++
		}
	}
	return s
}
