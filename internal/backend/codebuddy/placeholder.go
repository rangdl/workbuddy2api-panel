// placeholder.go CodeBuddy 账号的占位适配器（原 pool.authAdapter）。
//
// 为什么搬到这里：它持有 *auth.Auth，留在 pool 里会让 internal/pool 事实上
// import internal/auth，与「池只认 gateway」的分层约定相悖（见
// docs/architecture.md「已知例外①」）。CodeBuddy 的凭证类型本来就属于本包，
// 放在这里依赖方向才干净：auth ← backend/codebuddy → gateway。
package codebuddy

import (
	"context"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/gateway"
)

// Placeholder 构造一个「只有身份、没有 HTTP 客户端」的占位账号。
//
// 用途：池从 state.json 恢复时先建条目承载 credits/冷却等数据，真实账号要等
// SyncToDir/Add 才到位。占位账号只要求 Backend() 能回答、Credential() 能交出
// 凭证（backend 分发路径靠 Unwrap 取回它），能力方法一律 ErrUnsupported。
//
// 与 Account 的区别：Account 带 client（真能打上游），Placeholder 不能。
// 之所以不复用 Account：Account 的能力方法会在 client 为 nil 时做一堆防御，
// 而占位账号的语义就是「明确没有能力」，两者混在一起容易误用。
func Placeholder(uid string) gateway.Account {
	return placeholder{A: &auth.Auth{UID: uid}}
}

type placeholder struct{ A *auth.Auth }

func (x placeholder) UID() string      { return x.A.UID }
func (x placeholder) Nickname() string { return x.A.NicknameValue() }
func (x placeholder) Realm() string    { return x.A.Realm() }
func (x placeholder) Backend() string  { return Name }
func (x placeholder) Enterprise() bool { return x.A.IsEnterprise() }

func (x placeholder) Credits(context.Context) (*gateway.Credits, error) {
	return nil, gateway.ErrUnsupported
}
func (x placeholder) Checkin(context.Context) (*gateway.CheckinResult, error) {
	return nil, gateway.ErrUnsupported
}
func (x placeholder) Keepalive(context.Context) error { return gateway.ErrUnsupported }

// Credential 实现 gateway.CredentialUnwrapper：backend 分发路径靠它取回凭证。
//
// 少了这个方法，Unwrap 对占位账号返回 nil，表现为「账号明明在池里却报
// account is not a CodeBuddy auth」——占位/测试路径全挂，而生产（走
// backend 包装）正常，是最难排查的那类不一致。
func (x placeholder) Credential() any { return x.A }

// NicknameValue / SetNickname 实现 pool.Nicknamer（可选能力）。
func (x placeholder) NicknameValue() string { return x.A.NicknameValue() }

func (x placeholder) SetNickname(name string) error {
	x.A.Lock()
	x.A.Nickname = name
	x.A.Unlock()
	return x.A.SaveAtomic()
}

// 编译期断言。
var _ gateway.Account = placeholder{}
var _ gateway.CredentialUnwrapper = placeholder{}

// WrapAuth 把 *auth.Auth 包装成池账号（占位形态，无 client）。
//
// 保留给调用方（scheduler / 测试）在传入池之前包装；nil 安全。
// 生产路径的应用层包装走 WrapWithClient（带 Credits/Checkin 能力）。
func WrapAuth(a *auth.Auth) gateway.Account {
	if a == nil {
		return nil
	}
	return placeholder{A: a}
}

// WrapAuths 批量包装。
func WrapAuths(list []*auth.Auth) []gateway.Account {
	out := make([]gateway.Account, 0, len(list))
	for _, a := range list {
		if a != nil {
			out = append(out, placeholder{A: a})
		}
	}
	return out
}

// UnwrapAuth 取出适配的 *auth.Auth（非 CodeBuddy 账号返回 nil）。
//
// 这是给「确实需要 CodeBuddy 凭证」的调用方用的（成长任务/余额/搜索）。
// 选号与调度路径不该用它——那些路径只该认 gateway.Account。
func UnwrapAuth(acc gateway.Account) *auth.Auth {
	switch a := acc.(type) {
	case placeholder:
		return a.A
	case *placeholder:
		return a.A
	}
	return Unwrap(acc)
}
