// trae_test.go 钉住「Trae 账号参与统一签到链路」：
// 池里的 Trae 账号会被 RunCheckinNow 一并签到、回写池状态并刷新余额；
// 设备维度已签与回查未确认都不标记已签。
package scheduler

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// fakeTraeAccount 最小 gateway.Account（backend=trae）。
type fakeTraeAccount struct{ uid string }

func (f fakeTraeAccount) UID() string      { return f.uid }
func (f fakeTraeAccount) Nickname() string { return "" }
func (f fakeTraeAccount) Realm() string    { return "cn" }
func (f fakeTraeAccount) Backend() string  { return "trae" }
func (f fakeTraeAccount) Enterprise() bool { return false }

// fakeTraeCheckiner 记录调用并返回预设结果。
type fakeTraeCheckiner struct {
	checkinUIDs []string
	creditsUIDs []string
	result      *TraeCheckinResult
	err         error
	snapshot    pool.CreditsSnapshot
	snapErr     error
}

func (f *fakeTraeCheckiner) CheckinTrae(_ context.Context, uid string) (*TraeCheckinResult, error) {
	f.checkinUIDs = append(f.checkinUIDs, uid)
	if f.err != nil {
		return nil, f.err
	}
	if f.result != nil {
		return f.result, nil
	}
	return &TraeCheckinResult{Message: "签到成功"}, nil
}

func (f *fakeTraeCheckiner) TraeCredits(_ context.Context, uid string) (pool.CreditsSnapshot, error) {
	f.creditsUIDs = append(f.creditsUIDs, uid)
	return f.snapshot, f.snapErr
}

// TestRunCheckinNowCoversTraeAccounts Trae 账号与 CodeBuddy 一起参与签到。
func TestRunCheckinNowCoversTraeAccounts(t *testing.T) {
	p := pool.New("")
	p.Add(fakeTraeAccount{uid: "t1"})
	f := &fakeTraeCheckiner{snapshot: pool.CreditsSnapshot{Remain: 750, Total: 1000}}
	s := New(Config{Pool: p})
	s.SetTraeCheckiner(f)

	s.RunTraeCheckinNow(context.Background())

	if len(f.checkinUIDs) != 1 || f.checkinUIDs[0] != "t1" {
		t.Fatalf("应签到 Trae 账号 t1，实际 %v", f.checkinUIDs)
	}
	st, ok := p.Status("t1")
	if !ok || !st.CheckinDone {
		t.Fatalf("签到后池状态应显示已签: ok=%v st=%+v", ok, st)
	}
	if st.Credits != 750 || st.CreditsTotal != 1000 {
		t.Fatalf("余额应写回池: credits=%d total=%d", st.Credits, st.CreditsTotal)
	}
}

// TestRunTraeCheckinSkipsCodeBuddy Trae 链路不碰 CodeBuddy 账号。
func TestRunTraeCheckinSkipsCodeBuddy(t *testing.T) {
	p := pool.New("")
	p.Add(pool.WrapAuth(&auth.Auth{UID: "cb1", AccessToken: "at", RefreshToken: "rt"}))
	p.Add(fakeTraeAccount{uid: "t1"})
	f := &fakeTraeCheckiner{}
	s := New(Config{Pool: p})
	s.SetTraeCheckiner(f)

	s.RunTraeCheckinNow(context.Background())

	if len(f.checkinUIDs) != 1 || f.checkinUIDs[0] != "t1" {
		t.Fatalf("只应处理 Trae 账号，实际 %v", f.checkinUIDs)
	}
}

// TestRunTraeCheckinDeviceAlreadyDoesNotMarkDone 设备维度已签不标记账号已签。
func TestRunTraeCheckinDeviceAlreadyDoesNotMarkDone(t *testing.T) {
	p := pool.New("")
	p.Add(fakeTraeAccount{uid: "t1"})
	f := &fakeTraeCheckiner{result: &TraeCheckinResult{
		Already: true, DeviceAlready: true, Message: "该设备今日已签到",
	}}
	s := New(Config{Pool: p})
	s.SetTraeCheckiner(f)

	s.RunTraeCheckinNow(context.Background())

	if st, _ := p.Status("t1"); st.CheckinDone {
		t.Fatalf("device_already 不应标记账号已签: %+v", st)
	}
}

// TestRunTraeCheckinUnverifiedDoesNotMarkDone 回查未确认保留重试信号。
func TestRunTraeCheckinUnverifiedDoesNotMarkDone(t *testing.T) {
	p := pool.New("")
	p.Add(fakeTraeAccount{uid: "t1"})
	f := &fakeTraeCheckiner{result: &TraeCheckinResult{Unverified: true, Message: "未确认"}}
	s := New(Config{Pool: p})
	s.SetTraeCheckiner(f)

	s.RunTraeCheckinNow(context.Background())

	if st, _ := p.Status("t1"); st.CheckinDone {
		t.Fatalf("unverified 不应标记账号已签: %+v", st)
	}
}

// TestRunTraeCheckinErrorStillRefreshesCredits 签到失败也要刷新余额
// （余额是 credit_floor 判定与展示的唯一数据源）。
func TestRunTraeCheckinErrorStillRefreshesCredits(t *testing.T) {
	p := pool.New("")
	p.Add(fakeTraeAccount{uid: "t1"})
	f := &fakeTraeCheckiner{
		err:      errors.New("boom"),
		snapshot: pool.CreditsSnapshot{Remain: 42, Total: 100},
	}
	s := New(Config{Pool: p})
	s.SetTraeCheckiner(f)

	s.RunTraeCheckinNow(context.Background())

	if len(f.creditsUIDs) != 1 {
		t.Fatalf("签到失败后仍应查余额，实际 %v", f.creditsUIDs)
	}
	if st, _ := p.Status("t1"); st.Credits != 42 {
		t.Fatalf("余额应写回池: %d", st.Credits)
	}
}

// TestRunTraeCheckinNilAdapter 未注入适配器时不 panic、不影响 CodeBuddy。
func TestRunTraeCheckinNilAdapter(t *testing.T) {
	p := pool.New("")
	p.Add(fakeTraeAccount{uid: "t1"})
	s := New(Config{Pool: p})
	s.RunTraeCheckinNow(context.Background()) // 不应 panic
	if st, _ := p.Status("t1"); st.CheckinDone {
		t.Fatal("无适配器时不应标记已签")
	}
}

// TestRunCheckinNowInvokesTrae Trae 签到挂在 RunCheckinNow 末尾，
// 保证「全部签到」按钮与自动排程都覆盖两种账号。
func TestRunCheckinNowInvokesTrae(t *testing.T) {
	f := &fakeUpstream{resourceRemain: 500}
	srv := f.server()
	defer srv.Close()

	p := pool.New("")
	p.Add(fakeTraeAccount{uid: "t1"})
	tf := &fakeTraeCheckiner{}
	s := New(Config{Pool: p, Upstream: &upstream.Client{
		HTTP:          srv.Client(),
		ChatBaseCN:    srv.URL,
		BillingBaseCN: srv.URL,
	}})
	s.SetTraeCheckiner(tf)

	s.RunCheckinNow()

	if len(tf.checkinUIDs) != 1 || tf.checkinUIDs[0] != "t1" {
		t.Fatalf("RunCheckinNow 应覆盖 Trae 账号，实际 %v", tf.checkinUIDs)
	}
	if f.checkinCalls.Load() != 0 {
		t.Fatalf("Trae 账号不应触发 CodeBuddy 的 daily-checkin，实际 %d 次", f.checkinCalls.Load())
	}
}

// TestExpiringWithin 窗口内到期批次合计。
func TestExpiringWithin(t *testing.T) {
	now := time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)
	buckets := []pool.ExpiryBucket{
		{Date: "2026-11-15", ExpireAt: now.Add(24 * time.Hour), Remain: 100},
		{Date: "2026-11-20", ExpireAt: now.Add(6 * 24 * time.Hour), Remain: 200},
	}
	if got := expiringWithin(buckets, 48*time.Hour, now); got != 100 {
		t.Fatalf("48h 窗口内应只有 100，实际 %d", got)
	}
	if got := expiringWithin(buckets, 7*24*time.Hour, now); got != 300 {
		t.Fatalf("7 天窗口内应为 300，实际 %d", got)
	}
	if got := expiringWithin(buckets, 0, now); got != 0 {
		t.Fatalf("window<=0 应不设门槛，返回 0，实际 %d", got)
	}
}
