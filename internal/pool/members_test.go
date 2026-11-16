package pool

import (
	"context"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/gateway"
)

// fakeMemberAccount 可指定 backend 的测试账号（bareAccount 的 backend 也是可设的，
// 但它在包内；这里用 authAdapter + 显式 backend 覆盖不便，故自建一个最小实现）。
type fakeMemberAccount struct {
	uid     string
	backend string
	realm   string
}

func (a fakeMemberAccount) UID() string      { return a.uid }
func (a fakeMemberAccount) Nickname() string { return "" }
func (a fakeMemberAccount) Realm() string    { return a.realm }
func (a fakeMemberAccount) Backend() string  { return a.backend }
func (a fakeMemberAccount) Enterprise() bool { return false }

func (a fakeMemberAccount) Credits(_ context.Context) (*gateway.Credits, error) {
	return nil, gateway.ErrUnsupported
}
func (a fakeMemberAccount) Checkin(_ context.Context) (*gateway.CheckinResult, error) {
	return nil, gateway.ErrUnsupported
}
func (a fakeMemberAccount) Keepalive(_ context.Context) error { return gateway.ErrUnsupported }

// TestMembersFiltersByBackend 按 backend 过滤只返回该上游的账号。
func TestMembersFiltersByBackend(t *testing.T) {
	p := New("")
	p.Add(fakeMemberAccount{uid: "c1", backend: "codebuddy", realm: "cn"})
	p.Add(fakeMemberAccount{uid: "t1", backend: "trae", realm: "cn"})
	p.Add(fakeMemberAccount{uid: "t2", backend: "trae", realm: "global"})

	got := p.Members(MemberFilter{Backend: "trae", IncludeDisabled: true, IncludePaused: true})
	if len(got) != 2 {
		t.Fatalf("应返回 2 个 trae 账号，实际 %d", len(got))
	}
	for _, m := range got {
		if m.Acc.Backend() != "trae" {
			t.Errorf("混入了非 trae 账号: %s", m.Acc.UID())
		}
	}
	// 排序稳定：按 QualifiedUID 升序 → trae:t1 < trae:t2
	if got[0].Acc.UID() != "t1" || got[1].Acc.UID() != "t2" {
		t.Errorf("输出应按 QualifiedUID 排序，实际 %s, %s", got[0].Acc.UID(), got[1].Acc.UID())
	}
}

// TestMembersFiltersRealm 按域过滤（Trae 的 cn/global 按 base 域名判定）。
func TestMembersFiltersRealm(t *testing.T) {
	p := New("")
	p.Add(fakeMemberAccount{uid: "t1", backend: "trae", realm: "cn"})
	p.Add(fakeMemberAccount{uid: "t2", backend: "trae", realm: "global"})

	got := p.Members(MemberFilter{Realm: "global", IncludeDisabled: true, IncludePaused: true})
	if len(got) != 1 || got[0].Acc.UID() != "t2" {
		t.Fatalf("应只返回 global 账号，实际 %+v", got)
	}
}

// TestMembersExcludesDisabledByDefault 默认排除已禁用账号（安全缺省）。
func TestMembersExcludesDisabledByDefault(t *testing.T) {
	p := New("")
	p.Add(fakeMemberAccount{uid: "t1", backend: "trae", realm: "cn"})
	p.Add(fakeMemberAccount{uid: "t2", backend: "trae", realm: "cn"})
	p.Disable("t2", "test")

	got := p.Members(MemberFilter{Backend: "trae"})
	if len(got) != 1 || got[0].Acc.UID() != "t1" {
		t.Fatalf("默认应排除禁用账号，实际 %+v", got)
	}
	// 显式包含时两个都在
	all := p.Members(MemberFilter{Backend: "trae", IncludeDisabled: true, IncludePaused: true})
	if len(all) != 2 {
		t.Fatalf("IncludeDisabled=true 应返回 2 个，实际 %d", len(all))
	}
}

// TestMembersExcludesPausedByDefault 默认排除暂停账号，显式包含时可取回。
//
// 语义提醒：paused 只关选号、不关保号任务，所以保号类调用方应显式置 true。
func TestMembersExcludesPausedByDefault(t *testing.T) {
	p := New("")
	p.Add(fakeMemberAccount{uid: "t1", backend: "trae", realm: "cn"})
	p.Add(fakeMemberAccount{uid: "t2", backend: "trae", realm: "cn"})
	p.Pause("t2")

	got := p.Members(MemberFilter{Backend: "trae"})
	if len(got) != 1 || got[0].Acc.UID() != "t1" {
		t.Fatalf("默认应排除暂停账号，实际 %+v", got)
	}
	withPaused := p.Members(MemberFilter{Backend: "trae", IncludePaused: true})
	if len(withPaused) != 2 {
		t.Fatalf("IncludePaused=true 应返回 2 个，实际 %d", len(withPaused))
	}
}

// TestMembersStatusFromSameSnapshot 成员快照里的 Status 与账号对象同源。
func TestMembersStatusFromSameSnapshot(t *testing.T) {
	p := New("")
	p.Add(fakeMemberAccount{uid: "t1", backend: "trae", realm: "cn"})
	p.Disable("t1", "credential revoked")

	got := p.Members(MemberFilter{IncludeDisabled: true})
	if len(got) != 1 {
		t.Fatalf("应返回 1 个成员，实际 %d", len(got))
	}
	m := got[0]
	if !m.Status.Disabled {
		t.Error("Status.Disabled 应为 true")
	}
	if m.Status.DisabledReason != "credential revoked" {
		t.Errorf("Status.DisabledReason = %q, want %q", m.Status.DisabledReason, "credential revoked")
	}
	if m.Status.Backend != "trae" {
		t.Errorf("Status.Backend = %q, want trae", m.Status.Backend)
	}
}

// TestAccountsByBackendIncludesDisabled 便捷方法包含禁用/暂停账号。
func TestAccountsByBackendIncludesDisabled(t *testing.T) {
	p := New("")
	p.Add(fakeMemberAccount{uid: "t1", backend: "trae", realm: "cn"})
	p.Add(fakeMemberAccount{uid: "t2", backend: "trae", realm: "cn"})
	p.Add(fakeMemberAccount{uid: "c1", backend: "codebuddy", realm: "cn"})
	p.Disable("t2", "test")

	if got := p.AccountsByBackend("trae"); len(got) != 2 {
		t.Fatalf("AccountsByBackend(trae) 应返回 2 个，实际 %d", len(got))
	}
	if got := p.Accounts(); len(got) != 3 {
		t.Fatalf("Accounts() 应返回全部 3 个，实际 %d", len(got))
	}
}

// TestMembersRemovedAccountAbsent 已移除的账号不出现在成员列表里。
func TestMembersRemovedAccountAbsent(t *testing.T) {
	p := New("")
	p.Add(fakeMemberAccount{uid: "t1", backend: "trae", realm: "cn"})
	p.Add(fakeMemberAccount{uid: "t2", backend: "trae", realm: "cn"})
	p.Remove("t1")

	got := p.Members(MemberFilter{Backend: "trae", IncludeDisabled: true, IncludePaused: true})
	if len(got) != 1 || got[0].Acc.UID() != "t2" {
		t.Fatalf("移除后应只剩 t2，实际 %+v", got)
	}
}

// TestMembersWithWrappedAuth 生产形态（authAdapter）也能被 Members 取出。
func TestMembersWithWrappedAuth(t *testing.T) {
	p := New("")
	p.Add(WrapAuth(&auth.Auth{UID: "u1"}))

	got := p.Members(MemberFilter{IncludeDisabled: true, IncludePaused: true})
	if len(got) != 1 {
		t.Fatalf("应返回 1 个，实际 %d", len(got))
	}
	if got[0].Acc.UID() != "u1" {
		t.Errorf("UID = %q, want u1", got[0].Acc.UID())
	}
	if got[0].Status.Backend != BackendCodeBuddy {
		t.Errorf("Backend = %q, want %q", got[0].Status.Backend, BackendCodeBuddy)
	}
}
