package pool

import (
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/gateway"
)

// gate_model_test.go 守「跨上游模型名串号」这条防线。
//
// 场景（真实发生过）：客户端请求 "cn:hy4free"（CodeBuddy 的模型）。Trae 账号的
// realm 也是 cn，于是它被选中，打过去回「模型不存在」。日志里那条
// 「trae 账号 + cn:hy4free」就是这么来的。
//
// 判据来自池里已有的模型目录缓存（gateway.Cataloger），**只在缓存新鲜时**生效，
// 缓存冷则放行——选号是热路径，不能因一次目录拉取失败就让整池不可用。

// seedCatalog 往池的目录缓存里塞一份「新鲜」的目录（cn 域）。
func seedCatalog(p *Pool, backend string, models ...string) {
	seedCatalogRealm(p, backend, "cn", models...)
}

// seedCatalogRealm 同 seedCatalog，但指定域。
func seedCatalogRealm(p *Pool, backend, realm string, models ...string) {
	p.catalogMu.Lock()
	defer p.catalogMu.Unlock()
	if p.catalogCache == nil {
		p.catalogCache = make(map[catalogGroup]catalogCacheEntry, 4)
	}
	list := make([]gateway.CatalogModel, 0, len(models))
	for _, m := range models {
		list = append(list, gateway.CatalogModel{ID: m})
	}
	p.catalogCache[catalogGroup{backend: backend, realm: realm}] = catalogCacheEntry{
		models:  list,
		fetched: p.now(),
	}
}

// TestPickSkipsBackendWithoutModel 目录里没有该模型的上游账号必须被跳过。
func TestPickSkipsBackendWithoutModel(t *testing.T) {
	withNoPickGap(t)
	p := New("")
	p.Add(WrapAuth(&auth.Auth{UID: "cb1"})) // backend=codebuddy, realm=cn
	p.Add(fakeTraeAccount{uid: "tr1"})      // backend=trae, realm=cn

	// CodeBuddy 有 hy4free，Trae 只有自己的模型。
	seedCatalog(p, "codebuddy", "hy4free", "hy3")
	seedCatalog(p, "trae", "deepseek-v4.1-flash", "glm-5.3")

	for i := 0; i < 30; i++ {
		acc := p.PickForBackend(nil, "hy4free", "cn", "")
		if acc == nil {
			t.Fatal("应选中 CodeBuddy 账号，却没有任何候选")
		}
		if acc.Backend() != "codebuddy" {
			t.Fatalf("hy4free 被派给了 %s —— 该上游目录里没有这个模型", acc.Backend())
		}
	}
}

// TestPickAllowsModelWhenCatalogCold 缓存里没有该上游目录时放行（不能误杀）。
func TestPickAllowsModelWhenCatalogCold(t *testing.T) {
	withNoPickGap(t)
	p := New("")
	p.Add(fakeTraeAccount{uid: "tr1"})
	// 不塞任何目录缓存 → 无判据。

	if acc := p.PickForBackend(nil, "hy4free", "cn", ""); acc == nil {
		t.Fatal("目录缓存为空时应放行（否则目录拉取失败会让整池不可用）")
	}
}

// TestPickIgnoresStaleCatalog 过期目录不作为判据。
func TestPickIgnoresStaleCatalog(t *testing.T) {
	withNoPickGap(t)
	p := New("")
	p.Add(fakeTraeAccount{uid: "tr1"})

	p.catalogMu.Lock()
	p.catalogCache = map[catalogGroup]catalogCacheEntry{
		{backend: "trae", realm: "cn"}: {
			models:  []gateway.CatalogModel{{ID: "deepseek-v4.1-flash"}},
			fetched: p.now().Add(-catalogTTL - time.Minute), // 已过期
		},
	}
	p.catalogMu.Unlock()

	if acc := p.PickForBackend(nil, "hy4free", "cn", ""); acc == nil {
		t.Fatal("过期目录不应导致排除（上游可能刚新增了该模型）")
	}
}

// TestBackendServesModelTriState 直接断言三态语义。
func TestBackendServesModelTriState(t *testing.T) {
	p := New("")
	seedCatalog(p, "trae", "glm-5.3")

	p.mu.RLock()
	defer p.mu.RUnlock()

	if !p.backendServesModelLocked("trae", "cn", "glm-5.3") {
		t.Error("目录里有该模型 → 应为 true")
	}
	if p.backendServesModelLocked("trae", "cn", "hy4free") {
		t.Error("目录里没有该模型 → 应为 false")
	}
	if !p.backendServesModelLocked("codebuddy", "cn", "hy4free") {
		t.Error("没有该上游的任何目录 → 应放行（true）")
	}
	if !p.backendServesModelLocked("trae", "cn", "") {
		t.Error("空模型名 → 应放行（true）")
	}
}

// TestBackendServesModelCaseInsensitive 大小写不同的写法必须视为同一模型。
//
// 真实场景：Trae 目录里是 `DeepSeek-V4.1-Flash`，客户端写全小写。精确比对会
// 判成「该上游没有此模型」，把唯一可用的号静默排除 → 503 no_healthy_account，
// 报错完全不指向大小写。
func TestBackendServesModelCaseInsensitive(t *testing.T) {
	p := New("")
	seedCatalog(p, "trae", "DeepSeek-V4.1-Flash")

	p.mu.RLock()
	defer p.mu.RUnlock()

	// 目录里是混合大小写，客户端各种写法都要命中。
	for _, variant := range []string{
		"DeepSeek-V4.1-Flash", "deepseek-v4.1-flash", "DEEPSEEK-V4.1-FLASH",
		"  deepseek-v4.1-flash  ", // 前后空白也要归一（TrimSpace）
	} {
		if !p.backendServesModelLocked("trae", "cn", variant) {
			t.Errorf("%q 应命中目录里的 DeepSeek-V4.1-Flash", variant)
		}
	}
	// 真的不存在的模型仍要排除（归一不能变成「一律放行」）。
	if p.backendServesModelLocked("trae", "cn", "deepseek-v4.1-pro") {
		t.Error("不存在的模型应被排除")
	}
}

// TestBackendServesModelRealmScoped 目录判据必须按 (上游, 域) 分开。
//
// 真实场景：Trae 国内目录有 glm-5.2，国际目录没有。请求 trae:global:glm-5.2 时
// realm 已把候选限死在 global，但只按上游查目录会让**国内**目录替 global 账号
// 「作证」——账号进候选，打到国际网关报「模型不存在」，还可能被记一次失败。
func TestBackendServesModelRealmScoped(t *testing.T) {
	p := New("")
	seedCatalogRealm(p, "trae", "cn", "glm-5.2", "deepseek-v4.1-flash")
	seedCatalogRealm(p, "trae", "global", "gpt-5.4", "gemini-3-pro")

	p.mu.RLock()
	defer p.mu.RUnlock()

	// 国内模型不能替国际账号作证。
	if p.backendServesModelLocked("trae", "global", "glm-5.2") {
		t.Error("global 账号不该被 cn 目录里的 glm-5.2 放行")
	}
	// 反向同理。
	if p.backendServesModelLocked("trae", "cn", "gpt-5.4") {
		t.Error("cn 账号不该被 global 目录里的 gpt-5.4 放行")
	}
	// 各自域内正常命中。
	if !p.backendServesModelLocked("trae", "cn", "glm-5.2") {
		t.Error("cn 账号应命中 cn 目录里的 glm-5.2")
	}
	if !p.backendServesModelLocked("trae", "global", "gpt-5.4") {
		t.Error("global 账号应命中 global 目录里的 gpt-5.4")
	}
	// 两域共有的模型（如国际也上了 deepseek）互不干扰。
	if p.backendServesModelLocked("trae", "global", "deepseek-v4.1-flash") {
		t.Error("global 目录里没有 deepseek-v4.1-flash，不该放行")
	}
	// realm 为空 = 不分域（旧语义，调用方没给域上下文时不该额外收紧）。
	if !p.backendServesModelLocked("trae", "", "glm-5.2") {
		t.Error("realm 为空时应退化为按上游查")
	}
	// 另一域全无目录时放行（三态语义不变：无判据 ≠ 没有该模型）。
	if !p.backendServesModelLocked("trae", "us", "glm-5.2") {
		t.Error("该域没有任何目录 → 应放行")
	}
}
