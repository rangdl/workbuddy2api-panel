// companion.go 特色能力在调度器侧的共用常量与工具。
//
// 状态机已全部搬到账号对象（backend/codebuddy/features.go），这里只保留
// 「调度器要知道的节奏参数」与「日志格式化」这类与上游无关的东西。
package scheduler

import "time"

// 账号间限速（防风控）。测试可置 0。
//
// 实际等待发生在 pool 的批量调度层；调度器在每次巡检前把它同步给 pool。
var (
	// activityAccountDelay 活跃上报的账号间限速。
	activityAccountDelay = 800 * time.Millisecond
	// nightChatAccountDelay 夜猫子对话的账号间限速。
	nightChatAccountDelay = 800 * time.Millisecond
)

// activityStreakSettle 已迁到账号实现（backend/codebuddy/features.go）——
// 那里才有真正的等待逻辑，本包不再保留同名变量（保留了也是空转，
// 曾导致测试改 0 无效、整包被拖慢 70 秒）。测试请用
// codebuddy.SetActivityStreakSettle(0)。

// cstZone 上游每日重置按自然日 00:00 CST (Asia/Shanghai)。
// 中国无夏令时，固定 +8 即可，不依赖容器 tzdata。
var cstZone = time.FixedZone("CST", 8*60*60)

// travelDay 返回 t 所属的上游自然日（CST），格式 2006-01-02。
//
// 生产的旅行日判定已迁到 pool/capabilities.go 与 backend/codebuddy/features.go
// （各留一份，因为池不能 import backend）。本函数仅保留给 CST 换算的单测。
func travelDay(t time.Time) string {
	return t.In(cstZone).Format("2006-01-02")
}
