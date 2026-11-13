package pool

import (
	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/gateway"
)

// authAdapter 把 *auth.Auth 适配成 gateway.Account。
//
// 为什么不直接在 auth 包上加方法：那会让 internal/auth 反向依赖 gateway，
// 而 gateway 的设计前提是「不依赖任何 internal 包」。适配器放在 pool 侧，
// 依赖方向依旧是 auth ← pool → gateway。
//
// 为什么不用类型断言取回 *auth.Auth：池里可能有多种账号实现，断言会失败。
// 适配器实现了 Unwrap，调用方需要原始凭证时用 UnwrapAuth 取出。
type authAdapter struct{ a *auth.Auth }

func (x authAdapter) UID() string      { return x.a.UID }
func (x authAdapter) Nickname() string { return x.a.NicknameValue() }
func (x authAdapter) Realm() string    { return x.a.Realm() }
func (x authAdapter) Backend() string  { return BackendCodeBuddy }
func (x authAdapter) Enterprise() bool { return x.a.IsEnterprise() }

// NicknameValue / SetNickname 实现 Nicknamer（可选能力）。
func (x authAdapter) NicknameValue() string { return x.a.NicknameValue() }

func (x authAdapter) SetNickname(name string) error {
	x.a.Lock()
	x.a.Nickname = name
	x.a.Unlock()
	return x.a.SaveAtomic()
}

// UnwrapAuth 取出适配的 *auth.Auth（非 CodeBuddy 账号返回 nil）。
func UnwrapAuth(acc gateway.Account) *auth.Auth {
	if ad, ok := acc.(authAdapter); ok {
		return ad.a
	}
	if ad, ok := acc.(*authAdapter); ok {
		return ad.a
	}
	return nil
}

// WrapAuth 把 *auth.Auth 包装成 gateway.Account。
//
// 保留给调用方（scheduler / panel）在传入池之前包装；nil 安全。
func WrapAuth(a *auth.Auth) gateway.Account {
	if a == nil {
		return nil
	}
	return authAdapter{a: a}
}

// WrapAuths 批量包装。
func WrapAuths(list []*auth.Auth) []gateway.Account {
	out := make([]gateway.Account, 0, len(list))
	for _, a := range list {
		if a != nil {
			out = append(out, authAdapter{a: a})
		}
	}
	return out
}

// BackendCodeBuddy CodeBuddy 上游标识。
const BackendCodeBuddy = "codebuddy"

// placeholderAccount 构造一个「只有身份、没有凭证」的占位账号。
//
// 用途：applyAccountsLocked 恢复持久化状态时，需要先建 entry 承载
// credits/冷却等数据，真实账号要等 SyncToDir/Add 才到位。占位账号只需
// 让 entry.acc 非 nil（避免 nil 解引用），且 Backend() 能回答出所属上游。
//
// 默认实现只支持 CodeBuddy（历史行为）。多 backend 部署用
// SetPlaceholderFactory 注入能构造任意 upstream 占位账号的工厂。
func (p *Pool) placeholderAccount(backend, uid string) gateway.Account {
	if p.placeholder != nil {
		if a := p.placeholder(backend, uid); a != nil {
			return a
		}
	}
	if backend == BackendCodeBuddy {
		return authAdapter{a: &auth.Auth{UID: uid}}
	}
	// 未知 backend 且无工厂：返回一个最小实现，避免 nil。
	return bareAccount{backend: backend, uid: uid}
}

// SetPlaceholderFactory 注入占位账号工厂（main 在多 backend 部署时注入）。
func (p *Pool) SetPlaceholderFactory(fn func(backend, uid string) gateway.Account) {
	p.mu.Lock()
	p.placeholder = fn
	p.mu.Unlock()
}

// bareAccount 最小账号实现（无凭证、无 backend 特异能力）。
type bareAccount struct {
	backend string
	uid     string
}

func (b bareAccount) UID() string      { return b.uid }
func (b bareAccount) Nickname() string { return "" }
func (b bareAccount) Realm() string    { return gateway.DefaultRealm }
func (b bareAccount) Backend() string  { return b.backend }
func (b bareAccount) Enterprise() bool { return false }

// TriedKey 返回 tried 集合的键（"backend:uid"）。
//
// 调用方**必须**用它构造 tried 键，不能直接写裸 uid：两个上游的 uid 形态
// 可能相同，裸 uid 会让「Trae 的 u1 试过了」误伤「CodeBuddy 的 u1」，
// 表现为换号重试提前耗尽、池里明明有可用号却报 no_healthy_account。
func TriedKey(acc gateway.Account) string {
	if acc == nil {
		return ""
	}
	return gateway.QualifiedUID(acc)
}
