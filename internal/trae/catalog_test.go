package trae

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// newCatalogServer 假模型名册：只有指定 function 返回模型。
func newCatalogServer(t *testing.T, byFunction map[string]string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != EndpointGetDetailParamPath {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		fn, _ := body["function"].(string)
		w.Header().Set("Content-Type", "application/json")
		resp, ok := byFunction[fn]
		if !ok {
			_, _ = w.Write([]byte(`{"code":0,"config_info_list":[]}`))
			return
		}
		_, _ = w.Write([]byte(resp))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestFetchCatalogMergesFunctions 多个 function 的模型合并去重，
// 且 function 归属取「第一个列出它的」。
func TestFetchCatalogMergesFunctions(t *testing.T) {
	InvalidateCatalog()
	srv := newCatalogServer(t, map[string]string{
		FunctionSoloWorkLite: `{"code":0,"config_info_list":[
			{"config_name":"glm-5.3","display_config":{"display_name":"GLM-5.3"},
			 "model_detail_list":[{"max_tokens":8192,"prompt_max_tokens":200000}]},
			{"config_name":"kimi-k3"}]}`,
		FunctionSoloAgent: `{"code":0,"config_info_list":[
			{"config_name":"kimi-k3"},
			{"config_name":"deepseek-v4-pro"}]}`,
	})

	cfg := &Config{ChatBase: srv.URL, AppID: DefaultAppID,
		IDEVersion: DefaultIDEVersion, IDEVersionCode: DefaultIDEVersionCode}
	acc := &Account{UID: "u1", AccessToken: "t1", DeviceID: "1234567890123456"}

	cat := NewClient().FetchCatalog(context.Background(), cfg, acc, true)
	if cat == nil || !cat.Live {
		t.Fatalf("应拿到上游目录: %+v", cat)
	}
	if len(cat.Models) != 3 {
		t.Fatalf("模型数 = %d, want 3（去重后）: %+v", len(cat.Models), cat.Models)
	}
	// kimi-k3 被两个 function 列出，归属应是最先询问的 solo_work_lite。
	if fn := cat.FunctionForConfigName("kimi-k3"); fn != FunctionSoloWorkLite {
		t.Errorf("kimi-k3 function = %q, want %q", fn, FunctionSoloWorkLite)
	}
	if fn := cat.FunctionForConfigName("deepseek-v4-pro"); fn != FunctionSoloAgent {
		t.Errorf("deepseek-v4-pro function = %q", fn)
	}
	// 元数据解析。
	for _, m := range cat.Models {
		if m.ID == "glm-5.3" {
			if m.DisplayName != "GLM-5.3" {
				t.Errorf("DisplayName = %q", m.DisplayName)
			}
			if m.MaxTokens != 8192 || m.PromptMaxTokens != 200000 {
				t.Errorf("上限解析不符: %+v", m)
			}
		}
	}
}

// TestFetchCatalogFallbackToBuiltin 上游全失败时回落到内置目录，
// 且不标记 Live。
func TestFetchCatalogFallbackToBuiltin(t *testing.T) {
	InvalidateCatalog()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	cfg := &Config{ChatBase: srv.URL, AppID: DefaultAppID,
		IDEVersion: DefaultIDEVersion, IDEVersionCode: DefaultIDEVersionCode}
	acc := &Account{UID: "u1", AccessToken: "t1"}

	cat := NewClient().FetchCatalog(context.Background(), cfg, acc, true)
	if cat == nil {
		t.Fatal("应返回内置目录而非 nil")
	}
	if cat.Live {
		t.Error("失败时不应标记 Live")
	}
	if len(cat.Models) != len(KnownModels()) {
		t.Errorf("内置目录模型数 = %d, want %d", len(cat.Models), len(KnownModels()))
	}
}

// TestFetchCatalogSkipsPlaceholders 占位行（空 / "-" / 纯下划线）被过滤。
func TestFetchCatalogSkipsPlaceholders(t *testing.T) {
	InvalidateCatalog()
	srv := newCatalogServer(t, map[string]string{
		FunctionSoloWorkLite: `{"code":0,"config_info_list":[
			{"config_name":""},{"config_name":"-"},{"config_name":"__"},{"config_name":"real-model"}]}`,
	})
	cfg := &Config{ChatBase: srv.URL, AppID: DefaultAppID,
		IDEVersion: DefaultIDEVersion, IDEVersionCode: DefaultIDEVersionCode}
	cat := NewClient().FetchCatalog(context.Background(), cfg,
		&Account{UID: "u1", AccessToken: "t1"}, true)
	if len(cat.Models) != 1 || cat.Models[0].ID != "real-model" {
		t.Errorf("占位行未过滤: %+v", cat.Models)
	}
}

// TestFetchCatalogCached 第二次调用走缓存（不重复打上游）。
func TestFetchCatalogCached(t *testing.T) {
	InvalidateCatalog()
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":0,"config_info_list":[{"config_name":"m1"}]}`))
	}))
	defer srv.Close()

	cfg := &Config{ChatBase: srv.URL, AppID: DefaultAppID,
		IDEVersion: DefaultIDEVersion, IDEVersionCode: DefaultIDEVersionCode}
	acc := &Account{UID: "u1", AccessToken: "t1"}
	c := NewClient()
	c.FetchCatalog(context.Background(), cfg, acc, true)
	first := hits
	c.FetchCatalog(context.Background(), cfg, acc, false)
	if hits != first {
		t.Errorf("缓存未生效：第二次又打了 %d 次上游", hits-first)
	}
	// force=true 必须绕过缓存。
	c.FetchCatalog(context.Background(), cfg, acc, true)
	if hits == first {
		t.Error("force=true 应绕过缓存")
	}
}

// TestPrepareChatBodyExplicitFunction 显式 function 覆盖硬编码规则。
func TestPrepareChatBodyExplicitFunction(t *testing.T) {
	src := []byte(`{"model":"glm-5.3","messages":[{"role":"user","content":"hi"}]}`)
	out, err := PrepareChatBody(src, PrepareOptions{UID: "u1", Function: "chat_v3"})
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	_ = json.Unmarshal(out, &body)
	if body["function"] != "chat_v3" {
		t.Errorf("function = %v, want chat_v3（显式覆盖）", body["function"])
	}
}
