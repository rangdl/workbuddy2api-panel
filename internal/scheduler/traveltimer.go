// traveltimer.go 猫猫旅行「按到站时刻精确定时」的独立 goroutine。
//
// 为什么需要（线上实证 2026-11-12）：服务每天开关机，travel_hours=[9,21] 的两个
// 固定窗口几乎从不同时命中；而「派出 → 2h → 到站 → 领奖」需要跨两个窗口。
// 结果某账号到站奖励挂了 19 天无人领。固定 hours 排程是在赌「那个整点机器开着」，
// 而上游 travel/status 本来就返回了精确的 arrive_at —— 直接按它定闹钟即可。
//
// 与固定 hours 的关系：hours 继续作为兜底（timer 漏唤醒/机器长时间关机时仍有一趟），
// timer 负责主路径「到站即领、领完接着派出」。timer 不增加稳态上游请求
// （只在到点后跑一次 travelOne），其余时间纯 sleep。
package scheduler

import (
	"context"
	"log"
	"math/rand"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/logfmt"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// travelTimerParams 到站定时器的时间参数。
//
// 为什么挂在 Scheduler 而不是包级 var：定时器跑在独立 goroutine 里，包级 var 由
// 测试改写时会与运行中的 goroutine 形成数据竞争（-race 实测）。作为实例字段，
// 每个 Scheduler（每个测试）各持一份，互不影响。
type travelTimerParams struct {
	poll    time.Duration // 无在途行程时的兜底重查间隔
	step    time.Duration // 分段等待段长（墙钟重判，规避休眠冻结单调时钟）
	minWait time.Duration // 到站时刻距 now 的最小睡眠（吸收上游与本地秒级偏差）
	// jitterMax 到站后额外等待的上限（随机 0..jitterMax）。
	//
	// 为什么需要：上游的 arrive_at 是精确到秒的固定值，到点即领会让「到达时刻」
	// 与「领奖时刻」完全重合、每次都是 0 秒延迟——真人不会这样，规律性太强，
	// 容易被判成脚本。加一段随机延迟把领奖时刻打散。
	jitterMax time.Duration
	// jitter 随机源（测试可注入确定性实现；nil 时用 math/rand）。
	jitter func(time.Duration) time.Duration
}

// travelTimerDefaults 生产取值：
//   - poll 10min：所有账号 idle / 查询失败时没有 arrive_at 可等，周期重查既覆盖
//     「上游刚给出新行程」，也覆盖启动初期的临时失败；
//   - step 与 wallclockCheckStep 同口径；
//   - minWait 1s：到点瞬间查 status 可能仍返回 traveling（上游时间只到秒），
//     留 1s 缓冲避免白跑一趟。已过点的行程不受此限，立即触发；
//   - jitterMax 15min：到站后随机再等 0~15 分钟才领奖，避免「0 秒领取」的机器特征。
//     猫到站后奖励不会过期（实测挂 19 天仍可领），等待无风险。
var travelTimerDefaults = travelTimerParams{
	poll:      10 * time.Minute,
	step:      wallclockCheckStep,
	minWait:   time.Second,
	jitterMax: 15 * time.Minute,
}

// travelTimer 返回本调度器生效的时间参数（零值回落默认，New 构造的实例恒有值）。
func (s *Scheduler) travelTimer() travelTimerParams {
	p := s.travelTimerCfg
	if p.poll <= 0 {
		p.poll = travelTimerDefaults.poll
	}
	if p.step <= 0 {
		p.step = travelTimerDefaults.step
	}
	if p.minWait < 0 {
		p.minWait = travelTimerDefaults.minWait
	}
	if p.jitter == nil {
		p.jitter = randomJitter
	}
	if p.jitterMax < 0 {
		p.jitterMax = 0
	}
	return p
}

// randomJitter 返回 [0, max) 内的随机时长；max<=0 返回 0。
func randomJitter(max time.Duration) time.Duration {
	if max <= 0 {
		return 0
	}
	return time.Duration(rand.Int63n(int64(max)))
}

// travelArrival 一个账号的下一次到站时刻（本机墙钟口径）。
//
// arriveAt 保留上游原始 Unix 时刻，仅用于「这一趟是否已经触发过」的去重键：
// at 每轮按 ServerNow 重算会漂移，不能当身份用。
//
// immediate 标记「已到站待领」（启动补跑场景）：这类不需要再加抖动等待——
// 奖励已经等了不知道多久，再等只是无谓拖延；抖动只用于「在途 → 即将到站」
// 的正常闭环，那里才有「0 秒领取」的机器特征。
type travelArrival struct {
	uid       string
	nick      string
	at        time.Time
	arriveAt  int64
	immediate bool
}

// RunTravelTimer 到站定时器主循环，阻塞直到 ctx 取消。
//
// 每轮：
//  1. 遍历全池查 travel/status，收集所有在途账号的到站时刻；
//  2. 取最早的一个（同一账号本轮只跑一次），sleep 到点；
//  3. 到点只对那个账号跑 travelOne（领奖 → 重查 → 盲盒 → 再派出）；
//  4. 重算下一轮。
//
// 第一轮本身就是「启动补跑」：覆盖关机期间已到站未领的行程，无需额外 goroutine。
// 无在途行程时按 travelTimerPoll 周期重查（此时上游无新请求，纯空闲）。
//
// fired 记「哪一趟行程已经触发过」（uid → 上游 arrive_at）：上游若持续返回
// 状态未更新的在途行程（arrive_at 已过点却仍是 traveling），没有这份记录就会
// 以 travelTimerMinWait 的节奏无限重查——退回轮询间隔等它自愈。
func (s *Scheduler) RunTravelTimer(ctx context.Context) {
	fired := make(map[string]int64)
	tp := s.travelTimer()
	for {
		// 扫描与「到点执行」都与固定整点巡检/面板手动触发互斥（travelMu），
		// 但**等待期间不持锁**——定时器可能一睡两小时，持锁会让整点巡检全部饿死。
		s.travelMu.Lock()
		arrivals := filterUnfired(s.scanTravelArrivals(), fired)
		s.travelMu.Unlock()
		wait, target := travelTimerNext(arrivals, time.Now(), tp.minWait, tp.poll)
		// 到站抖动：只在「等一趟真实在途行程」时加，兜底轮询（target==nil）不加。
		// 目的见 travelTimerParams.jitterMax——把「到达即领」的 0 秒延迟打散。
		if target != nil && !target.immediate && tp.jitterMax > 0 {
			j := tp.jitter(tp.jitterMax)
			if j > 0 {
				wait += j
				log.Printf("travel-timer %s: 到站后再等 %s（随机抖动，避免固定时刻领奖）",
					logfmt.Label(target.uid, target.nick), roundDuration(j))
			}
		}
		switch {
		case target == nil:
			log.Printf("travel-timer: 无可等待行程，%s 后重查", tp.poll)
		case wait <= tp.minWait:
			// arrived（启动补跑）或已过点的行程：立即执行，不喊「还剩 0s」。
			log.Printf("travel-timer %s: 有到站奖励待领，立即唤醒", logfmt.Label(target.uid, target.nick))
		default:
			log.Printf("travel-timer %s: 到站 %s，定时唤醒（还剩 %s）",
				logfmt.Label(target.uid, target.nick), target.at.In(cstZone).Format("2006-01-02 15:04:05"), roundDuration(wait))
		}
		switch s.waitSlotChan(ctx, time.Now().Add(wait), tp.step, s.rearmTravel) {
		case slotCancel:
			return
		case slotRearm:
			continue // 手动触发/配置变更：重查状态，按最新 arrive_at 重算
		case slotFired:
			if target == nil {
				continue // 兜底轮询到期：回到循环顶重扫
			}
			fired[target.uid] = target.arriveAt
			if a := s.cfg.Pool.AuthByUID(target.uid); a != nil {
				log.Printf("travel-timer %s: 唤醒，执行 travelOne", logfmt.Label(a.UID, a.NicknameValue()))
				s.travelMu.Lock()
				s.travelOne(a)
				s.travelMu.Unlock()
			} else {
				log.Printf("travel-timer %s: 唤醒时账号已不在池中，跳过", logfmt.Label(target.uid, target.nick))
			}
			// 唤醒后这一趟行程不再参与定时（去重），若上游仍报同一趟在途，
			// 下一轮扫描会看到它但被 filterUnfired 过滤，退回兜底轮询等它自愈。
			// 补一行让「为什么没继续等」在日志里可解释。
			log.Printf("travel-timer %s: 本趟已触发，等待上游刷新状态（兜底 %s 后重扫）",
				logfmt.Label(target.uid, target.nick), tp.poll)
		}
	}
}

// filterUnfired 去掉已经触发过的同一趟行程，并清理已消失行程的去重记录
// （账号数有限，且记录随行程结束收敛，不会无限增长）。
func filterUnfired(arrivals []travelArrival, fired map[string]int64) []travelArrival {
	seen := make(map[string]int64, len(arrivals))
	kept := arrivals[:0]
	for _, ar := range arrivals {
		seen[ar.uid] = ar.arriveAt
		if k, ok := fired[ar.uid]; ok && k == ar.arriveAt {
			continue
		}
		kept = append(kept, ar)
	}
	for uid := range fired {
		if _, ok := seen[uid]; !ok {
			delete(fired, uid)
		}
	}
	return kept
}

// scanTravelArrivals 查询全池行程的待处理时刻（在途 → 到站时刻；已到站 → 立即）。
//
// 只读：每号一次 travel/status。账号间限速沿用 travelAccountDelay（与批次巡检同
// 口径，避免定时器与手动巡检撞在一起放大风控面）。
// 调用方需持 travelMu（本函数不自行加锁：RunTravelTimer 只在空闲扫描时调用，
// 由主循环统一持锁可让「扫描 + 到点执行」成为同一临界区，避免扫描与执行交错）。
func (s *Scheduler) scanTravelArrivals() []travelArrival {
	var out []travelArrival
	first := true
	for _, a := range s.travelAccounts() {
		if !first {
			time.Sleep(travelAccountDelay)
		}
		first = false
		ts, err := s.cfg.Upstream.TravelStatus(a)
		if err != nil {
			// 401/网络错误只跳过本轮（不强刷 token，交 22:00 keepalive）。
			log.Printf("travel-timer %s: status: %v", logfmt.Label(a.UID, a.NicknameValue()), err)
			continue
		}
		if at, key, immediate, ok := travelDueTime(ts, time.Now()); ok {
			out = append(out, travelArrival{
				uid: a.UID, nick: a.NicknameValue(),
				at: at, arriveAt: key, immediate: immediate,
			})
		}
	}
	return out
}

// travelDueTime 计算该账号下一次需要唤醒的时刻：
//
//   - traveling + arrive_at：按上游口径换算到站时刻（ArriveAt-ServerNow，免疫本机
//     时钟偏差）；immediate=false，调用方会附加随机抖动；
//   - arrived：**立即**（at=now）——这是启动补跑的核心场景：服务关机期间猫已到站，
//     重启后必须马上领奖，否则要等到下一个 travel_hours 整点，而线上实证里
//     [9,21] 窗口常年错过，奖励挂了 19 天。immediate=true，跳过抖动（已经等够久）。
//   - 其余（idle / 未知 / 缺时间）：不参与定时。
//
// 去重键 key：traveling 用上游 arrive_at（同一趟行程身份稳定）；arrived 用
// RecordID（同一笔待领奖励身份稳定）。用「每轮重算的 at」当键会漂移，不行。
func travelDueTime(ts *upstream.TravelState, now time.Time) (at time.Time, key int64, immediate bool, ok bool) {
	if ts == nil {
		return time.Time{}, 0, false, false
	}
	switch ts.State {
	case travelStateArrived:
		if ts.RecordID == 0 {
			return time.Time{}, 0, false, false // 缺 record_id 无法领奖，交给整点巡检记日志
		}
		return now, ts.RecordID, true, true
	case travelStateTraveling:
		if ts.ArriveAt <= 0 {
			return time.Time{}, 0, false, false
		}
		remain := ts.ArriveAt - now.Unix() // 上游未回 server_now 时退回本机时钟
		if ts.ServerNow > 0 {
			remain = ts.ArriveAt - ts.ServerNow
		}
		if remain < 0 {
			remain = 0 // 已过点：立即唤醒（travelOne 按真实状态决定领奖/跳过）
		}
		return now.Add(time.Duration(remain) * time.Second), ts.ArriveAt, false, true
	default:
		return time.Time{}, 0, false, false
	}
}

// travelTimerNext 取最早到站时刻与对应账号；无候选时返回兜底轮询间隔。
// wait 恒为正：到点账号留 minWait 缓冲，避免与上游秒级偏差白跑。
//
// poll 由调用方传入（而非在此读 travelTimerDefaults）：Scheduler 的时间参数
// 可被测试覆盖，硬编码默认值会让「无候选」这条路径绕过覆盖，与日志里打印的
// tp.poll 不一致。
func travelTimerNext(arrivals []travelArrival, now time.Time, minWait, poll time.Duration) (time.Duration, *travelArrival) {
	if len(arrivals) == 0 {
		return poll, nil
	}
	best := 0
	for i := 1; i < len(arrivals); i++ {
		if arrivals[i].at.Before(arrivals[best].at) {
			best = i
		}
	}
	wait := arrivals[best].at.Sub(now)
	if wait < minWait {
		wait = minWait
	}
	return wait, &arrivals[best]
}

// roundDuration 日志用的粗粒度时长（四舍五入到秒，避免 1h57m59.998s 这类噪声）。
func roundDuration(d time.Duration) time.Duration {
	if d < 0 {
		return 0
	}
	return d.Round(time.Second)
}
