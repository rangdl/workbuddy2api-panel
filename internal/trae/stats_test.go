package trae

import (
	"testing"
	"time"
)

// TestStatsBeginFinish 在途计数必须归零，成功/失败分别累计。
func TestStatsBeginFinish(t *testing.T) {
	s := NewStats()

	fin := s.Begin("u1")
	if v := s.Get("u1"); v.InFlight != 1 {
		t.Fatalf("在途 = %d, want 1", v.InFlight)
	}
	fin(true, UsageDelta{Model: "glm-5.3",
		HasPromptTokens: true, PromptTokens: 10,
		HasCompletionTokens: true, CompletionTokens: 20,
		HasTotalTokens: true, TotalTokens: 30})

	v := s.Get("u1")
	if v.InFlight != 0 {
		t.Errorf("结束后在途 = %d, want 0", v.InFlight)
	}
	if v.SuccessCount != 1 || v.ErrTotal != 0 {
		t.Errorf("成功/失败 = %d/%d, want 1/0", v.SuccessCount, v.ErrTotal)
	}
	if v.TokenUsage.PromptTokens != 10 || v.TokenUsage.CompletionTokens != 20 || v.TokenUsage.TotalTokens != 30 {
		t.Errorf("token 累计不符: %+v", v.TokenUsage)
	}
	if v.TokenUsage.LastModel != "glm-5.3" {
		t.Errorf("LastModel = %q", v.TokenUsage.LastModel)
	}
	if v.LastSuccess == nil {
		t.Error("LastSuccess 应有值")
	}

	s.Begin("u1")(false, UsageDelta{})
	v = s.Get("u1")
	if v.SuccessCount != 1 || v.ErrTotal != 1 {
		t.Errorf("失败后成功/失败 = %d/%d, want 1/1", v.SuccessCount, v.ErrTotal)
	}
	if v.LastErr == nil {
		t.Error("LastErr 应有值")
	}
}

// TestStatsFinishIdempotent finish 重复调用只结算一次（在途不能变负）。
func TestStatsFinishIdempotent(t *testing.T) {
	s := NewStats()
	fin := s.Begin("u1")
	fin(true, UsageDelta{})
	fin(true, UsageDelta{})
	v := s.Get("u1")
	if v.SuccessCount != 1 {
		t.Errorf("重复 finish 后成功数 = %d, want 1", v.SuccessCount)
	}
	if v.InFlight != 0 {
		t.Errorf("在途 = %d, want 0", v.InFlight)
	}
}

// TestStatsConcurrentInFlight 并发 Begin 的在途计数准确。
func TestStatsConcurrentInFlight(t *testing.T) {
	s := NewStats()
	const n = 50
	done := make(chan struct{}, n)
	for i := 0; i < n; i++ {
		go func() {
			fin := s.Begin("u1")
			done <- struct{}{}
			_ = fin
		}()
	}
	for i := 0; i < n; i++ {
		<-done
	}
	// 所有 Begin 已执行且都未 finish → 在途应等于 n。
	if v := s.Get("u1"); v.InFlight != n {
		t.Errorf("在途 = %d, want %d", v.InFlight, n)
	}
}

// TestStatsTokensPerSecondDroppedOnFailure 失败时不应保留上一次的吞吐。
func TestStatsTokensPerSecondDroppedOnFailure(t *testing.T) {
	s := NewStats()
	s.Begin("u1")(true, UsageDelta{HasTokensPerSecond: true, TokensPerSecond: 42})
	if v := s.Get("u1"); v.TokenUsage.LastTokensPerSecond == nil {
		t.Fatal("成功时应记录吞吐")
	}
	s.Begin("u1")(false, UsageDelta{HasTokensPerSecond: true, TokensPerSecond: 99})
	if v := s.Get("u1"); v.TokenUsage.LastTokensPerSecond != nil {
		t.Errorf("失败后应清空吞吐，实际 %v", *v.TokenUsage.LastTokensPerSecond)
	}
}

// TestStatsNoteRemainDoesNotZeroLimit notify_usage 只有分解没有上限，
// 不能把已有的额度上限覆盖成 0（否则面板把「未知」画成「0 额度」）。
func TestStatsNoteRemainDoesNotZeroLimit(t *testing.T) {
	s := NewStats()
	s.NoteCredits("u1", 1000, 200, 1200, 5000)
	v := s.Get("u1")
	if v.CreditsLimit != 5000 || v.Credits != 1200 {
		t.Fatalf("初始积分 = %+v", v)
	}

	s.NoteRemain("u1", 800, 150)
	v = s.Get("u1")
	if v.CreditsIDE != 800 || v.CreditsWork != 150 {
		t.Errorf("分解未更新: %+v", v)
	}
	if v.CreditsLimit != 5000 {
		t.Errorf("额度上限被覆盖成 %v, want 5000", v.CreditsLimit)
	}
	if v.CreditsAt == nil {
		t.Error("CreditsAt 应刷新")
	}
}

// TestStatsNoteRemainWithoutLimit 未查过积分时，只剩 notify_usage 的分解，
// 合计取两者之和且 Limit 为 0（前端据此不画进度条）。
func TestStatsNoteRemainWithoutLimit(t *testing.T) {
	s := NewStats()
	s.NoteRemain("u1", 300, 100)
	v := s.Get("u1")
	if v.Credits != 400 {
		t.Errorf("合计 = %v, want 400", v.Credits)
	}
	if v.CreditsLimit != 0 {
		t.Errorf("Limit 应为 0（未知），实际 %v", v.CreditsLimit)
	}
}

// TestStatsSnapshotStable 快照按 uid 升序，输出稳定。
func TestStatsSnapshotStable(t *testing.T) {
	s := NewStats()
	s.Begin("b")(true, UsageDelta{})
	s.Begin("a")(true, UsageDelta{})
	got := s.Snapshot()
	if len(got) != 2 || got[0].UID != "a" || got[1].UID != "b" {
		t.Fatalf("顺序不符: %+v", got)
	}
}

// TestStatsReset 删除账号后计数清零。
func TestStatsReset(t *testing.T) {
	s := NewStats()
	s.Begin("u1")(true, UsageDelta{})
	s.Reset("u1")
	v := s.Get("u1")
	if v.SuccessCount != 0 || v.InFlight != 0 {
		t.Errorf("Reset 后仍有数据: %+v", v)
	}
}

// TestStatsNilSafe 零值 Stats 不 panic（handler 未注入时的兜底路径）。
func TestStatsNilSafe(t *testing.T) {
	var s *Stats
	fin := s.Begin("u1")
	fin(true, UsageDelta{})
	s.NoteRemain("u1", 1, 2)
	s.NoteCredits("u1", 1, 2, 3, 4)
	s.Reset("u1")
	if got := s.Snapshot(); got != nil {
		t.Errorf("nil Stats 快照应为 nil, got %+v", got)
	}
	if v := s.Get("u1"); v.UID != "u1" {
		t.Errorf("nil Stats Get 应返回带 uid 的空视图, got %+v", v)
	}
}

// TestStatsLatency 延迟记录为毫秒。
func TestStatsLatency(t *testing.T) {
	s := NewStats()
	s.Begin("u1")(true, UsageDelta{HasLatencyMs: true, LatencyMs: 1234})
	if v := s.Get("u1"); v.LastLatencyMs != 1234 {
		t.Errorf("LastLatencyMs = %d, want 1234", v.LastLatencyMs)
	}
}

// TestStatsZeroValueAccount 从未被调用的账号返回零值视图而不是 nil，
// 面板据此渲染空行（而不是整行消失）。
func TestStatsZeroValueAccount(t *testing.T) {
	s := NewStats()
	v := s.Get("never")
	if v.UID != "never" || v.SuccessCount != 0 || v.InFlight != 0 {
		t.Errorf("零值视图不符: %+v", v)
	}
	if v.LastSuccess != nil || v.CreditsAt != nil {
		t.Error("零值视图的时间字段应为 nil")
	}
}

// TestStatsLastUsedAtFollowsSuccess last_used_at 跟随最近成功时间。
func TestStatsLastUsedAtFollowsSuccess(t *testing.T) {
	s := NewStats()
	before := time.Now()
	s.Begin("u1")(true, UsageDelta{})
	v := s.Get("u1")
	if v.TokenUsage.LastUsedAt == nil {
		t.Fatal("LastUsedAt 应有值")
	}
	if v.TokenUsage.LastUsedAt.Before(before.Add(-time.Second)) {
		t.Errorf("LastUsedAt 过早: %v", v.TokenUsage.LastUsedAt)
	}
}
