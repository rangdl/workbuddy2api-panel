package trae

import (
	"context"
	"log"
	"sync"
	"time"
)

// scheduler.go Trae 签到调度器。
//
// 为什么独立而不接入 internal/scheduler：那个包是 CodeBuddy 账号池专用
// （且是上游活跃文件，两轮上游同步均被触及），把 Trae 塞进去会持续产生合并成本。
// Trae 的调度需求很简单——每天固定时点跑一轮签到——独立 ticker 足够，
// 也保持了「Trae 链路整体独立」的设计一致性。

// CheckinSchedule 签到排程配置。
type CheckinSchedule struct {
	// Enabled 是否启用自动签到（缺省 false：需用户显式开启）。
	Enabled bool `json:"checkin_enabled"`
	// Hours 每日执行的整点（缺省 [9]）。
	Hours []int `json:"checkin_hours"`
	// GapMS 账号间间隔（毫秒；缺省 800，防连打触发风控）。
	GapMS int `json:"checkin_gap_ms"`
}

// CheckinOutcome 单账号签到结果（面板与日志共用）。
type CheckinOutcome struct {
	UID  string `json:"uid"`
	Name string `json:"name"`
	// Status success / already / fail / skipped。
	Status string `json:"status"`
	// Reward 本次奖励积分。
	Reward int64 `json:"reward"`
	// Message 可读原因（上游原文优先）。
	Message string `json:"message"`
	// ErrorType 失败时的分类（供面板展示）。
	ErrorType string `json:"error_type,omitempty"`
	// Elapsed 单账号耗时（秒）。
	Elapsed float64 `json:"elapsed"`
}

// Scheduler Trae 签到调度器。
type Scheduler struct {
	// ConfigFn 返回当前生效配置（每次 tick 重新读取，支持热重载）。
	ConfigFn func() *Config
	Client   *Client
	// Cooldowns 账号冷却表（跳过冷却中账号；签到失败也写回）。
	Cooldowns *Cooldowns

	mu sync.Mutex
	// lastRunDay 上次执行日期（本地时区 "2006-01-02"），防同日重复执行。
	lastRunDay string
	// lastOutcomes 最近一次执行结果（面板展示）。
	lastOutcomes []CheckinOutcome
	lastRunAt    time.Time
}

// NewScheduler 构造调度器。
func NewScheduler(configFn func() *Config, client *Client, cool *Cooldowns) *Scheduler {
	return &Scheduler{ConfigFn: configFn, Client: client, Cooldowns: cool}
}

// Run 主循环：每分钟检查一次是否到点（时点粒度是小时，分钟级轮询足够且实现简单）。
func (s *Scheduler) Run(ctx context.Context) {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			s.tick(ctx, now)
		}
	}
}

// tick 判定并触发一轮签到。
func (s *Scheduler) tick(ctx context.Context, now time.Time) {
	cfg := s.ConfigFn()
	if cfg == nil || !cfg.Enabled || !cfg.Schedule.Enabled {
		return
	}
	if !hourMatches(cfg.Schedule.Hours, now.Hour()) {
		return
	}
	day := now.Format("2006-01-02")
	s.mu.Lock()
	if s.lastRunDay == day {
		s.mu.Unlock()
		return // 当天该时点已跑过
	}
	s.lastRunDay = day
	s.mu.Unlock()

	log.Printf("[trae] 自动签到开始（%s %02d 点）", day, now.Hour())
	outcomes := s.RunOnce(ctx)
	ok, already, failed := 0, 0, 0
	for _, o := range outcomes {
		switch o.Status {
		case "success":
			ok++
		case "already":
			already++
		case "fail":
			failed++
		}
	}
	log.Printf("[trae] 自动签到结束：成功 %d，已签 %d，失败 %d", ok, already, failed)
}

// RunOnce 立即执行一轮签到（面板手动触发也走这里）。
//
// 返回每个账号的结果；账号间按配置间隔限速（默认 800ms）。
//
// 语义：**不检查 cfg.Enabled 与排程开关**——手动触发是用户的显式意图，
// 允许"先登录账号、手动签到验证，再决定是否启用 /trae/v1/*"。
// 自动排程的开关判定在 tick 里（那里才需要 Enabled + Schedule.Enabled 双闸）。
func (s *Scheduler) RunOnce(ctx context.Context) []CheckinOutcome {
	cfg := s.ConfigFn()
	if cfg == nil || s.Client == nil {
		return nil
	}
	gap := time.Duration(cfg.Schedule.GapMS) * time.Millisecond
	if gap <= 0 {
		gap = 800 * time.Millisecond
	}
	outcomes := make([]CheckinOutcome, 0, len(cfg.Accounts))
	for i := range cfg.Accounts {
		acc := cfg.Accounts[i]
		if acc.Disabled || acc.AccessToken == "" {
			continue
		}
		if s.Cooldowns != nil && !s.Cooldowns.Available(acc.UID) {
			outcomes = append(outcomes, CheckinOutcome{
				UID: acc.UID, Name: acc.Name, Status: "skipped", Message: "账号冷却中",
			})
			continue
		}
		if ctx.Err() != nil {
			break // 优雅停机：不再开新一轮
		}
		outcomes = append(outcomes, s.checkinOne(ctx, cfg, &acc))
		if i < len(cfg.Accounts)-1 {
			select {
			case <-ctx.Done():
			case <-time.After(gap):
			}
		}
	}
	s.mu.Lock()
	s.lastOutcomes = outcomes
	s.lastRunAt = time.Now()
	s.mu.Unlock()
	return outcomes
}

// checkinOne 单账号签到（含错误分类与冷却回写）。
func (s *Scheduler) checkinOne(ctx context.Context, cfg *Config, acc *Account) CheckinOutcome {
	start := time.Now()
	out := CheckinOutcome{UID: acc.UID, Name: acc.Name, Status: "fail"}
	res, err := s.Client.Checkin(ctx, cfg, acc)
	out.Elapsed = time.Since(start).Seconds()
	if err != nil {
		cls := Classification{Kind: ErrNone}
		if apiErr, ok := err.(*APIError); ok {
			cls = apiErr.Classification()
		}
		out.ErrorType = string(cls.Kind)
		out.Message = orDefault(cls.Message, err.Error())
		if s.Cooldowns != nil && cls.Kind != ErrNone {
			if cls.Disable {
				s.Cooldowns.Mark(acc.UID, 24*time.Hour, cls.Kind, out.Message)
			} else if cls.Cooldown > 0 {
				s.Cooldowns.Mark(acc.UID, cls.Cooldown, cls.Kind, out.Message)
			}
		}
		log.Printf("WARN: [trae] 签到失败 %s: %s", acc.UID, out.Message)
		return out
	}
	if res.Already {
		out.Status = "already"
		out.Message = res.Message
		return out
	}
	out.Status = "success"
	out.Reward = res.Reward
	out.Message = res.Message
	if s.Cooldowns != nil {
		s.Cooldowns.Clear(acc.UID) // 签到成功即清冷却（账号已恢复）
	}
	log.Printf("[trae] 签到成功 %s: +%d（%s）", acc.UID, out.Reward, out.Message)
	return out
}

// LastOutcomes 返回最近一次执行结果与时刻（面板展示用）。
func (s *Scheduler) LastOutcomes() ([]CheckinOutcome, time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]CheckinOutcome, len(s.lastOutcomes))
	copy(out, s.lastOutcomes)
	return out, s.lastRunAt
}

// hourMatches hours 是否包含 h（空列表视为未配置 → 不匹配）。
func hourMatches(hours []int, h int) bool {
	for _, v := range hours {
		if v == h {
			return true
		}
	}
	return false
}
