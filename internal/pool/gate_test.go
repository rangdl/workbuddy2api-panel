// 选号可用性判据的一致性测试。
//
// 背景：同一套「这个号此刻能不能接这个模型的请求」原本在十处各写一遍，其中
// WeightedAvailableUIDsForModelRealm（新会话粘性分配的候选列表来源）与
// AvailableUIDsForModelRealm 都漏了积分保底，而 ServableForModelRealm 有 ——
// 于是同一个问题存在两种答案：计数器说「这个域没有可服务的号」，候选表却能把
// 触底号分配出去，随后在 PickByUIDForModel 才被拦住并解绑。
//
// 本文件的目的是：让「所有入口对同一个号给出同一个答案」成为可断言的事实，
// 而不是靠改动时记得去十个地方各加一行。
package pool

import (
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// TestGateAgreesAcrossEntryPoints 触底号（credits < floor 且模型实测收费）必须被
// **每一个**模型感知的入口一致排除。
func TestGateAgreesAcrossEntryPoints(t *testing.T) {
	withNoPickGap(t)
	p := New("")
	p.SetCostExploreInterval(0)
	p.SetCreditFloor(100)
	p.SetRandomSource(func(n int64) int64 { return 0 })
	p.Add(WrapAuth(&auth.Auth{UID: "poor"})) // 空 domain → Realm()=="cn"
	p.Add(WrapAuth(&auth.Auth{UID: "rich"}))
	p.SetCredits("poor", 30, 30)
	p.SetCredits("rich", 1_000_000, 1_000_000)
	p.NoteModelCost("poor", "hy4-preview", 2.9, 1000) // 实测收费 → floor 生效
	p.NoteModelCost("rich", "hy4-preview", 5.0, 1000)

	const model = "hy4-preview"

	// ① 候选表类入口：触底号一律不得出现。
	tables := []struct {
		name string
		got  []string
	}{
		{"AvailableUIDsForModel", p.AvailableUIDsForModel(model)},
		{"AvailableUIDsForModelRealm", p.AvailableUIDsForModelRealm(model, "cn")},
		{"WeightedAvailableUIDsForModelRealm", p.WeightedAvailableUIDsForModelRealm(model, "cn")},
	}
	for _, tbl := range tables {
		if len(tbl.got) == 0 {
			t.Fatalf("%s 为空：rich 应可用", tbl.name)
		}
		for _, uid := range tbl.got {
			if uid == "poor" {
				t.Errorf("%s 含触底号 poor，积分保底漏判: %v", tbl.name, tbl.got)
			}
		}
	}

	// ② 计数口径必须与候选表口径一致（曾经「计数 0 / 候选表非空」两种答案）。
	if n := p.ServableForModelRealm(model, "cn"); n != 1 {
		t.Errorf("ServableForModelRealm=%d，want 1（只有 rich 可服务）", n)
	}

	// ③ 粘性直取：分配阶段就得拒绝，不能「先粘上再解绑」。
	if a := p.PickByUIDForModel("poor", model); a != nil {
		t.Errorf("PickByUIDForModel(poor)=%s，want nil（触底号不得续期）", a.UID())
	}

	// ④ 普通选号：不得选中触底号。
	for i := 0; i < 10; i++ {
		a := p.PickExcludingForRealm(nil, model, "cn")
		if a == nil {
			t.Fatalf("第 %d 次选号返回 nil，want rich", i)
		}
		if a.UID() == "poor" {
			t.Fatalf("第 %d 次选中触底号 poor", i)
		}
	}
}

// TestGateAllowsFloorOnFreeModel 保底只拦「实测收费」模型：同一触底号对免费模型
// 照常可用——所有入口必须一致地放行（防止把保底做成「什么都不许调」）。
func TestGateAllowsFloorOnFreeModel(t *testing.T) {
	withNoPickGap(t)
	p := New("")
	p.SetCostExploreInterval(0)
	p.SetCreditFloor(100)
	p.Add(WrapAuth(&auth.Auth{UID: "poor"}))
	p.SetCredits("poor", 30, 30)
	p.NoteModelCost("poor", "free-model", 0, 1000) // 实测免费 → tier 0，floor 不拦

	for _, name := range []string{"AvailableUIDsForModel", "AvailableUIDsForModelRealm", "WeightedAvailableUIDsForModelRealm"} {
		var got []string
		switch name {
		case "AvailableUIDsForModel":
			got = p.AvailableUIDsForModel("free-model")
		case "AvailableUIDsForModelRealm":
			got = p.AvailableUIDsForModelRealm("free-model", "cn")
		case "WeightedAvailableUIDsForModelRealm":
			got = p.WeightedAvailableUIDsForModelRealm("free-model", "cn")
		}
		if len(got) != 1 || got[0] != "poor" {
			t.Errorf("%s=%v，want [poor]（免费模型不受保底限制）", name, got)
		}
	}
	if a := p.PickByUIDForModel("poor", "free-model"); a == nil {
		t.Error("PickByUIDForModel(poor, free-model)=nil，want poor（免费模型不受保底限制）")
	}
}
