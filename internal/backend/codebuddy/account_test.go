package codebuddy

import (
	"context"
	"github.com/linguo2625469/workbuddy2api-panel/internal/gateway"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// TestAccountAdapter 适配器必须正确暴露身份字段且标记 backend。
func TestAccountAdapter(t *testing.T) {
	a := &auth.Auth{UID: "u1", Nickname: "小明", Domain: "www.workbuddy.ai"}
	acc := Account{A: a}

	if acc.UID() != "u1" {
		t.Errorf("UID = %q", acc.UID())
	}
	if acc.Nickname() != "小明" {
		t.Errorf("Nickname = %q", acc.Nickname())
	}
	if acc.Backend() != Name {
		t.Errorf("Backend = %q, want %q", acc.Backend(), Name)
	}
	// global 域由 domain 后缀判定。
	if acc.Realm() != "global" {
		t.Errorf("Realm = %q, want global", acc.Realm())
	}
	// 往返：Unwrap 必须拿回同一个指针。
	if got := Unwrap(acc); got != a {
		t.Error("Unwrap 未返回原始 *auth.Auth")
	}
}

// TestUnwrapRejectsOtherBackend 非本 backend 的账号必须返回 nil，
// 否则 handler 会拿 Trae 账号去调 CodeBuddy 接口。
func TestUnwrapRejectsOtherBackend(t *testing.T) {
	if got := Unwrap(nil); got != nil {
		t.Error("nil 应返回 nil")
	}
	if got := Unwrap(fakeAccount{}); got != nil {
		t.Error("其他 backend 的账号应返回 nil")
	}
}

type fakeAccount struct{}

func (fakeAccount) UID() string      { return "x" }
func (fakeAccount) Nickname() string { return "" }
func (fakeAccount) Realm() string    { return "cn" }
func (fakeAccount) Backend() string  { return "trae" }
func (fakeAccount) Enterprise() bool { return false }

// credentialOnlyAccount 只实现 gateway.CredentialUnwrapper（模拟 pool.authAdapter
// 这类占位/测试路径账号）：它没有本包的 Account 具体类型，只能靠 Credential() 取凭证。
type credentialOnlyAccount struct {
	fakeAccount
	cred any
}

func (a credentialOnlyAccount) Credential() any { return a.cred }

// TestUnwrapViaCredentialUnwrapper 占位账号经 CredentialUnwrapper 也能解出凭证。
//
// 回归点：Unwrap 曾只认本包 Account 类型。把 CodeBuddy 切到 Backend.Chat 后，
// 测试/占位路径上的 pool.authAdapter 账号会集体解不出凭证（nil），表现为
// 「测试全挂但生产正常」。
func TestUnwrapViaCredentialUnwrapper(t *testing.T) {
	a := &auth.Auth{UID: "u9"}
	acc := credentialOnlyAccount{fakeAccount: fakeAccount{}, cred: a}
	if got := Unwrap(acc); got != a {
		t.Errorf("应经 CredentialUnwrapper 取回同一凭证，实际 %v", got)
	}
	// Credential() 返回非 *auth.Auth 时仍应返回 nil（不能瞎断言）。
	bad := credentialOnlyAccount{fakeAccount: fakeAccount{}, cred: "not-an-auth"}
	if got := Unwrap(bad); got != nil {
		t.Errorf("凭证类型不符时应返回 nil，实际 %v", got)
	}
}
func (fakeAccount) Credits(context.Context) (*gateway.Credits, error) {
	return nil, gateway.ErrUnsupported
}
func (fakeAccount) Checkin(context.Context) (*gateway.CheckinResult, error) {
	return nil, gateway.ErrUnsupported
}
func (fakeAccount) Keepalive(context.Context) error { return gateway.ErrUnsupported }
