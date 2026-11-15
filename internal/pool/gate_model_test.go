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

// seedCatalog 往池的目录缓存里塞一份「新鲜」的目录。
func seedCatalog(p *Pool, backend string, models ...string) {
	p.catalogMu.Lock()
	defer p.catalogMu.Unlock()
	if p.catalogCache == nil {
		p.catalogCache = make(map[catalogGroup]catalogCacheEntry, 4)
	}
	list := make([]gateway.CatalogModel, 0, len(models))
	for _, m := range models {
		list = append(list, gateway.CatalogModel{ID: m})
	}
	p.catalogCache[catalogGroup{backend: backend, realm: "cn"}] = catalogCacheEntry{
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

	if !p.backendServesModelLocked("trae", "glm-5.3") {
		t.Error("目录里有该模型 → 应为 true")
	}
	if p.backendServesModelLocked("trae", "hy4free") {
		t.Error("目录里没有该模型 → 应为 false")
	}
	if !p.backendServesModelLocked("codebuddy", "hy4free") {
		t.Error("没有该上游的任何目录 → 应放行（true）")
	}
	if !p.backendServesModelLocked("trae", "") {
		t.Error("空模型名 → 应放行（true）")
	}
}
