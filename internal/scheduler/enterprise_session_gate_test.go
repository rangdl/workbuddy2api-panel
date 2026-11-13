package scheduler

import (
	"encoding/base64"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// personalWithSwitchClaimScheduler 构造「个人号但 JWT 带 token_source=enterprise_switch」的
// 调度器。enterpriseId 为空，属正常个人账号——该章只记录本次授权走了哪条流程。
//
// 这正是线上真实形态：个人号从企业切换入口登录后，refresh 出的 token 会长期带该章。
// 历史 bug：旧 IsEnterpriseSession 据此把这类账号判成企业号，四条任务链路全线跳过。
func personalWithSwitchClaimScheduler(t *testing.T) (*Scheduler, *recorderUpstream) {
	t.Helper()
	r := &recorderUpstream{}
	srv := r.server()
	t.Cleanup(srv.Close)

	payload := base64.RawURLEncoding.EncodeToString([]byte(`{"sub":"u1","token_source":"enterprise_switch"}`))
	a := &auth.Auth{
		UID:          "u1",
		AccessToken:  "hdr." + payload + ".sig",
		RefreshToken: "rt",
		ExpiresAt:    9999999999,
		EnterpriseID: "", // 关键：个人号
	}
	p := pool.New("")
	p.Add(pool.WrapAuth(a))

	up := &upstream.Client{
		HTTP:          srv.Client(),
		ChatBaseCN:    srv.URL,
		BillingBaseCN: srv.URL,
		WebBaseCN:     srv.URL,
	}
	return New(Config{Pool: p, Upstream: up}), r
}

// enterpriseAccountScheduler 构造真企业版（enterpriseId 非空）的调度器。
func enterpriseAccountScheduler(t *testing.T) (*Scheduler, *recorderUpstream) {
	t.Helper()
	r := &recorderUpstream{}
	srv := r.server()
	t.Cleanup(srv.Close)

	a := &auth.Auth{
		UID:          "u1",
		AccessToken:  "at",
		RefreshToken: "rt",
		ExpiresAt:    9999999999,
		EnterpriseID: "e1", // 真企业版
	}
	p := pool.New("")
	p.Add(pool.WrapAuth(a))

	up := &upstream.Client{
		HTTP:          srv.Client(),
		ChatBaseCN:    srv.URL,
		BillingBaseCN: srv.URL,
		WebBaseCN:     srv.URL,
	}
	return New(Config{Pool: p, Upstream: up}), r
}

// TestPersonalAccountWithSwitchClaimRunsAllTasks 个人号带 enterprise_switch 章时，
// 所有个人成长任务必须照常发起（回归）。
//
// 背景（2026-11-13）：旧 IsEnterpriseSession 把该章当作企业上下文，导致
// 活跃上报 / 连登管家 / 夜猫子 / 旅行四条链路对正常个人号全线跳过。线上表现为
// 服务运行一段时间后（refresh 继承该章）任务突然全停、盲盒按钮返回 0 个账号。
//
// 实测依据：该账号 email / enterpriseId / realm_access 全是个人号特征；
// 下游 fork 亦独立注释「不要再用 token_source=enterprise_switch 判定企业上下文，
// 企业形态一律以 enterpriseId 为准」。
func TestPersonalAccountWithSwitchClaimRunsAllTasks(t *testing.T) {
	restoreClock := nowForNightWindow
	nowForNightWindow = func() time.Time { return time.Date(2026, 10, 12, 23, 30, 0, 0, time.Local) }
	t.Cleanup(func() { nowForNightWindow = restoreClock })
	fastActivity(t) // 关闭账号间限速与 streak 回读等待（否则每例白等 8s）
	fastTravel(t)

	cases := []struct {
		name     string
		run      func(*Scheduler)
		endpoint string
	}{
		{"签到", func(s *Scheduler) { s.RunCheckinNow() }, "/daily-checkin"},
		{"活跃上报", func(s *Scheduler) { s.RunActivityNow() }, "/v2/report"},
		{"猫猫旅行", func(s *Scheduler) { s.RunTravelNow() }, "/activity/growth/buddy/info"},
		{"夜猫子", func(s *Scheduler) { s.RunBlackcatNow() }, "/v2/activity/growth/tasks"},
		{"连登管家", func(s *Scheduler) { s.RunStreakBonusNow() }, "/activity/growth/streak"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s, r := personalWithSwitchClaimScheduler(t)
			c.run(s)
			if !r.has(c.endpoint) {
				t.Errorf("个人号（带 enterprise_switch 章）应照常发起 %s，实际请求=%v", c.endpoint, r.snapshot())
			}
		})
	}
}

// TestEnterpriseAccountSkipsGrowthTasks 真企业版（enterpriseId 非空）仍然全部跳过：
// 上游 growth 端点一律 403，签到 / 领奖返回 400 code 10001，执行即零收益。
func TestEnterpriseAccountSkipsGrowthTasks(t *testing.T) {
	restoreClock := nowForNightWindow
	nowForNightWindow = func() time.Time { return time.Date(2026, 10, 12, 23, 30, 0, 0, time.Local) }
	t.Cleanup(func() { nowForNightWindow = restoreClock })
	fastActivity(t) // 关闭账号间限速与 streak 回读等待
	fastTravel(t)

	cases := []struct {
		name     string
		run      func(*Scheduler)
		endpoint string
	}{
		{"活跃上报", func(s *Scheduler) { s.RunActivityNow() }, "/v2/report"},
		{"猫猫旅行", func(s *Scheduler) { s.RunTravelNow() }, "/activity/growth/buddy/info"},
		{"夜猫子", func(s *Scheduler) { s.RunBlackcatNow() }, "/v2/activity/growth/tasks"},
		{"连登管家", func(s *Scheduler) { s.RunStreakBonusNow() }, "/activity/growth/streak"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s, r := enterpriseAccountScheduler(t)
			c.run(s)
			if r.has(c.endpoint) {
				t.Errorf("企业版不应发起 %s，实际请求=%v", c.endpoint, r.snapshot())
			}
		})
	}
}

// TestEnterpriseAccountKeepsKeepalive 企业版照常保活（token 会过期，企业形态
// 不影响选号/刷新能力）。
func TestEnterpriseAccountKeepsKeepalive(t *testing.T) {
	s, r := enterpriseAccountScheduler(t)
	s.RunKeepaliveNow()
	if !r.has("/v2/plugin/auth/token/refresh") {
		t.Errorf("企业版必须照常保活，实际请求=%v", r.snapshot())
	}
}
