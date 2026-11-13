package codebuddy

import (
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
