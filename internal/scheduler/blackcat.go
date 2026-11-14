// blackcat.go 夜猫子任务的排程入口。
//
// 状态机已搬到账号对象（backend/codebuddy/features.go 的 Account.RunBlackcat）：
// 查还需补足的对话次数 → 逐条发短对话并上报事件链。
//
// 本文件只保留调度职责：夜间窗口守卫 + 排程触发。
package scheduler

import (
	"context"
	"log"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// nowForNightWindow 夜猫子窗口判定所用时钟（生产恒为 time.Now）。
//
// 之所以留一个可替换点：窗口守卫**先于**账号循环，若不钉住时钟，「企业号门控是否生效」
// 这条断言的判别力就完全取决于运行环境的墙钟——本机白天跑等于该用例什么都没测，
// 只有 CI 恰好落在 UTC 夜里才真正执行。属典型的「随环境静默失效」的测试。
var nowForNightWindow = time.Now

// RunBlackcatNow 对所有适用账号执行夜猫子对话补足（窗口外跳过）。
//
// 由 blackcat_hours 排程（默认 [23]）触发；执行前二次校验 InNightWindow。
// 池的 BlackcatAll 不含 paused 账号：夜猫子是唯一「整任务都是真实模型对话」的
// 特色能力，与「让位防风控」正面冲突（旅行/连登/补偿都是纯 RPC，照常跑）。
func (s *Scheduler) RunBlackcatNow() {
	s.runBlackcat(context.Background())
}

// RunBlackcatContext 同 RunBlackcatNow，但可被 ctx 中断。
func (s *Scheduler) RunBlackcatContext(ctx context.Context) {
	s.runBlackcat(ctx)
}

func (s *Scheduler) runBlackcat(ctx context.Context) {
	if !upstream.InNightWindow(nowForNightWindow()) {
		log.Printf("blackcat: 当前不在 23:00–08:00 计数窗口，跳过")
		return
	}
	pool.SetCapabilityDelay(nightChatAccountDelay)
	s.cfg.Pool.BlackcatAll(ctx)
}
