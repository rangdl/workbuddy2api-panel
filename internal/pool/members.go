// members.go 池成员的对外快照 API。
//
// 为什么需要：特色能力（旅行 / 盲盒 / 成长任务 / 连登 / 补偿 / 活跃上报）
// 与面板的逐账号操作都需要「拿到某类账号的对象」，而此前唯一的出口是
// AuthByUID —— 它只对 CodeBuddy 有效（返回 *auth.Auth），Trae 账号一律 nil，
// 于是所有特色功能天然只支持 CodeBuddy，调用方还得自己去 trae.json 里翻凭证。
//
// 本文件提供与上游无关的快照接口：调用方拿 gateway.Account，用类型断言或
// 可选能力接口判断「这个账号支不支持我要做的事」。
//
// 与 operations.go 的分工：
//   - operations.go 的批量方法（CheckinAll 等）是「池替所有账号做同一件事」，
//     账号不支持就跳过；
//   - members.go 只负责「把账号取出来」，具体做什么由调用方决定。
package pool

import (
	"sort"

	"github.com/linguo2625469/workbuddy2api-panel/internal/gateway"
)

// Member 一个池成员的快照：账号对象 + 同一把锁下取到的状态。
//
// 两者必须同源取：分两次取会出现「遍历时健康、处理后已禁用」这类矛盾口径，
// 调用方据此做的门控判断（跳过 disabled / paused）就失去意义。
type Member struct {
	Acc    gateway.Account
	Status Status
}

// MemberFilter 成员过滤条件。零值 = 全部成员（含已禁用、已暂停）。
//
// 刻意不提供「企业版 / global / 有无 refreshToken」这类过滤：它们是业务门控
// （不同任务口径不同，例如活跃上报含 global 但旅行不含），属于调用方的知识，
// 不该沉到池里——否则每加一条业务规则都要改池。
type MemberFilter struct {
	// Backend 只取指定上游（空 = 不限）。
	Backend string
	// Realm 只取指定域 "cn"/"global"（空 = 不限）。
	Realm string
	// IncludeDisabled 是否包含已禁用账号（零值 false = 排除）。
	//
	// 默认排除是安全选择：绝大多数调用方都不该对禁用号发上游请求。
	IncludeDisabled bool
	// IncludePaused 是否包含「暂停选号」账号（零值 false = 排除）。
	//
	// 注意语义：paused 只关选号，不关保号任务。所以保号类任务（签到 / 余额 /
	// 旅行等纯 RPC）通常应显式置 true，而对话类调用方保持 false。
	IncludePaused bool
}

// admitsMember 判定成员是否入选（与 opOptions.admits 同口径，但多一个 paused 维度）。
func (f MemberFilter) admitsMember(e *entry, st Status) bool {
	if e.acc == nil {
		return false
	}
	if f.Backend != "" && e.acc.Backend() != f.Backend {
		return false
	}
	if f.Realm != "" && e.acc.Realm() != f.Realm {
		return false
	}
	if st.Disabled && !f.IncludeDisabled {
		return false
	}
	if st.Paused && !f.IncludePaused {
		return false
	}
	return true
}

// Members 返回满足条件的成员快照，按 QualifiedUID 排序（输出稳定，便于日志与测试比对）。
//
// 返回值是值拷贝：调用方在锁外做网络调用，不持有池锁。这也是本 API 与
// accountsLocked 的关键共同约定——锁内绝不发请求。
func (p *Pool) Members(f MemberFilter) []Member {
	p.mu.RLock()
	out := make([]Member, 0, len(p.byUID))
	for _, e := range p.byUID {
		if e.acc == nil {
			continue
		}
		st := p.statusOf(e.acc.UID(), e)
		if !f.admitsMember(e, st) {
			continue
		}
		out = append(out, Member{Acc: e.acc, Status: st})
	}
	p.mu.RUnlock()

	sort.Slice(out, func(i, j int) bool {
		return gateway.QualifiedUID(out[i].Acc) < gateway.QualifiedUID(out[j].Acc)
	})
	return out
}

// Accounts 返回全部账号对象（含禁用/暂停）。等价于 Members 零值过滤后只取账号。
//
// 调用方需要账号状态（判断 disabled/paused）时请用 Members——单独调 Status
// 会引入「两次取不一致」的窗口。
func (p *Pool) Accounts() []gateway.Account {
	return accountsOf(p.Members(MemberFilter{IncludeDisabled: true, IncludePaused: true}))
}

// AccountsByBackend 返回指定上游的账号对象（含禁用/暂停；backend 为空 = 全部）。
func (p *Pool) AccountsByBackend(backend string) []gateway.Account {
	return accountsOf(p.Members(MemberFilter{
		Backend:         backend,
		IncludeDisabled: true,
		IncludePaused:   true,
	}))
}

// Backends 返回池内**实际存在账号**的上游名（去重，顺序稳定）。
//
// 为什么由池提供而不是让调用方读配置：调用方（调度器/面板）要的是「现在有
// 哪些上游需要处理」，配置里列着但没有账号的上游跑一轮是纯空转；反过来，新
// 加了账号的上游即使配置里没提也必须照常参与（缺键 = 启用）。
//
// 顺序按首次出现稳定排序，便于日志与面板展示不跳动。
func (p *Pool) Backends() []string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	seen := make(map[string]bool, 4)
	out := make([]string, 0, 4)
	for _, e := range p.byUID {
		if e.acc == nil {
			continue
		}
		be := e.acc.Backend()
		if be == "" || seen[be] {
			continue
		}
		seen[be] = true
		out = append(out, be)
	}
	sort.Strings(out)
	return out
}

// accountsOf 从成员快照里剥出账号对象。
func accountsOf(ms []Member) []gateway.Account {
	out := make([]gateway.Account, 0, len(ms))
	for _, m := range ms {
		out = append(out, m.Acc)
	}
	return out
}
