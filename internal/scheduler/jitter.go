// jitter.go 定时任务的时刻抖动。
//
// 为什么需要：整点触发是最强的机器特征。定时任务若每天在 9:00:00、每 5 分钟
// 在 :00 秒精确发请求，上游看到的是「同一批账号、同一毫秒、固定周期」的行为模式，
// 与真实用户的使用形态完全不同——这正是平台风控最容易识别的信号之一，
// 且与本项目其余「避免机器特征」的努力（出站指纹、账号间限速、到站抖动）方向一致。
//
// 三类入口都加抖动，但形态不同：
//   - 整点排程（签到/旅行/活跃/保活/夜猫子/成长）：在计划时刻上加一个**只向后**
//     的随机延迟（0..window）。不能提前——提前会让「21 点签到」在 20:59 发出去，
//     跨日边界、上游额度刷新时刻都可能没到。
//   - 周期 ticker（余额刷新/任务清扫）：每轮间隔加 ±window 抖动。基准是 interval
//     本身，不需要担心跨日语义。
//
// 抖动量取「间隔或整点」的一个小比例，且设上限：抖得太大会让「签到时点 9 点」
// 实际漂到 10 点，违背用户配置的语义。宁小勿大——防的是「毫秒级精确」这个特征，
// 不是要打散到小时级。
package scheduler

import (
	"math/rand"
	"time"
)

// fireJitterWindow 整点排程的单次抖动上限（只向后加 0..window）。
//
// 取 10 分钟：对「9 点签到」这类配置，最坏情况是 9:10 执行，语义仍然是"9 点那档"，
// 用户感知不到；而对风控而言，「每天 9:00:00.000」与「每天 9:0x:xx.xxx」的差别
// 是决定性的。窗口再大就会落到下一个语义档（如 9 点配置跑到 10 点），不取。
const fireJitterWindow = 10 * time.Minute

// fireJitterFn 抖动源。生产恒为 fireJitter；测试注入零值以断言精确整点。
//
// 为什么要留注入点：排程断言的语义是「下一次唤醒落在哪个档位」，而抖动让
// 返回时刻带 0..10 分钟的随机偏移。若让测试直接承受随机值，就只能写成
// 「落在 [9:00, 9:10) 之间」这种弱断言，一旦抖动实现出错（比如加错方向、
// 算错窗口）也会通过。注入点让排程语义的测试保持精确。
var fireJitterFn = fireJitter

// fireJitter 返回 [0, fireJitterWindow) 内的随机延迟，加在整点排程的计划时刻上。
//
// 用 rand.Float64 而不是 Int63n 取模：窗口值虽小，但两者成本一样，
// 浮点路径不受整数取模偏差影响，读起来也更直白。
func fireJitter() time.Duration {
	if fireJitterWindow <= 0 {
		return 0
	}
	return time.Duration(rand.Float64() * float64(fireJitterWindow))
}

// tickerJitter 给周期 ticker 的**本轮间隔**加 ±interval×fraction 抖动。
//
// fraction 取 0.15：余额刷新 5 分钟 → 4.25~5.75 分钟，既打散了「每 5 分钟整点
// 一刷」的节律，又不会让刷新明显变稀或变密。下限钳到 1 秒，避免极小间隔抖动成 0
// 导致空转热循环。
func tickerJitter(interval time.Duration, fraction float64) time.Duration {
	if interval <= 0 {
		return interval
	}
	d := interval + time.Duration((rand.Float64()*2-1)*fraction*float64(interval))
	if d < time.Second {
		d = time.Second
	}
	return d
}

// tickerJitterFraction 周期任务的抖动比例（±15%）。
const tickerJitterFraction = 0.15
