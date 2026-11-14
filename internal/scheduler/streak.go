// streak.go 连登管家的排程入口。
//
// 状态机已搬到账号对象（backend/codebuddy/features.go 的 Account.RunStreak）：
// 补签 → 礼包/补偿 → 兑换已解锁档位 → 抽完所有 chances。
//
// 本文件只保留调度职责：由签到排程末尾调用（也可面板手动触发）。
package scheduler

import (
	"context"
)

// RunStreakBonusNow 对所有适用账号执行连登兑换 + 抽奖（幂等：locked/无次数自动跳过）。
//
// 由签到排程（RunCheckinNow）末尾调用；也可面板手动触发。
func (s *Scheduler) RunStreakBonusNow() {
	s.RunStreakBonusContext(context.Background())
}

// RunStreakBonusContext 同 RunStreakBonusNow，但可被 ctx 中断（停机时立即退出）。
func (s *Scheduler) RunStreakBonusContext(ctx context.Context) {
	s.cfg.Pool.StreakAll(ctx)
}
