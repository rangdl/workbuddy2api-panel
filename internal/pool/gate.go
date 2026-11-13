// 选号可用性判据的**单一事实来源**。
//
// 为什么要有这个文件：同一套「这个号此刻能不能接这个模型的请求」的判定，曾在
// 十处各写一遍（pick / pickEarliestExpiryLocked / PickByUID / PickByUIDForModel /
// AvailableUIDs / AvailableUIDsForModel / AvailableUIDsForRealm /
// AvailableUIDsForModelRealm / WeightedAvailableUIDsForModelRealm /
// ServableForModelRealm），每处都要手动凑齐「域 / 健康 / 积分保底 / 在途」四个
// 条件，且各处的组合顺序还得自己记得住。**已经漏过一次**：
// WeightedAvailableUIDsForModelRealm（新会话粘性分配的候选列表来源）缺了积分保底，
// 于是触底号照样被分配给新会话，直到 PickByUIDForModel 才被拦住 —— 表现为
// 「先粘一个死号再解绑」的抖动 + 反复刷同一条 WARN 日志。
//
// 这不是粗心，是结构问题：一个判据 N 份拷贝，改一份忘一份是必然的。所以判据只在
// gateLocked 里组合一次，各处入口统一调它；将来再加硬约束（比如新的成本/额度维度）
// 只需改这里，所有入口自动同步。
//
// 各处唯一的合法差异是「口径」：
//   - 常规口径（gateSpec 零值）：健康 = healthy / healthyForModel（6004 模型豁免生效）；
//   - 兜底口径（allowCooling）：软冷却 / 熔断号仍可参与试探（它们可能已恢复，失败
//     成本仅一轮换），但余额耗尽的 CoolHard 号不参与（调了必 402，白烧轮换）。
package pool

import (
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/gateway"
)

// exclusion 报告一个账号被排除的原因。空值 = 可用（候选）。
//
// 做成枚举而不是 bool：排除原因是可观测性的一部分（哪条约束拦下了它），
// 也让「十处入口是否一致」可以被单测直接断言。
type exclusion string

const (
	excludeNone        exclusion = ""
	excludeTried       exclusion = "tried"          // 本次请求已试过它（请求级轮换）
	excludeBackend     exclusion = "backend"        // 不属于请求锁定的上游
	excludeRealm       exclusion = "realm"          // 不属于请求所在的域
	excludeUnhealthy   exclusion = "unhealthy"      // 禁用/暂停/冷却/熔断/降级/模型级冷却
	excludeHardCooling exclusion = "hard-cooling"   // 余额耗尽冷却中（仅兜底口径区分）
	excludeCreditFloor exclusion = "credit-floor"   // 积分保底：触底不接实测收费模型
	excludeInFlight    exclusion = "in-flight-full" // 在途名额占满
)

// ok 报告该原因是否表示「可用」。
func (x exclusion) ok() bool { return x == excludeNone }

// gateSpec 描述一次可用性询问的上下文。
//
// 零值即「账号级、不限域、不考虑请求级轮换」——等于旧 AvailableUIDs / PickByUID
// 的口径。model 为空时 healthy 退化为账号级 healthy()、积分保底恒不拦（无成本维度）。
type gateSpec struct {
	model string // 请求模型；空 = 账号级口径（不查 6004 模型级冷却、不查成本）
	realm string // 请求域（cn/global）；空 = 不按域过滤
	// backend 请求锁定的上游；空 = 不限（跨上游统一调度）。
	//
	// 三种来源会设置它：模型名显式前缀（trae:...）、粘性会话绑定
	// （同一会话必须落在同一上游，否则 prompt cache 全废）、
	// 以及回退流程中的「先只在本上游内换号」阶段。
	backend string
	now     time.Time // 判定时刻（调用方取一次，保证同一次遍历内时间一致）
	// tried 请求级轮换：本次请求已试过的账号。
	//
	// 键是 gateway.QualifiedUID（"backend:uid"）而非裸 uid——两个上游的
	// uid 形态可能相同，裸 uid 会让「Trae 的 u1 试过了」误伤「CodeBuddy 的 u1」。
	tried        map[string]bool
	allowCooling bool // 兜底口径：软冷却/熔断号可参与（CoolHard 仍排除）
}

// gateLocked 判定账号 e 在本次询问下是否可用，返回排除原因。
//
// 调用方**必须已持有 p.mu**（读锁即可）：本方法读账号状态与积分保底配置。
// 条件顺序有意为之：先便宜的过滤（tried/域），再健康口径，最后成本与租约
// （后两者各含一次表查询 / 原子读）。
func (p *Pool) gateLocked(e *entry, s gateSpec) exclusion {
	if s.tried != nil && s.tried[gateway.QualifiedUID(e.acc)] {
		return excludeTried
	}
	if s.backend != "" && e.acc.Backend() != s.backend {
		return excludeBackend
	}
	if s.realm != "" && e.acc.Realm() != s.realm {
		return excludeRealm
	}
	if s.allowCooling {
		// 兜底口径：不看 healthy（冷却中的号正是要试探的对象），只排除
		// 禁用/暂停（终态，需人工介入）与有效 CoolHard（余额耗尽，等签到）。
		if e.disabled || e.paused {
			return excludeUnhealthy
		}
		if e.coolKind == CoolHard && !e.until.IsZero() && s.now.Before(e.until) {
			return excludeHardCooling
		}
	} else if !p.healthyLocked(e, s.model, s.now) {
		return excludeUnhealthy
	}
	// 积分保底：model 为空时恒不拦（见 floorBlockedForRealmModel 顶部早返回）。
	if p.floorBlockedForRealmModel(e, s.model, s.realm, s.now) {
		return excludeCreditFloor
	}
	if p.inFlightFull(e) {
		return excludeInFlight
	}
	return excludeNone
}

// healthyLocked 健康口径：model 非空用模型级判定（6004 冷却对其它模型豁免），
// 否则用账号级判定。抽出来是为了让 gateLocked 里只有一处「口径分叉」。
func (p *Pool) healthyLocked(e *entry, model string, now time.Time) bool {
	if model == "" {
		return e.healthy(now)
	}
	return e.healthyForModel(now, model)
}
