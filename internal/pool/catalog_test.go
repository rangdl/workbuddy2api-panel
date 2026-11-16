package pool

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/gateway"
)

// catalogAccount 实现 gateway.Cataloger 的测试账号。
type catalogAccount struct {
	capableAccount
	models []gateway.CatalogModel
	err    error
	calls  *atomic.Int32
}

func (a catalogAccount) Catalog(ctx context.Context) ([]gateway.CatalogModel, error) {
	if a.calls != nil {
		a.calls.Add(1)
	}
	if a.err != nil {
		return nil, a.err
	}
	return a.models, nil
}

func (a catalogAccount) RateFor(ctx context.Context, model string) (float64, bool) {
	for _, m := range a.models {
		if m.ID == model && m.Credits != "" {
			return 0.15, true
		}
	}
	return 0, false
}

// 池的目录调度：每个上游挑**一个**健康号拉一次，不遍历全部账号。
func TestCatalogAllPicksOneAccountPerBackend(t *testing.T) {
	p := New("")
	var a1, a2 atomic.Int32
	p.Add(catalogAccount{capableAccount: capableAccount{uid: "u1", backend: "codebuddy"},
		models: []gateway.CatalogModel{{ID: "m1"}}, calls: &a1})
	p.Add(catalogAccount{capableAccount: capableAccount{uid: "u2", backend: "codebuddy"},
		models: []gateway.CatalogModel{{ID: "m1"}}, calls: &a2})

	res := p.CatalogAll(context.Background())
	if len(res) != 1 {
		t.Fatalf("groups = %d, want 1: %+v", len(res), res)
	}
	if res[0].Backend != "codebuddy" || len(res[0].Models) != 1 {
		t.Fatalf("unexpected result: %+v", res[0])
	}
	// 关键：不是 2。目录是上游级数据，遍历全部号等于把同一个请求打两遍。
	if got := a1.Load() + a2.Load(); got != 1 {
		t.Errorf("upstream catalog calls = %d, want 1（每上游只挑一个号）", got)
	}
}

// 健康号优先：冷却中的号即使排在前面也不该被选中。
func TestCatalogCandidatesHealthFirst(t *testing.T) {
	p := New("")
	var sickCalls, healthyCalls atomic.Int32
	p.Add(catalogAccount{capableAccount: capableAccount{uid: "sick", backend: "codebuddy"},
		models: []gateway.CatalogModel{{ID: "from-sick"}}, calls: &sickCalls})
	p.Add(catalogAccount{capableAccount: capableAccount{uid: "ok", backend: "codebuddy"},
		models: []gateway.CatalogModel{{ID: "from-ok"}}, calls: &healthyCalls})
	// 把 sick 号打进长冷却。
	p.Cooldown("sick", CoolSoft, time.Hour, "test")

	res := p.CatalogAll(context.Background())
	if len(res) != 1 || len(res[0].Models) != 1 {
		t.Fatalf("unexpected: %+v", res)
	}
	if res[0].Models[0].ID != "from-ok" {
		t.Errorf("模型来自 %q，应优先使用未冷却的号", res[0].Models[0].ID)
	}
	if sickCalls.Load() != 0 {
		t.Errorf("冷却号不应被调用，got %d", sickCalls.Load())
	}
}

// 失败不罚号：目录拉取失败不得让账号进冷却/熔断（models ≠ chat 不可用）。
func TestCatalogFailureDoesNotPenalizeAccount(t *testing.T) {
	p := New("")
	p.Add(catalogAccount{capableAccount: capableAccount{uid: "u1", backend: "codebuddy"},
		err: errors.New("boom")})

	res := p.CatalogAll(context.Background())
	if len(res) != 1 || res[0].Err == nil {
		t.Fatalf("应回传错误: %+v", res)
	}
	st, ok := p.Status("u1")
	if !ok {
		t.Fatal("账号应从池快照里可见")
	}
	if st.Cooling {
		t.Errorf("目录拉取失败不该罚号: %+v", st)
	}
}

// 缓存：TTL 内的第二次调用不再打上游；负缓存期内的失败也不重试。
func TestCatalogCacheAndNegativeCache(t *testing.T) {
	p := New("")
	var calls atomic.Int32
	p.Add(catalogAccount{capableAccount: capableAccount{uid: "u1", backend: "codebuddy"},
		models: []gateway.CatalogModel{{ID: "m1"}}, calls: &calls})

	now := time.Now()
	p.SetCatalogClock(func() time.Time { return now })

	p.CatalogAll(context.Background())
	p.CatalogAll(context.Background())
	if got := calls.Load(); got != 1 {
		t.Errorf("TTL 内应命中缓存，upstream calls = %d, want 1", got)
	}

	// 过 TTL → 重新拉。
	now = now.Add(catalogTTL + time.Second)
	p.CatalogAll(context.Background())
	if got := calls.Load(); got != 2 {
		t.Errorf("TTL 过期后应重新拉取，calls = %d, want 2", got)
	}
}

func TestCatalogNegativeCache(t *testing.T) {
	p := New("")
	var calls atomic.Int32
	p.Add(catalogAccount{capableAccount: capableAccount{uid: "u1", backend: "codebuddy"},
		err: errors.New("boom"), calls: &calls})

	now := time.Now()
	p.SetCatalogClock(func() time.Time { return now })

	p.CatalogAll(context.Background())
	p.CatalogAll(context.Background())
	if got := calls.Load(); got != 1 {
		t.Errorf("负缓存期内不该重试，calls = %d, want 1", got)
	}

	// 负缓存过期 → 重新尝试（仍失败）。
	now = now.Add(catalogFailCooldown + time.Second)
	p.CatalogAll(context.Background())
	if got := calls.Load(); got != 2 {
		t.Errorf("负缓存过期后应重试，calls = %d, want 2", got)
	}
}

// 兜底：账号未实现 Cataloger 时用注入的兜底，且只试一次。
func TestCatalogFallbackTriedOnce(t *testing.T) {
	p := New("")
	// 两个账号都不实现 Cataloger（capableAccount 本身没有 Catalog 方法）。
	p.Add(capableAccount{uid: "u1", backend: "codebuddy"})
	p.Add(capableAccount{uid: "u2", backend: "codebuddy"})

	var calls atomic.Int32
	p.SetCatalogFallback(func(context.Context, string, gateway.Account) ([]gateway.CatalogModel, error) {
		calls.Add(1)
		return []gateway.CatalogModel{{ID: "fallback-model"}}, nil
	})

	res := p.CatalogAll(context.Background())
	if len(res) != 1 || len(res[0].Models) != 1 || res[0].Models[0].ID != "fallback-model" {
		t.Fatalf("兜底未生效: %+v", res)
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("兜底应只试一次（上游级数据），calls = %d", got)
	}
}

// 无兜底 + 账号不实现 Cataloger → ErrUnsupported（不是失败，调用方静默跳过）。
func TestCatalogNoFallbackUnsupported(t *testing.T) {
	p := New("")
	p.Add(capableAccount{uid: "u1", backend: "codebuddy"})

	res := p.CatalogAll(context.Background())
	if len(res) != 1 {
		t.Fatalf("groups = %d", len(res))
	}
	if !errors.Is(res[0].Err, gateway.ErrUnsupported) {
		t.Errorf("err = %v, want ErrUnsupported", res[0].Err)
	}
}

// CachedCatalog 只读缓存，不触发上游拉取（错误路径用）。
func TestCachedCatalogDoesNotFetch(t *testing.T) {
	p := New("")
	var calls atomic.Int32
	p.Add(catalogAccount{capableAccount: capableAccount{uid: "u1", backend: "codebuddy"},
		models: []gateway.CatalogModel{{ID: "m1"}}, calls: &calls})

	if got := p.CachedCatalog("codebuddy"); len(got) != 0 {
		t.Errorf("冷缓存应返回空，got %v", got)
	}
	if calls.Load() != 0 {
		t.Errorf("CachedCatalog 不得触发上游拉取，calls = %d", calls.Load())
	}

	p.CatalogAll(context.Background())
	if got := p.CachedCatalog("codebuddy"); len(got) != 1 {
		t.Errorf("预热后应能读到缓存，got %v", got)
	}
	if calls.Load() != 1 {
		t.Errorf("calls = %d, want 1（读缓存不该再拉）", calls.Load())
	}
}

// ModelRateFor 转到账号自己的实现；未实现 ModelRate 的账号返回 false。
func TestModelRateFor(t *testing.T) {
	p := New("")
	withRate := catalogAccount{capableAccount: capableAccount{uid: "u1", backend: "codebuddy"},
		models: []gateway.CatalogModel{{ID: "m1", Credits: "x0.15"}}}
	p.Add(withRate)
	if rate, ok := p.ModelRateFor(context.Background(), withRate, "m1"); !ok || rate != 0.15 {
		t.Errorf("rate = %v ok=%v, want 0.15/true", rate, ok)
	}
	if _, ok := p.ModelRateFor(context.Background(), withRate, "nope"); ok {
		t.Error("未知模型应返回 false")
	}

	noRate := capableAccount{uid: "u2", backend: "codebuddy"}
	p.Add(noRate)
	if _, ok := p.ModelRateFor(context.Background(), noRate, "m1"); ok {
		t.Error("未实现 ModelRate 的账号应返回 false")
	}
}
