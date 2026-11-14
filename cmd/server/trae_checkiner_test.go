// trae_checkiner_test.go 钉住 Trae → pool 的余额换算口径。
// 这部分最容易出错：取错字段会让快耗尽的账号看起来健康。
package main

import (
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/trae"
)

// TestTraeCreditsSnapshotUsesGeneral Remain 必须取通用积分，不含 Work。
func TestTraeCreditsSnapshotUsesGeneral(t *testing.T) {
	sum := &trae.CreditsSummary{
		General: 750, Work: 5000, Total: 5750, Limit: 1000,
		Packs: []trae.CreditPack{
			{ProductID: 208, CreditsLimit: 1000, CreditsUsed: 250, EndTime: "2099-12-01 00:00:00"},
			{ProductID: 209, CreditsLimit: 5000, CreditsUsed: 0, EndTime: "2099-12-01 00:00:00"},
		},
	}
	snap := traeCreditsSnapshot(sum)
	if snap.Remain != 750 {
		t.Fatalf("Remain 应取 General=750，实际 %d", snap.Remain)
	}
	if snap.Total != 1000 {
		t.Fatalf("Total 应取 Limit=1000，实际 %d", snap.Total)
	}
	// Work 包不进入到期批次（不参与对话，进批次会让最早到期路由选错账号）。
	if len(snap.ExpiryBuckets) != 1 || snap.ExpiryBuckets[0].Remain != 750 {
		t.Fatalf("批次只应含通用积分 750: %+v", snap.ExpiryBuckets)
	}
}

// TestTraeCreditsSnapshotGroupsByDay 同一天多个包合计，ExpireAt 取当日最早。
func TestTraeCreditsSnapshotGroupsByDay(t *testing.T) {
	sum := &trae.CreditsSummary{
		General: 30, Limit: 30,
		Packs: []trae.CreditPack{
			{ProductID: 208, CreditsLimit: 10, CreditsUsed: 0, EndTime: "2099-12-01 18:00:00"},
			{ProductID: 208, CreditsLimit: 20, CreditsUsed: 0, EndTime: "2099-12-01 09:00:00"},
			{ProductID: 208, CreditsLimit: 99, CreditsUsed: 0, EndTime: "2099-12-05 09:00:00"},
		},
	}
	snap := traeCreditsSnapshot(sum)
	if len(snap.ExpiryBuckets) != 2 {
		t.Fatalf("应按自然日聚合成 2 批，实际 %d: %+v", len(snap.ExpiryBuckets), snap.ExpiryBuckets)
	}
	first := snap.ExpiryBuckets[0]
	if first.Date != "2099-12-01" || first.Remain != 30 {
		t.Fatalf("首日应合计 30: %+v", first)
	}
	if got := first.ExpireAt.Hour(); got != 9 {
		t.Fatalf("ExpireAt 应取当日最早 9 点，实际 %d", got)
	}
	if !snap.EarliestAt.Equal(first.ExpireAt) || snap.EarliestRemaining != 30 {
		t.Fatalf("最早批次应同步: at=%v remain=%d", snap.EarliestAt, snap.EarliestRemaining)
	}
}

// TestTraeCreditsSnapshotSkipsExpiredAndEmpty 过期与零剩余包不进批次。
func TestTraeCreditsSnapshotSkipsExpiredAndEmpty(t *testing.T) {
	sum := &trae.CreditsSummary{
		Packs: []trae.CreditPack{
			{ProductID: 208, CreditsLimit: 100, CreditsUsed: 100, EndTime: "2099-12-01 00:00:00"}, // 用完
			{ProductID: 208, CreditsLimit: 100, CreditsUsed: 0, EndTime: "2000-01-01 00:00:00"},   // 已过期
			{ProductID: 208, CreditsLimit: 100, CreditsUsed: 0, EndTime: ""},                     // 无到期
		},
	}
	snap := traeCreditsSnapshot(sum)
	if len(snap.ExpiryBuckets) != 0 {
		t.Fatalf("不应有有效批次: %+v", snap.ExpiryBuckets)
	}
}

// TestTraeCreditsSnapshotNilSummary 空汇总不 panic。
func TestTraeCreditsSnapshotNilSummary(t *testing.T) {
	snap := traeCreditsSnapshot(nil)
	if snap.Remain != 0 || len(snap.ExpiryBuckets) != 0 {
		t.Fatalf("空汇总应返回零值: %+v", snap)
	}
}
