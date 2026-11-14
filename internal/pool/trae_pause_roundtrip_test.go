package pool

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/gateway"
)

// TestTraePausedSurvivesDiskRoundtrip 端到端复现用户报的问题：
// 暂停 Trae 账号 → 落盘 → 新进程加载 → 状态必须保持。
//
// 与既有测试的差异：这里**直接检查 state.json 的内容**，而不只是看内存。
// 用户报的正是「磁盘上明明有记录，重启后界面变回未暂停」。
func TestTraePausedSurvivesDiskRoundtrip(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "state.json")

	// 第一次运行：Add（模拟 main 的 syncTraeAccountsToPool）+ Pause + 落盘。
	p1 := New(fp)
	p1.Add(fakeTraeAccount{uid: "4175318908939818"})
	if !p1.Pause("4175318908939818") {
		t.Fatal("Pause 应成功")
	}
	p1.Flush()

	// 磁盘上必须有这条记录且 paused=true。
	raw, err := os.ReadFile(fp)
	if err != nil {
		t.Fatalf("state.json 应存在: %v", err)
	}
	var sf stateFile
	if err := json.Unmarshal(raw, &sf); err != nil {
		t.Fatalf("state.json 应可解析: %v", err)
	}
	rec, ok := sf.Accounts["trae:4175318908939818"]
	if !ok {
		t.Fatalf("磁盘上应有 trae 复合键记录，实际键: %v", keysOf(sf.Accounts))
	}
	if !rec.Paused {
		t.Fatal("磁盘上 paused 应为 true")
	}

	// 第二次运行：New 加载 → Add 重新同步（与 main 的顺序一致）。
	p2 := New(fp)
	p2.Add(fakeTraeAccount{uid: "4175318908939818"})
	st, found := p2.Status("4175318908939818")
	if !found {
		t.Fatal("重载后账号应存在")
	}
	if !st.Paused {
		t.Errorf("重载后 paused 应为 true（用户报的 bug）: %+v", st)
	}
}

// TestTraePausedSurvivesWithSyncToDir 同上，但第二次运行走 SyncToDir
// （CodeBuddy 的路径）后再 Add（Trae 的路径），覆盖 main 的真实顺序。
func TestTraePausedSurvivesWithSyncToDir(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "state.json")

	p1 := New(fp)
	p1.Add(fakeTraeAccount{uid: "t1"})
	p1.Pause("t1")
	p1.Flush()

	p2 := New(fp)
	// main 的顺序：先 RestoreFromSnapshot（此处无 store，跳过），
	// 再 SyncToDir（CodeBuddy），再 syncTraeAccountsToPool（Add）。
	p2.SyncToDir(nil)
	p2.Add(fakeTraeAccount{uid: "t1"})

	st, found := p2.Status("t1")
	if !found {
		t.Fatal("重载后账号应存在")
	}
	if !st.Paused {
		t.Errorf("重载后 paused 应为 true: %+v", st)
	}
}

func keysOf(m map[string]stateAccount) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

var _ = gateway.QualifiedUID
