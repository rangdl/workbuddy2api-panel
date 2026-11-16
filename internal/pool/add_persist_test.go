package pool

import (
	"path/filepath"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/gateway"
)

// TestAddMarksDirty 新账号必须置 dirty，否则永不落盘。
//
// 回归：Trae 账号靠 Add 进池（不走 SyncToDir），而 Add 从不置 dirty，
// 于是它们的暂停/禁用状态在 state.json 里根本没有记录——面板暂停后
// 重启就变回未暂停。
func TestAddMarksDirty(t *testing.T) {
	p := New("")
	if p.dirty.Load() {
		t.Fatal("初始状态不应 dirty")
	}
	p.Add(fakeTraeAccount{uid: "u1"})
	if !p.dirty.Load() {
		t.Error("Add 新账号后应置 dirty（否则不会落盘）")
	}
}

// TestAddPersistsTraeAccountState Trae 账号的暂停状态必须跨重启保持。
func TestAddPersistsTraeAccountState(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "state.json")

	p := New(fp)
	p.Add(fakeTraeAccount{uid: "t1"})
	p.Pause("t1")
	p.Flush()

	// 新进程：恢复 + 重新 Add（与 main 的 syncTraeAccountsToPool 一致）。
	p2 := New(fp)
	p2.Add(fakeTraeAccount{uid: "t1"})
	p2.SyncToDir([]gateway.Account{fakeTraeAccount{uid: "t1"}})
	st, ok := p2.Status("t1")
	if !ok {
		t.Fatal("账号应恢复")
	}
	if !st.Paused {
		t.Errorf("Trae 账号暂停状态应跨重启保持: %+v", st)
	}
}
