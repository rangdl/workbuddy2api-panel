// wafip.go 短窗多账号命中判定器（WAF IP 级拦截 / 上游平台级账号受限 共用）。
//
// 两个场景的**形态完全相同**：单个账号出问题属于账号级（冷却/禁用了事），
// 但「短时间内多个**不同**账号接连出同一类问题」说明问题不在账号，而在
// 更外层——继续轮转只会把请求放大 MaxRotate 倍去撞同一堵墙，还加速扩散。
// 所以共用一台状态机，只换窗口、阈值与文案。
//
// ── 场景 A：WAF 403（IP 级拦截）──
// 背景（fork-scan-absorb T-1 / BulidH 实测）：WAF 403 拦的是网关出口 IP 而非
// 账号——3 个账号 1 秒内全 403。既有 ErrWafBlock 账号级软冷却在 IP 级拦截时
// 不够：轮转会把一次客户端请求放大 MaxRotate 倍，同一出口 IP 继续打上游只会
// 加重风控。窗口 60s、阈值 2。
//
// ── 场景 B：11140 账号受限（平台级封号 wave）──
// 背景（2026-11 社区实测）：4 个国内版账号在同一天 23:52 同时 403 + 11140，
// 凭据校验与积分查询却都正常——这是平台侧批量动作，不是 4 个账号各自坏掉。
// 此时继续换号，等于把剩下还健康的号挨个送去撞风控。窗口 10 分钟、阈值 2。
//
// 归属层评估：放 server（Handler 局部）而非 pool——这类状态唯一消费者是
// chatCompletions 轮转循环（是否继续轮转），pool 是账号级记账层，跨 UID 语义
// 不属于任何账号；server 已有进程级状态先例 degradeGate（mu+until 同风格）。
// 账号级处置照常记账（applyErrorPolicy / Disable 不变），平台级状态只改变
// 「是否继续轮转」——协同不叠加。进程内状态、重启清零（窗口短，重建成本极低）。
package server

import (
	"log"
	"sync"
	"time"
)

// wafIPWindow WAF IP 级判定滑动窗 + 激活时长：窗内不同账号命中 WAF 403 达阈值即
// 判 IP 级拦截，激活同样长（到期自然解除）。var 仅供测试注入短窗（生产恒 60s）。
var wafIPWindow = 60 * time.Second

// wafIPThreshold 判定阈值：窗内不同 UID 数达到该值激活。取 2——「多号」的最小
// 定义：单号反复 403 永不触发（账号级偶发归软冷却管），两个不同号在 60s 内接连
// 被拦（同一出口 IP）已是 IP 级证据（BulidH 实测 3 号 1s 全拦，阈值 2 更早止损，
// 少放大一次轮转）。
const wafIPThreshold = 2

// restrictWindow 平台级账号受限的判定滑动窗。
//
// 比 WAF 窗口宽得多：WAF 是秒级爆发（同一出口 IP 同时被拦），而封号 wave 是
// 平台侧的批量动作，账号可能在几分钟到几十分钟内陆续被标记。窗口太窄会漏判
// （第 1 个号受限、第 2 个号 5 分钟后受限时窗已滑过，判定不到「平台级」）。
//
// 为什么不会误判：已受限的账号会被 Disable 移出选号（gateLocked 过滤），同一
// 个号不会重复喂进本状态机。因此窗内每个命中都代表「又一个原本健康的号被
// 平台标记了」，这正是平台级信号的准确定义。
var restrictWindow = 10 * time.Minute

// restrictThreshold 平台级账号受限的判定阈值。同样取 2：一个号受限是账号自己
// 的事（照常禁用、照常换号），两个不同号在 10 分钟内接连受限，已足以说明是
// 平台侧动作而非巧合。
const restrictThreshold = 2

// multiAcctGate 短窗「多少个不同账号命中」判定器（Handler 内嵌，零值可用）。
//
// 窗口/阈值/标签都不做字段，而由调用方在 note 时传入：保持零值可用（Handler
// 直接内嵌，无需构造），也让测试能按需注入短窗而不用改全局 var。
type multiAcctGate struct {
	mu    sync.Mutex
	hits  map[string]time.Time // uid → 最近一次命中时刻（判定窗内，惰性剪枝）
	until time.Time            // 平台级判定激活截止；零值 = 未激活
}

// note 记一次某账号命中，返回记账后平台级判定是否激活（调用方据此 fail-fast
// 终止轮转，优先于 rotateBackoff 退避）。
//   - 已激活（now < until）：不续期、不记账（窗口期内不重置——保守自然解除）→ true；
//   - 未激活：记 hits[uid]=now（同号重复命中覆盖不累计，判定口径是「不同号数」），
//     剪掉窗外的过期命中；不同 UID 数达 threshold → 激活到 now+window
//     （打一条 WARN 供观测），清空判定窗（解除后需全新命中重新判定，不叠旧账）。
func (g *multiAcctGate) note(uid string, window time.Duration, threshold int, label string) bool {
	now := time.Now()
	g.mu.Lock()
	defer g.mu.Unlock()
	if now.Before(g.until) {
		return true // 激活期内新命中：不续期（自然解除语义）
	}
	if g.hits == nil {
		g.hits = map[string]time.Time{}
	}
	g.hits[uid] = now
	for u, t := range g.hits {
		if now.Sub(t) > window {
			delete(g.hits, u)
		}
	}
	if len(g.hits) >= threshold {
		g.until = now.Add(window)
		log.Printf("WARN: [server] %s: %d accounts hit within %s, rotate fail-fast until %s",
			label, len(g.hits), window, g.until.Format(time.RFC3339))
		g.hits = map[string]time.Time{}
		return true
	}
	return false
}

// active 报告平台级判定是否已激活（末端错误文案区分平台级/账号级措辞用）。
func (g *multiAcctGate) active() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return time.Now().Before(g.until)
}

// noteWaf 记一次账号命中 WAF 403（场景 A）。
func (g *multiAcctGate) noteWaf(uid string) bool {
	return g.note(uid, wafIPWindow, wafIPThreshold, "waf ip-level block")
}

// noteRestricted 记一次账号被上游限制（场景 B）。
func (g *multiAcctGate) noteRestricted(uid string) bool {
	return g.note(uid, restrictWindow, restrictThreshold, "upstream account-restriction wave")
}
