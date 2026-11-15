package scheduler

import (
	"testing"
	"time"
)

// TestFireJitterBoundedAndNonNegative 整点抖动必须落在 [0, window) 且不为负。
//
// 为什么只向后：提前会让「21 点签到」在 20:59 发出去，跨日边界与上游额度刷新
// 时刻都可能没到。这条断言锁死方向——若哪天有人改成 ±window，负值会立刻暴露。
func TestFireJitterBoundedAndNonNegative(t *testing.T) {
	const n = 500
	var sawNonZero bool
	for i := 0; i < n; i++ {
		d := fireJitter()
		if d < 0 {
			t.Fatalf("抖动为负 %v：只允许向后加", d)
		}
		if d >= fireJitterWindow {
			t.Fatalf("抖动 %v 超出窗口 %v", d, fireJitterWindow)
		}
		if d > 0 {
			sawNonZero = true
		}
	}
	// 500 次全为 0 的概率实质为零，说明抖动源没接上（例如函数体被改成 return 0）。
	if !sawNonZero {
		t.Fatal("500 次抖动全为 0，抖动源疑似未生效")
	}
}

// TestFireJitterIsDistributed 抖动必须有实际分散度，不能是固定值。
//
// 固定值只是把「9:00:00」平移成「9:00:07」，机器特征依然存在。
func TestFireJitterIsDistributed(t *testing.T) {
	seen := map[time.Duration]bool{}
	for i := 0; i < 200; i++ {
		seen[fireJitter()] = true
	}
	if len(seen) < 50 {
		t.Fatalf("200 次抖动只有 %d 个不同值，分散度不足（疑似固定值）", len(seen))
	}
}

// TestNextWakeAppliesJitterWhenEnabled 排程结果确实带上了抖动（用真实抖动源）。
//
// 其余排程测试把 fireJitterFn 注入为 0 以断言精确档位；这条反过来验证
// 「生产路径确实会加抖动」——没有它，抖动可能被后续重构悄悄摘掉而测试全绿。
func TestNextWakeAppliesJitterWhenEnabled(t *testing.T) {
	old := fireJitterFn
	fireJitterFn = fireJitter
	defer func() { fireJitterFn = old }()

	s := New(Config{CheckinHours: []int{9}, KeepaliveHours: []int{22},
		TravelDisabled: true, ActivityDisabled: true})
	base := time.Date(2026, 9, 11, 20, 0, 0, 0, time.Local)
	at, _ := s.nextWake(base)

	lo := time.Date(2026, 9, 11, 22, 0, 0, 0, time.Local)
	hi := lo.Add(fireJitterWindow)
	if at.Before(lo) || !at.Before(hi) {
		t.Fatalf("next=%v 应落在 [%v, %v)（22 点档 + 向后抖动）", at, lo, hi)
	}
}

// TestTickerJitterWithinBoundsAndClamped 周期抖动落在 ±fraction 内且有下限钳制。
func TestTickerJitterWithinBoundsAndClamped(t *testing.T) {
	const interval = 5 * time.Minute
	lo := time.Duration(float64(interval) * (1 - tickerJitterFraction))
	hi := time.Duration(float64(interval) * (1 + tickerJitterFraction))
	for i := 0; i < 300; i++ {
		d := tickerJitter(interval, tickerJitterFraction)
		if d < lo || d > hi {
			t.Fatalf("抖动后间隔 %v 超出 [%v, %v]", d, lo, hi)
		}
	}
	// 极小间隔抖动后不得低于 1 秒，否则会退化成空转热循环。
	if d := tickerJitter(100*time.Millisecond, tickerJitterFraction); d < time.Second {
		t.Fatalf("极小间隔抖动后 %v，应钳到 1s 下限", d)
	}
	// interval<=0 原样返回（调用方语义是「不启动」）。
	if d := tickerJitter(0, tickerJitterFraction); d != 0 {
		t.Fatalf("interval=0 应原样返回 0，得到 %v", d)
	}
}
