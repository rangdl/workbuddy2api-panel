// 分池选号域：按 realm（cn/global）过滤选号与可用集合。realm=="" 退化为现状。
package pool

import (
	"sort"
	"time"
)

// expiringVirtualSlots 快过期账号在新会话候选集中的虚拟实例权重。
// 3:1 是温和偏好，不是固定比例：账号组成变化会自然改变最终占比。
//
// 注意它与 pick 里的快过期偏好是**两个维度**：这里表达的是「新会话哈希分配时
// 更容易落到快过期号上」（重复槽位，无法表达硬优先级），pick 里则是先硬优先级
// 进短名单、再靠 ×expiringVirtualSlots 提高抽中概率（见 pick.go 的 atRisk 键）。
const expiringVirtualSlots = 3

// AvailableUIDsForRealm 同 AvailableUIDs，但仅返回 Realm()==realm 的账号。
// realm=="" 退化为 AvailableUIDs（现状语义）。
func (p *Pool) AvailableUIDsForRealm(realm string) []string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	now := time.Now()
	uids := make([]string, 0, len(p.byUID))
	for _, e := range p.byUID {
		if !p.gateLocked(e, gateSpec{realm: realm, now: now}).ok() {
			continue
		}
		uids = append(uids, e.acc.UID()) // 返回裸 uid
	}
	sort.Strings(uids)
	return uids
}

// ServableForModelRealm 返回 realm 内此刻**真正能服务该模型**的账号数。
//
// 与 pick 的候选口径一致（healthyForModel 的 6004/11102 模型级冷却豁免、在途租约、
// 积分保底都计入），但**不含 pick 的全冷却兜底**——跨域回退（issue #135）要的正是
// 「这个域此刻有没有健康号」，兜底把冷却号捞回来的乐观语义会掩盖这一点。
//
// 只读：持 RLock 遍历，不改任何状态。realm=="" 时统计全池（与既有 realm 语义一致）。
func (p *Pool) ServableForModelRealm(model, realm string) int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	now := time.Now()
	n := 0
	for _, e := range p.byUID {
		if !p.gateLocked(e, gateSpec{model: model, realm: realm, now: now}).ok() {
			continue
		}
		n++
	}
	return n
}

// WeightedAvailableUIDsForModelRealm 返回带虚拟实例权重的可用账号列表。
//
// 普通账号出现 1 次；有效快过期账号出现 expiringVirtualSlots 次。调用方继续按
// 原有序列表哈希，即可让新会话对快过期账号形成温和偏好。重复项按 UID 排序后
// 展开，保证同一账号拓扑下不同进程得到一致列表。
//
// 该方法是现有 AvailableUIDsForModelRealm 的增量入口：两者只在「快过期账号是否
// 重复计入」上不同，可用性判据**完全一样**（同一个 gateLocked，含积分保底）。
// 此前两处各自手写一遍条件表，本函数漏了积分保底：触底号照样进入新会话候选表，
// 被粘上后才在 PickByUIDForModel 拦住——「先粘一个死号再解绑」。
// prefer_expiring=false 时退化为逐账号一次。
func (p *Pool) WeightedAvailableUIDsForModelRealm(model, realm string) []string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	now := time.Now()
	uids := make([]string, 0, len(p.byUID))
	for _, e := range p.byUID {
		if !p.gateLocked(e, gateSpec{model: model, realm: realm, now: now}).ok() {
			continue
		}
		uids = append(uids, e.acc.UID()) // 返回裸 uid（调用方按裸 uid 使用）
	}
	sort.Strings(uids)

	out := make([]string, 0, len(uids))
	for _, uid := range uids {
		slots := 1
		if p.preferExpiring {
			if e, ok := p.lookupLocked(uid); ok && expiringNow(e, now) {
				slots = expiringVirtualSlots
			}
		}
		for i := 0; i < slots; i++ {
			out = append(out, uid)
		}
	}
	return out
}

// AvailableUIDsForModelRealm 同 AvailableUIDsForModel，但仅返回 Realm()==realm 的账号
// （6004 模型豁免照常生效）。realm=="" 退化为 AvailableUIDsForModel。
//
// 两者现在都由 gateLocked 判定，因此语义确实相等（含积分保底）；此前本函数缺
// 积分保底而 AvailableUIDsForModel 也无，两者虽相等但都与 ServableForModelRealm
// 不一致（后者计入保底）——同一个问题两种答案。
func (p *Pool) AvailableUIDsForModelRealm(model, realm string) []string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	now := time.Now()
	uids := make([]string, 0, len(p.byUID))
	for _, e := range p.byUID {
		if !p.gateLocked(e, gateSpec{model: model, realm: realm, now: now}).ok() {
			continue
		}
		uids = append(uids, e.acc.UID()) // 返回裸 uid
	}
	sort.Strings(uids)
	return uids
}
