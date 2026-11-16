package trae

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// region_behavior_test.go 守「国际域在运行期的行为差异」。
//
// 这些差异都是真实契约，不是可选优化：
//   - 签到：国际网关整族 404，必须返 Skipped（面板可见）而不是 ErrUnsupported；
//   - 目录缓存：两个域的阵容基本不重叠，混用会让账号看到另一个域的模型。

// 签到「国际域返回 Skipped」的行为断言在 backend 层
// （internal/backend/trae/account_test.go 的 TestCheckinSkippedForGlobalDomain）：
// 那里才有 gateway.Account 的 Checkin 实现，本包只提供域判据。

// TestCatalogCacheSeparatedByRegion 目录缓存按域分表。
//
// 改造前是一份全局单例（注释写「模型目录与账号无关」）。国际版接入后，
// 国内账号会看到国际阵容、反之亦然——两套阵容基本不重叠。
func TestCatalogCacheSeparatedByRegion(t *testing.T) {
	InvalidateCatalog()
	defer InvalidateCatalog()

	cn := &Account{UID: "cn1", AccessToken: "t", Host: "https://api.trae.cn", UserRegion: "CN"}
	sg := &Account{UID: "sg1", AccessToken: "t", Host: "https://growsg-normal.trae.ai", UserRegion: "SG"}
	cfg := &Config{}
	cfg.Normalize()

	// 两个账号必须落进不同的缓存分表。
	if k1, k2 := catalogCacheKey(cfg, cn), catalogCacheKey(cfg, sg); k1 == k2 {
		t.Fatalf("两个域的缓存键相同（%q），会让两个域共用一份目录", k1)
	}

	// 直接注入不同内容，验证互不可见。
	// FetchedAt 必须设成"刚刚"——零值会被判为过期，从而绕过缓存真的发请求。
	now := time.Now()
	globalCatalogCache.mu.Lock()
	globalCatalogCache.data["cn"] = &Catalog{Models: []CatalogModel{{ID: "glm-5.2"}}, Live: true, FetchedAt: now}
	globalCatalogCache.data["global"] = &Catalog{Models: []CatalogModel{{ID: "gpt-5.4"}}, Live: true, FetchedAt: now}
	globalCatalogCache.mu.Unlock()

	c := NewClient()
	gotCN := c.FetchCatalog(context.Background(), cfg, cn, false)
	gotSG := c.FetchCatalog(context.Background(), cfg, sg, false)
	if len(gotCN.Models) != 1 || gotCN.Models[0].ID != "glm-5.2" {
		t.Errorf("国内账号拿到 %+v, want glm-5.2", gotCN.Models)
	}
	if len(gotSG.Models) != 1 || gotSG.Models[0].ID != "gpt-5.4" {
		t.Errorf("国际账号拿到 %+v, want gpt-5.4", gotSG.Models)
	}
}

// TestChatFunctionsExcludeSoloAgent 聊天 function 表不得含 solo_agent。
//
// solo_agent 的目录条目最多，但它**不能聊天**：任何模型用它发 llm_utils_chat
// 都回 4011（dsh 2026-10-01 实测，issue #19）。目录查询可以用它，聊天不行。
func TestChatFunctionsExcludeSoloAgent(t *testing.T) {
	for _, region := range []ServiceRegion{RegionCN, RegionGlobal} {
		for _, fn := range chatFunctionOrder(region) {
			if fn == FunctionSoloAgent {
				t.Errorf("%s 的聊天 function 表含 solo_agent —— 它不能承载聊天", region)
			}
		}
	}
	// 但目录查询表**应当**含它（国际侧靠它拿全）。
	found := false
	for _, fn := range directoryFunctions {
		if fn == FunctionSoloAgent {
			found = true
		}
	}
	if !found {
		t.Error("目录查询表应含 solo_agent（国际侧 7/7 覆盖靠它）")
	}
}

// TestCatalogAssignsChatableFunction 目录归属必须收敛到能聊天的 function。
//
// 场景：solo_agent 先列出某模型（目录表里它排第一），但它不能聊天。
// 归属必须被改写成后面某个能聊天的 function，否则调用会得到 4011。
func TestCatalogAssignsChatableFunction(t *testing.T) {
	InvalidateCatalog()
	defer InvalidateCatalog()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// solo_agent 列 m1；solo_work_lite 也列 m1。前者不能聊天，后者能。
		body := string(readBodyForTest(t, r))
		switch {
		case strings.Contains(body, FunctionSoloAgent):
			_, _ = w.Write([]byte(`{"code":0,"config_info_list":[{"config_name":"m1"}]}`))
		case strings.Contains(body, FunctionSoloWorkLite):
			_, _ = w.Write([]byte(`{"code":0,"config_info_list":[{"config_name":"m1"}]}`))
		default:
			_, _ = w.Write([]byte(`{"code":0,"config_info_list":[]}`))
		}
	}))
	defer srv.Close()

	cfg := &Config{}
	cfg.Normalize()
	cfg.CN.ChatBase = srv.URL
	acc := &Account{UID: "u1", AccessToken: "t", Host: "https://api.trae.cn", UserRegion: "CN"}
	cat := NewClient().FetchCatalog(context.Background(), cfg, acc, true)
	if len(cat.Models) != 1 {
		t.Fatalf("目录条目数 = %d, want 1", len(cat.Models))
	}
	if got := cat.Models[0].Function; got == FunctionSoloAgent {
		t.Errorf("归属 = solo_agent —— 该函数不能聊天，会回 4011")
	}
	if got := cat.FunctionForConfigName("m1"); got != FunctionSoloWorkLite {
		t.Errorf("FunctionForConfigName = %q, want %q", got, FunctionSoloWorkLite)
	}
}

// readBodyForTest 读请求体（测试辅助）。
func readBodyForTest(t *testing.T, r *http.Request) []byte {
	t.Helper()
	b, _ := io.ReadAll(r.Body)
	return b
}
