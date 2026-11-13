// Package codebuddy 把 internal/upstream（CodeBuddy 上游）适配成 gateway.Backend。
//
// 依赖方向：codebuddy → upstream/auth/gateway。gateway 不反向依赖任何 internal 包，
// pool 也不认识本包——三者通过接口解耦。
package codebuddy

import (
	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/gateway"
)

// Name 上游标识。
const Name = "codebuddy"

// Account 把 *auth.Auth 适配成 gateway.Account。
//
// 为什么包装而不是直接给 auth.Auth 加方法：那会让 internal/auth 反向依赖
// gateway，破坏「gateway 不依赖任何 internal 包」的设计前提。
type Account struct{ A *auth.Auth }

func (x Account) UID() string      { return x.A.UID }
func (x Account) Nickname() string { return x.A.NicknameValue() }
func (x Account) Realm() string    { return x.A.Realm() }
func (x Account) Backend() string  { return Name }
func (x Account) Enterprise() bool { return x.A.IsEnterprise() }

// Unwrap 取出原始凭证（非本 backend 的账号返回 nil）。
func Unwrap(acc gateway.Account) *auth.Auth {
	if a, ok := acc.(Account); ok {
		return a.A
	}
	if a, ok := acc.(*Account); ok {
		return a.A
	}
	return nil
}

// Wrap 批量包装。
func Wrap(list []*auth.Auth) []gateway.Account {
	out := make([]gateway.Account, 0, len(list))
	for _, a := range list {
		if a != nil {
			out = append(out, Account{A: a})
		}
	}
	return out
}
