package panel

import (
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/trae"
)

// TestTraeCreditsSnapshot 转换规则：只算通用积分、同日合并、跳过过期与零剩余。
//
// 回归：此前 Trae 积分只调 SetCredits（只写余额），从不写到期批次，
// 于是「积分到期提醒」与「积分构成」两处对 Trae 账号永远显示无数据。
func TestTraeCreditsSnapshot(t *testing.T) {
	now := time.Date(2026, 11, 13, 12, 0, 0, 0, time.Local)
	sum := &trae.CreditsSummary{
		General: 1500,
		Work:    800,
		Total:   2300,
		Limit:   3000,
		Packs: []trae.CreditPack{
			// 通用，两个同一天到期 → 合并
			{ProductID: 208, CreditsLimit: 500, CreditsUsed: 0, EndTime: "2026-12-01 10:00:00"},
			{ProductID: 208, CreditsLimit: 300, CreditsUsed: 100, EndTime: "2026-12-01 18:00:00"},
			// 通用，另一天且 7 天内 → 计入 Expiring
			{ProductID: 208, CreditsLimit: 200, CreditsUsed: 0, EndTime: "2026-11-18 00:00:00"},
			// Work 积分 → 必须排除
			{ProductID: 209, CreditsLimit: 800, CreditsUsed: 0, EndTime: "2026-12-01 10:00:00"},
			// 已过期 → 排除
			{ProductID: 208, CreditsLimit: 999, CreditsUsed: 0, EndTime: "2026-01-01 00:00:00"},
			// 已用完 → 排除
			{ProductID: 208, CreditsLimit: 100, CreditsUsed: 100, EndTime: "2026-12-05 00:00:00"},
			// 无到期时间 → 排除
			{ProductID: 208, CreditsLimit: 400, CreditsUsed: 0},
		},
	}

	snap := traeCreditsSnapshot(sum, now)

	if snap.Remain != 1500 || snap.Total != 3000 {
		t.Errorf("余额 = %d/%d, want 1500/3000（用 General 不是 Total）", snap.Remain, snap.Total)
	}
	if len(snap.ExpiryBuckets) != 2 {
		t.Fatalf("批次数 = %d, want 2（Work/过期/已用完/无到期都应排除）: %+v",
			len(snap.ExpiryBuckets), snap.ExpiryBuckets)
	}
	// 按日期升序：11-18 在前。
	first := snap.ExpiryBuckets[0]
	if first.Date != "2026-11-18" || first.Remain != 200 {
		t.Errorf("首批 = %+v, want 2026-11-18 / 200", first)
	}
	// 12-01 两个包合并：500 + (300-100) = 700。
	second := snap.ExpiryBuckets[1]
	if second.Date != "2026-12-01" || second.Remain != 700 {
		t.Errorf("次日批次 = %+v, want 2026-12-01 / 700", second)
	}
	// 最早批次 = 11-18。
	if !snap.EarliestAt.Equal(first.ExpireAt) || snap.EarliestRemaining != 200 {
		t.Errorf("最早批次 = %v/%d", snap.EarliestAt, snap.EarliestRemaining)
	}
	// 7 天内只有 11-18 的 200。
	if snap.Expiring != 200 {
		t.Errorf("快过期 = %d, want 200（只有 11-18 在 7 天内）", snap.Expiring)
	}
}

// TestTraeCreditsSnapshotEmpty 全是 Work 或无到期时，不产生批次。
func TestTraeCreditsSnapshotEmpty(t *testing.T) {
	now := time.Now()
	sum := &trae.CreditsSummary{
		General: 100,
		Packs: []trae.CreditPack{
			{ProductID: 209, CreditsLimit: 500, EndTime: "2027-01-01 00:00:00"},
		},
	}
	snap := traeCreditsSnapshot(sum, now)
	if len(snap.ExpiryBuckets) != 0 {
		t.Errorf("Work 积分不应产生到期批次: %+v", snap.ExpiryBuckets)
	}
	if !snap.EarliestAt.IsZero() {
		t.Error("无批次时 EarliestAt 应为零值")
	}
	if snap.Remain != 100 {
		t.Errorf("Remain = %d, want 100", snap.Remain)
	}
}
