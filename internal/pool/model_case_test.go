package pool

import (
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// model_case_test.go 守「模型名大小写归一」。
//
// 真实背景：Trae 的上游目录里同一个模型可能以 `DeepSeek-V4.1-Flash` 出现，
// 而客户端写 `deepseek-v4.1-flash`。归一是**只在网关内部比对**做的：
//
//   - 选号的存在性校验（gate.go 的 excludeNoModel）
//   - 模型级冷却台账（modelCooldowns）
//   - 实测成本台账（modelCost）
//   - 倍率查询
//
// **出站 wire 名绝不归一**：上游只认它自己发布的那种拼写，改成小写会回 4001。
// 那条约束由 backend 层的测试守（TestWireNamePreservesCase）。

// TestModelKeyNormalizes 归一键的基本形态。
func TestModelKeyNormalizes(t *testing.T) {
	cases := map[string]string{
		"DeepSeek-V4.1-Flash":   "deepseek-v4.1-flash",
		"deepseek-v4.1-flash":   "deepseek-v4.1-flash",
		"DEEPSEEK-V4.1-FLASH":   "deepseek-v4.1-flash",
		"  deepseek-v4.1-flash": "deepseek-v4.1-flash", // 前后空白
		"":                      "",
	}
	for in, want := range cases {
		if got := modelKey(in); got != want {
			t.Errorf("modelKey(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestModelCooldownKeyCaseInsensitive 大写请求打出的冷却必须挡住小写请求。
//
// 不归一时的表现：同一账号同一模型，客户端换个大小写就绕过冷却重新打上游，
// 上游继续限流，账号被反复记失败——而面板上「该模型的冷却」看起来是生效的。
func TestModelCooldownKeyCaseInsensitive(t *testing.T) {
	withNoPickGap(t)
	p := New("")
	p.Add(fakeTraeAccount{uid: "tr1"})

	// 用混合大小写记一次模型级冷却。
	p.CooldownSoftForModel("tr1", time.Minute, time.Now().Add(30*time.Minute),
		"DeepSeek-V4.1-Flash", "429 model rate limit")

	p.mu.RLock()
	e, ok := p.lookupLocked("tr1")
	p.mu.RUnlock()
	if !ok {
		t.Fatal("账号未入池")
	}

	now := time.Now()
	for _, variant := range []string{
		"DeepSeek-V4.1-Flash", "deepseek-v4.1-flash", "DEEPSEEK-V4.1-FLASH",
	} {
		if !e.modelCooled(now, variant) {
			t.Errorf("%q 应命中同一份冷却（归一后同键）", variant)
		}
		if e.healthyForModel(now, variant) {
			t.Errorf("%q 该模型冷却中，不应可选", variant)
		}
	}
	// 别的模型不受牵连（模型级冷却的正向语义）。
	if e.modelCooled(now, "glm-5.2") {
		t.Error("glm-5.2 不该被 deepseek 的冷却波及")
	}
}

// TestModelCostKeyCaseInsensitive 实测成本台账按归一键累计。
//
// 不归一时的表现：同一个模型的观测被拆成两份，样本数永远攒不够，
// 成本分层（免费/未知/收费）在两种写法之间来回跳——积分保底因此失效。
func TestModelCostKeyCaseInsensitive(t *testing.T) {
	p := New("")
	p.Add(fakeTraeAccount{uid: "tr1"})

	p.NoteModelCost("tr1", "DeepSeek-V4.1-Flash", 2.0, 1000)
	p.NoteModelCost("tr1", "deepseek-v4.1-flash", 4.0, 1000)

	p.mu.RLock()
	e, ok := p.lookupLocked("tr1")
	p.mu.RUnlock()
	if !ok {
		t.Fatal("账号未入池")
	}

	now := time.Now()
	mc, seen := e.modelCostOf("deepseek-v4.1-flash", now)
	if !seen {
		t.Fatal("成本观测未命中（两种拼写应落到同一个键）")
	}
	if mc.Samples != 2 {
		t.Errorf("样本数 = %d, want 2（两种拼写应累计到同一条台账）", mc.Samples)
	}
	// 大写写法同样能读到。
	if _, seen := e.modelCostOf("DeepSeek-V4.1-Flash", now); !seen {
		t.Error("大写写法也应命中同一条台账")
	}
	// 台账里只有一条（归一后不应分裂成两条）。
	if len(e.modelCost) != 1 {
		t.Errorf("台账条目数 = %d, want 1（归一后不应分裂）", len(e.modelCost))
	}
}

// TestPickCaseInsensitiveCatalogMatch 目录里混合大小写时，小写请求也能选中该上游。
func TestPickCaseInsensitiveCatalogMatch(t *testing.T) {
	withNoPickGap(t)
	p := New("")
	p.Add(WrapAuth(&auth.Auth{UID: "cb1"})) // codebuddy / cn
	p.Add(fakeTraeAccount{uid: "tr1"})      // trae / cn

	// 只有 CodeBuddy 有这个模型，且目录里是混合大小写。
	seedCatalog(p, "codebuddy", "DeepSeek-V4.1-Flash")
	seedCatalog(p, "trae", "glm-5.2")

	// 客户端用小写请求：应选中 CodeBuddy，而不是被当成「没有该模型」全排除。
	for i := 0; i < 20; i++ {
		acc := p.PickForBackend(nil, "deepseek-v4.1-flash", "cn", "")
		if acc == nil {
			t.Fatal("大小写不同不应导致无候选（目录里确实有该模型）")
		}
		if acc.Backend() != "codebuddy" {
			t.Fatalf("应选中 codebuddy，却选中 %s", acc.Backend())
		}
	}
}

// TestRestoreNormalizesLegacyStateKeys 旧 state.json 里的混合大小写键在恢复时归一。
//
// 升级前的持久化键是请求模型名的原始拼写。不归一会让重启后出现两套台账：
// 同一模型的冷却只挡住其中一种写法。
func TestRestoreNormalizesLegacyStateKeys(t *testing.T) {
	now := time.Now()
	future := now.Add(time.Hour)
	past := now.Add(-time.Hour)

	accounts := map[string]stateAccount{
		"trae:tr1": {
			// 旧文件里同一模型两种拼写各一条，截止不同。
			ModelCooldowns: map[string]stateModelCooldown{
				"DeepSeek-V4.1-Flash": {Until: future, Reason: "429"},
				"deepseek-v4.1-flash": {Until: now.Add(2 * time.Hour), Reason: "429"},
			},
			// 成本账本同样两种拼写，样本数不同。
			ModelCosts: map[string]stateModelCost{
				"DeepSeek-V4.1-Flash": {CostPer1k: 1.0, LastSeen: now, Samples: 1},
				"deepseek-v4.1-flash": {CostPer1k: 3.0, LastSeen: now.Add(-time.Minute), Samples: 7},
			},
		},
		// 另一个账号用来确认过期条目仍被过滤。
		"trae:tr2": {
			ModelCooldowns: map[string]stateModelCooldown{
				"Gone-Model": {Until: past, Reason: "expired"},
			},
		},
	}

	p := New("")
	p.mu.Lock()
	p.applyAccountsLocked(accounts)
	p.mu.Unlock()

	// 直接按复合键取：applyAccountsLocked 自己会建 byUID/uidIndex，
	// 若先 Add 再恢复会让 uidIndex 出现重复键，lookupLocked 判成跨上游歧义。
	p.mu.RLock()
	e1, ok := p.byUID["trae:tr1"]
	p.mu.RUnlock()
	if !ok {
		t.Fatal("tr1 未恢复")
	}

	// 冷却：归一后只剩一条，取更晚的截止（保守方向）。
	if len(e1.modelCooldowns) != 1 {
		t.Fatalf("冷却条目数 = %d, want 1（归一后不应分裂）", len(e1.modelCooldowns))
	}
	mc, exists := e1.modelCooldowns["deepseek-v4.1-flash"]
	if !exists {
		t.Fatal("冷却键未归一为小写")
	}
	if !mc.Until.Equal(now.Add(2 * time.Hour)) {
		t.Errorf("撞键时应保留更晚的截止（保守），实际 %v", mc.Until)
	}

	// 成本：撞键时保留样本数更多的观测（证据更多）。
	if len(e1.modelCost) != 1 {
		t.Fatalf("成本条目数 = %d, want 1", len(e1.modelCost))
	}
	cost := e1.modelCost["deepseek-v4.1-flash"]
	if cost.Samples != 7 {
		t.Errorf("样本数 = %d, want 7（撞键应保留样本更多的那条）", cost.Samples)
	}

	// 过期条目仍被过滤（归一不能顺手把过期项复活）。
	p.mu.RLock()
	e2, ok := p.byUID["trae:tr2"]
	p.mu.RUnlock()
	if !ok {
		t.Fatal("tr2 未恢复")
	}
	if len(e2.modelCooldowns) != 0 {
		t.Errorf("过期冷却不应恢复，实际 %d 条", len(e2.modelCooldowns))
	}
}
