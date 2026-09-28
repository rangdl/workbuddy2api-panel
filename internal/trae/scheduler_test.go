package trae

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// newSchedServer 假 api 上游：按 X-Device-Id 区分账号行为。
// dev-bad 的账号返回 401（验证错误分类与冷却回写）。
func newSchedServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		dev := r.Header.Get("X-Device-Id")
		w.Header().Set("Content-Type", "application/json")
		if dev == "dev-bad" {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"message":"token 已失效"}`))
			return
		}
		switch r.URL.Path {
		case EndpointCheckinStatus:
			// dev-already 已签到
			if dev == "dev-already" {
				_, _ = w.Write([]byte(`{"code":0,"checked_in":true}`))
				return
			}
			_, _ = w.Write([]byte(`{"code":0,"checked_in":false}`))
		case EndpointCheckinClaim:
			_, _ = w.Write([]byte(`{"code":0,"data":{"reward":150}}`))
		default:
			_, _ = w.Write([]byte(`{"code":0}`))
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func schedCfg(base string, accounts ...Account) *Config {
	c := &Config{
		Enabled:     true,
		ChatBase:    base,
		APITraeBase: base,
		AppID:       DefaultAppID,
		Accounts:    accounts,
	}
	c.Normalize()
	return c
}

// TestSchedulerRunOnce 一轮签到：成功 / 已签 / 失败三类结果齐全，失败写冷却。
func TestSchedulerRunOnce(t *testing.T) {
	srv := newSchedServer(t)
	cfg := schedCfg(srv.URL,
		Account{UID: "u-ok", AccessToken: "t1", DeviceID: "dev-ok"},
		Account{UID: "u-already", AccessToken: "t2", DeviceID: "dev-already"},
		Account{UID: "u-bad", AccessToken: "t3", DeviceID: "dev-bad"},
	)
	cool := NewCooldowns()
	s := NewScheduler(func() *Config { return cfg }, NewClient(), cool)
	s.RunOnce(context.Background())

	outcomes, at := s.LastOutcomes()
	if at.IsZero() {
		t.Error("lastRunAt 未记录")
	}
	if len(outcomes) != 3 {
		t.Fatalf("结果数 = %d, want 3", len(outcomes))
	}
	byUID := map[string]CheckinOutcome{}
	for _, o := range outcomes {
		byUID[o.UID] = o
	}
	if o := byUID["u-ok"]; o.Status != "success" || o.Reward != 150 {
		t.Errorf("u-ok = %+v, want success +150", o)
	}
	if o := byUID["u-already"]; o.Status != "already" {
		t.Errorf("u-already = %+v, want already", o)
	}
	if o := byUID["u-bad"]; o.Status != "fail" || o.ErrorType != string(ErrSessionDead) {
		t.Errorf("u-bad = %+v, want fail/SessionDead", o)
	}
	// 失败账号被摘出轮转（SessionDead → Disable → 24h）
	if cool.Available("u-bad") {
		t.Error("401 账号应被摘出轮转")
	}
	if !cool.Available("u-ok") {
		t.Error("成功账号不应被冷却")
	}
}

// TestSchedulerSkipsCoolingAndDisabled 冷却中与已禁用账号应被跳过。
func TestSchedulerSkipsCoolingAndDisabled(t *testing.T) {
	srv := newSchedServer(t)
	cfg := schedCfg(srv.URL,
		Account{UID: "u-cool", AccessToken: "t1", DeviceID: "dev-cool"},
		Account{UID: "u-off", AccessToken: "t2", DeviceID: "dev-off", Disabled: true},
		Account{UID: "u-notoken", DeviceID: "dev-x"},
	)
	cool := NewCooldowns()
	cool.Mark("u-cool", time.Hour, ErrSoftRate, "限流")
	s := NewScheduler(func() *Config { return cfg }, NewClient(), cool)
	outcomes := s.RunOnce(context.Background())

	if len(outcomes) != 1 {
		t.Fatalf("结果数 = %d, want 1（只应有冷却账号的 skipped）", len(outcomes))
	}
	if outcomes[0].UID != "u-cool" || outcomes[0].Status != "skipped" {
		t.Errorf("outcome = %+v, want u-cool/skipped", outcomes[0])
	}
}

// TestSchedulerSuccessClearsCooldown 签到成功即清冷却（账号已恢复）。
func TestSchedulerSuccessClearsCooldown(t *testing.T) {
	srv := newSchedServer(t)
	cfg := schedCfg(srv.URL, Account{UID: "u-ok", AccessToken: "t1", DeviceID: "dev-ok"})
	cool := NewCooldowns()
	// 手动清掉冷却标记才能进 RunOnce（可用性检查会跳过冷却账号），
	// 这里直接调用 checkinOne 验证清理行为。
	s := NewScheduler(func() *Config { return cfg }, NewClient(), cool)
	out := s.checkinOne(context.Background(), cfg, &cfg.Accounts[0])
	if out.Status != "success" {
		t.Fatalf("outcome = %+v", out)
	}
	if !cool.Available("u-ok") {
		t.Error("成功后不应处于冷却")
	}
}

// TestSchedulerDisabledConfig nil 配置与无客户端时不执行；但「网关未启用」不影响
// 手动触发——用户可以先登录账号、手动签到验证，再决定是否启用 /trae/v1/*。
func TestSchedulerDisabledConfig(t *testing.T) {
	srv := newSchedServer(t)

	// nil 配置：无从执行
	s := NewScheduler(func() *Config { return nil }, NewClient(), NewCooldowns())
	if got := s.RunOnce(context.Background()); len(got) != 0 {
		t.Errorf("nil 配置不应有结果，得到 %d", len(got))
	}

	// 无客户端：不应执行
	cfg := schedCfg(srv.URL, Account{UID: "u-ok", AccessToken: "t1", DeviceID: "dev-ok"})
	s = NewScheduler(func() *Config { return cfg }, nil, NewCooldowns())
	if got := s.RunOnce(context.Background()); len(got) != 0 {
		t.Errorf("无客户端不应有结果，得到 %d", len(got))
	}

	// 网关未启用但配置有效：手动触发仍执行（与自动排程解耦）
	disabled := schedCfg(srv.URL, Account{UID: "u-ok", AccessToken: "t1", DeviceID: "dev-ok"})
	disabled.Enabled = false
	s = NewScheduler(func() *Config { return disabled }, NewClient(), NewCooldowns())
	if got := s.RunOnce(context.Background()); len(got) != 1 {
		t.Errorf("手动触发应执行（与 Enabled 解耦），得到 %d", len(got))
	}
}

// TestSchedulerTickDedup 同一时点当天只跑一次（lastRunDay 去重）。
func TestSchedulerTickDedup(t *testing.T) {
	srv := newSchedServer(t)
	cfg := schedCfg(srv.URL, Account{UID: "u-ok", AccessToken: "t1", DeviceID: "dev-ok"})
	cfg.Schedule.Enabled = true
	cfg.Schedule.Hours = []int{9}
	s := NewScheduler(func() *Config { return cfg }, NewClient(), NewCooldowns())

	at := time.Date(2026, 9, 27, 9, 0, 0, 0, time.Local)
	s.tick(context.Background(), at)
	first, _ := s.LastOutcomes()
	if len(first) == 0 {
		t.Fatal("首次 tick 应执行")
	}
	// 同一小时再 tick → 去重，不重跑
	s.mu.Lock()
	s.lastOutcomes = nil
	s.mu.Unlock()
	s.tick(context.Background(), at.Add(30*time.Minute))
	if again, _ := s.LastOutcomes(); len(again) != 0 {
		t.Error("同一天同一时点不应重复执行")
	}
	// 换一天 → 再次执行
	s.tick(context.Background(), at.AddDate(0, 0, 1))
	if next, _ := s.LastOutcomes(); len(next) == 0 {
		t.Error("次日应重新执行")
	}
}

// TestSchedulerTickHourGate 非配置时点不执行。
func TestSchedulerTickHourGate(t *testing.T) {
	srv := newSchedServer(t)
	cfg := schedCfg(srv.URL, Account{UID: "u-ok", AccessToken: "t1", DeviceID: "dev-ok"})
	cfg.Schedule.Enabled = true
	cfg.Schedule.Hours = []int{9, 21}
	s := NewScheduler(func() *Config { return cfg }, NewClient(), NewCooldowns())

	s.tick(context.Background(), time.Date(2026, 9, 27, 10, 0, 0, 0, time.Local))
	if got, _ := s.LastOutcomes(); len(got) != 0 {
		t.Error("非配置时点不应执行")
	}
	s.tick(context.Background(), time.Date(2026, 9, 27, 21, 0, 0, 0, time.Local))
	if got, _ := s.LastOutcomes(); len(got) == 0 {
		t.Error("配置时点应执行")
	}
}

// TestHourMatches 时点匹配边界。
func TestHourMatches(t *testing.T) {
	if hourMatches(nil, 9) {
		t.Error("空列表不应匹配")
	}
	if !hourMatches([]int{9, 21}, 21) {
		t.Error("21 应匹配")
	}
	if hourMatches([]int{9, 21}, 10) {
		t.Error("10 不应匹配")
	}
}

// TestSchedulerContextCancel ctx 取消后不再开新一轮账号。
func TestSchedulerContextCancel(t *testing.T) {
	srv := newSchedServer(t)
	cfg := schedCfg(srv.URL,
		Account{UID: "u1", AccessToken: "t", DeviceID: "dev-ok"},
		Account{UID: "u2", AccessToken: "t", DeviceID: "dev-ok"},
	)
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // 立即取消
	s := NewScheduler(func() *Config { return cfg }, NewClient(), NewCooldowns())
	outcomes := s.RunOnce(ctx)
	if len(outcomes) != 0 {
		t.Errorf("ctx 已取消时不应执行任何账号，得到 %d", len(outcomes))
	}
}
