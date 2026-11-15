// placeholder.go Trae 账号的占位适配器。
//
// 与 codebuddy.Placeholder 同一用途：池从 state.json 恢复时先建条目承载
// credits/冷却等数据，真实账号要等 SyncToDir 才到位。
//
// 与 CodeBuddy 的差异：Trae 的账号对象就是 Account 本身（没有独立的
// auth 类型），所以占位直接用 Account 的零能力形态 —— 带 nil client 与
// nil A 以外的字段会让能力方法自己返回 ErrUnsupported，不需要另造类型。
package trae

import (
	"github.com/linguo2625469/workbuddy2api-panel/internal/gateway"
	traepkg "github.com/linguo2625469/workbuddy2api-panel/internal/trae"
)

// Placeholder 构造一个「只有身份、没有 client」的占位账号。
//
// realm 取缺省 cn：占位账号只用于恢复持久化状态，真正的 realm 在
// SyncToDir 换成带 client 的真实账号后才有意义。用 cn 而不是空串是因为
// gateway.Account 的契约要求 Realm() 返回非空（空串表示「不过滤域」，
// 会让域隔离失效）。
func Placeholder(uid string) gateway.Account {
	return Account{
		A:        &traepkg.Account{UID: uid},
		realm:    gateway.DefaultRealm,
		client:   nil,
		ConfigFn: nil,
	}
}

// 编译期断言：占位账号必须能通过 CredentialUnwrapper 交出凭证
// （backend 分发路径靠它取回 *trae.Account）。
var _ gateway.CredentialUnwrapper = Account{}
