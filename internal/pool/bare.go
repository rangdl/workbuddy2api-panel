// bare.go 上游无关的占位账号与占位工厂。
//
// 历史：本文件曾是 adapter.go 的一部分，里面直接持有 *auth.Auth —— 那让
// internal/pool 事实上 import 了 internal/auth，与「池只认 gateway」的
// 分层约定相悖（见 docs/architecture.md「已知例外①」）。
//
// 现在占位账号由装配期注入的工厂构造（SetPlaceholderFactory），CodeBuddy 的
// 适配器搬到了 backend/codebuddy（它本来就知道凭证），池只保留上游无关的部分。
package pool

import (
	"context"

	"github.com/linguo2625469/workbuddy2api-panel/internal/gateway"
)

// BackendCodeBuddy CodeBuddy 上游标识。
//
// 保留在 pool 里是**唯一**的例外用途：state.json 的落盘键兼容（旧文件的键是
// 裸 uid，属于 CodeBuddy）。这是数据格式的向后兼容，不是上游逻辑分支——
// 池不认识 CodeBuddy 的任何能力，只认识这个键名。
const BackendCodeBuddy = "codebuddy"

// BackendTrae Trae 上游标识。
//
// 与 backend/trae.Name 同值。这里重复定义一份是为了让 pool/scheduler 不必
// import 具体 backend 包（依赖方向：pool 只认 gateway，不认某个上游）。
const BackendTrae = "trae"

// bareAccount 最小账号实现（无凭证、无 backend 特异能力）。
//
// 用途：applyAccountsLocked 恢复持久化状态时先建 entry 承载 credits/冷却等
// 数据，真实账号要等 SyncToDir/Add 才到位。占位账号只需让 entry.acc 非 nil
// （避免 nil 解引用），且 Backend() 能回答出所属上游。
type bareAccount struct {
	backend string
	uid     string
}

func (b bareAccount) UID() string      { return b.uid }
func (b bareAccount) Nickname() string { return "" }
func (b bareAccount) Realm() string    { return gateway.DefaultRealm }
func (b bareAccount) Backend() string  { return b.backend }
func (b bareAccount) Enterprise() bool { return false }

// 能力方法：bareAccount 只是占位（承载持久化状态、等真实账号替换），
// 没有任何上游能力，一律返回 ErrUnsupported 让池跳过。
func (b bareAccount) Credits(context.Context) (*gateway.Credits, error) {
	return nil, gateway.ErrUnsupported
}
func (b bareAccount) Checkin(context.Context) (*gateway.CheckinResult, error) {
	return nil, gateway.ErrUnsupported
}
func (b bareAccount) Keepalive(context.Context) error { return gateway.ErrUnsupported }

// placeholderAccount 构造一个「只有身份、没有凭证」的占位账号。
//
// 优先用注入的工厂：CodeBuddy 的占位账号需要是 backend/codebuddy.Account
// （这样它能通过 gateway.CredentialUnwrapper 交出凭证，且实现全部特性能力），
// 而池不能 import 那个包。
func (p *Pool) placeholderAccount(backend, uid string) gateway.Account {
	if p.placeholder != nil {
		if a := p.placeholder(backend, uid); a != nil {
			return a
		}
	}
	// 未注入工厂（裸用/测试）：上游无关的最小实现。
	return bareAccount{backend: backend, uid: uid}
}

// SetPlaceholderFactory 注入占位账号工厂（main 在多 backend 部署时注入）。
//
// 传 nil 表示回到 bareAccount。工厂必须对任意 (backend, uid) 返回非 nil 或 nil，
// 不得 panic——它在持池锁的恢复路径上调用。
func (p *Pool) SetPlaceholderFactory(fn func(backend, uid string) gateway.Account) {
	p.mu.Lock()
	p.placeholder = fn
	p.mu.Unlock()
}

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
