package pool

import (
	"path/filepath"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/gateway"
)

// fakeTraeAccount 最小 gateway.Account 实现（backend=trae，用于验证复合键路径）。
type fakeTraeAccount struct{ uid string }

func (f fakeTraeAccount) UID() string      { return f.uid }
func (f fakeTraeAccount) Nickname() string { return "" }
func (f fakeTraeAccount) Realm() string    { return "cn" }
func (f fakeTraeAccount) Backend() string  { return "trae" }
func (f fakeTraeAccount) Enterprise() bool { return false }

// TestPauseSurvivesReloadTraeAccount Trae 账号走复合键（trae:uid），
// 暂停状态同样必须跨重启保持。
//
// 这是用户报的场景：账号池里 Trae 账号暂停后重启变回未暂停。
func TestPauseSurvivesReloadTraeAccount(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "state.json")

	p := New(fp)
	p.Add(fakeTraeAccount{uid: "4175318908939818"})
	// 面板传的是裸 uid。
	if !p.Pause("4175318908939818") {
		t.Fatal("Pause 应能按裸 uid 找到 Trae 账号")
	}
	p.Flush()

	p2 := New(fp)
	p2.SyncToDir([]gateway.Account{fakeTraeAccount{uid: "4175318908939818"}})
	st, ok := p2.Status("4175318908939818")
	if !ok || !st.Paused {
		t.Fatalf("Trae 账号重载后应保持暂停: ok=%v paused=%v st=%+v", ok, st.Paused, st)
	}
}
