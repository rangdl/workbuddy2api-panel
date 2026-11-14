// travel.go 猫猫旅行巡检的排程入口。
//
// 状态机已搬到账号对象上（backend/codebuddy/features.go 的 Account.Travel）：
// 池遍历所有实现了 gateway.Traveler 的账号逐个调用，账号自己完成
// 「领养 / 派出 / 到站领奖 / 盲盒」的全部分支与门控（企业版 / global / 无凭证）。
//
// 本文件只保留调度器的职责：排程时点、并发互斥、以及到站定时器的唤醒信号。
package scheduler

import (
	"context"
	"log"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/gateway"
	"github.com/linguo2625469/workbuddy2api-panel/internal/logfmt"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
)

// blindboxDefaultMaxPerRun 面板不指定时的开盒次数上限。
const blindboxDefaultMaxPerRun = 5

// travelAccountDelay 账号间限速（防风控）。测试可置 0。
//
// 实际的等待发生在 pool 的批量调度层；这里每次巡检前把它同步给 pool，
// 让既有测试仍能通过改写本变量关掉等待。
var travelAccountDelay = 800 * time.Millisecond

// RunTravelNow 立即对池内所有适用账号执行一趟旅行巡检。
//
// paused 账号照常参与（旅行是纯 RPC，不发模型对话）；
// 末尾 poke 到站定时器：本趟可能派出了新行程，定时器需要按新的 arrive_at 重算。
func (s *Scheduler) RunTravelNow() {
	s.travelMu.Lock()
	defer s.travelMu.Unlock()
	pool.SetCapabilityDelay(travelAccountDelay)
	enabled, maxPerRun := s.travelBlindboxCfg()
	s.cfg.Pool.TravelAllWith(context.Background(), gateway.TravelOptions{
		BlindboxEnabled:   enabled,
		BlindboxMaxPerRun: maxPerRun,
	})
	// 无条件 poke：本趟可能派出了新行程，定时器需要按新的 arrive_at 重算。
	// 没派出时这次 poke 只让定时器重扫一遍状态，代价极小。
	s.PokeTravelTimer()
}

// RunBlindboxNow 手动触发一轮能量盲盒（面板按钮）。
//
// apply=false 为 dry-run：只查状态 + quota 并给出决策，不消耗任何能量。
// 同步返回逐账号结果——面板要直接展示「谁、为什么能开/不能开、开了几次」。
func (s *Scheduler) RunBlindboxNow(apply bool, maxPerRun int) []pool.BlindboxResult {
	if maxPerRun <= 0 {
		maxPerRun = blindboxDefaultMaxPerRun
	}
	s.travelMu.Lock()
	defer s.travelMu.Unlock()
	pool.SetCapabilityDelay(travelAccountDelay)
	results := s.cfg.Pool.BlindboxAll(context.Background(), apply, maxPerRun)
	log.Printf("blindbox: 巡检 %d 个账号（apply=%v max=%d）", len(results), apply, maxPerRun)
	return results
}

// travelOneByUID 对单个账号跑一趟旅行巡检（到站定时器用）。
//
// 定时器是「某账号的行程到站了」的精确唤醒，不走全池巡检——那会让每个到站
// 事件都触发一次全量上游调用。调用方需持 travelMu。
func (s *Scheduler) travelOneByUID(uid string) {
	acc := s.cfg.Pool.Account(uid)
	if acc == nil {
		log.Printf("travel-timer %s: 唤醒时账号已不在池中，跳过", uid)
		return
	}
	t, ok := acc.(gateway.Traveler)
	if !ok {
		return
	}
	log.Printf("travel-timer %s: 唤醒，执行巡检", logfmt.Label(uid, acc.Nickname()))
	outcome, err := t.Travel(context.Background(), gateway.TravelOptions{})
	if err != nil {
		log.Printf("travel-timer %s: %v", uid, err)
		return
	}
	// 派出成功 → 通知到站定时器去取这一趟的 arrive_at 并定时唤醒。
	// 事件驱动模型下这是最主要的唤醒源（没有周期轮询兜底）。
	// 状态机已搬到账号内部，所以「是否需要重算」由动作结果判定。
	if outcome.Action == "departed" {
		s.PokeTravelTimer()
	}
}

// travelTimerAccounts 返回到站扫描所需的账号集合。
//
// 只挑实现了 Traveler 的账号：只有它们才可能有行程，因此不需要判断上游类型。
// 含 paused——暂停选号不停止已有行程，到站奖励照常要领。
func (s *Scheduler) travelTimerAccounts() []travelTimerAccount {
	var out []travelTimerAccount
	for _, m := range s.cfg.Pool.Members(pool.MemberFilter{IncludePaused: true}) {
		t, ok := m.Acc.(gateway.Traveler)
		if !ok {
			continue
		}
		out = append(out, travelTimerAccount{
			uid:  m.Acc.UID(),
			nick: m.Acc.Nickname(),
			t:    t,
		})
	}
	return out
}

// travelTimerAccount 到站扫描所需的最小组合：身份 + 只读状态查询能力。
type travelTimerAccount struct {
	uid  string
	nick string
	t    gateway.Traveler
}
