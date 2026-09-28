package panel

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/thirdparty"
)

// TestThirdPartyConfigAPI 覆盖面板「上游接入」页接口：
// 缺文件回默认值、合法保存落盘、非法拒绝、鉴权、热重载钩子。
func TestThirdPartyConfigAPI(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "third_party.json")
	reloaded := 0
	p := New(Config{
		Version:        "test",
		APIKey:         "test-key",
		ThirdPartyPath: path,
		ReloadThirdParty: func() error {
			reloaded++
			return nil
		},
	})

	do := func(method, body string, auth bool) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, "/panel/api/thirdparty_config", bytes.NewReader([]byte(body)))
		if auth {
			req.Header.Set("Authorization", "Bearer test-key")
		}
		rec := httptest.NewRecorder()
		p.ServeHTTP(rec, req)
		return rec
	}

	// 1. 文件不存在 → 默认值（enabled 缺省 false：第三方需显式填地址，默认关闭更安全）。
	rec := do("GET", "", true)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET code=%d body=%s", rec.Code, rec.Body)
	}
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("GET not json: %v", err)
	}
	cfg, _ := got["config"].(map[string]any)
	if cfg["enabled"] != false {
		t.Errorf("默认 enabled=%v, want false", cfg["enabled"])
	}

	// 2. 未鉴权 → 401。
	if rec := do("GET", "", false); rec.Code != http.StatusUnauthorized {
		t.Fatalf("无鉴权 GET code=%d, want 401", rec.Code)
	}

	// 3. 非法配置（base_url 无 scheme）→ 400 且不落盘。
	bad := `{"enabled":true,"providers":[{"name":"x","base_url":"api.example.com"}]}`
	if rec := do("POST", bad, true); rec.Code != http.StatusBadRequest {
		t.Fatalf("非法配置 code=%d body=%s, want 400", rec.Code, rec.Body)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("非法配置不应落盘")
	}

	// 4. 名称重复 → 400（thirdparty.Config.Validate 与启动期同口径）。
	dup := `{"enabled":true,"providers":[
		{"name":"x","base_url":"https://a.example/v1"},
		{"name":"x","base_url":"https://b.example/v1"}]}`
	if rec := do("POST", dup, true); rec.Code != http.StatusBadRequest {
		t.Fatalf("重复名称 code=%d, want 400", rec.Code)
	}

	// 5. 合法保存 → 落盘 + 触发热重载。
	ok := `{"enabled":true,"providers":[{"name":"deepseek","base_url":"https://api.deepseek.com/v1","api_key":"sk-1","models":["deepseek-chat"]}]}`
	rec = do("POST", ok, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("保存 code=%d body=%s", rec.Code, rec.Body)
	}
	var saveResp map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &saveResp)
	if n, _ := saveResp["restart_required"].([]any); len(n) != 0 {
		t.Errorf("注入 ReloadThirdParty 后不应要求重启: %v", saveResp)
	}
	if reloaded != 1 {
		t.Errorf("热重载调用次数 = %d, want 1", reloaded)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读回落盘文件: %v", err)
	}
	var saved thirdparty.Config
	if err := json.Unmarshal(raw, &saved); err != nil {
		t.Fatalf("落盘内容非合法配置: %v\n%s", err, raw)
	}
	if !saved.Enabled || len(saved.Providers) != 1 || saved.Providers[0].Name != "deepseek" {
		t.Fatalf("落盘内容不符: %+v", saved)
	}

	// 6. 读回：字段顺序与未知字段保留（写盘用原始 body，不做结构体重序列化）。
	rec = do("GET", "", true)
	var back map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &back); err != nil {
		t.Fatal(err)
	}
	backCfg, _ := back["config"].(map[string]any)
	if backCfg["enabled"] != true {
		t.Errorf("读回 enabled=%v, want true", backCfg["enabled"])
	}
}

// TestThirdPartyConfigUnavailable 未注入路径时返回 501（接口存在但不可用）。
func TestThirdPartyConfigUnavailable(t *testing.T) {
	p := New(Config{Version: "test"})
	req := httptest.NewRequest(http.MethodGet, "/panel/api/thirdparty_config", nil)
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("code=%d, want 501", rec.Code)
	}
}

// TestThirdPartyTestProvider 覆盖「测试连接」：成功 / 上游 4xx / 未注入 client。
func TestThirdPartyTestProvider(t *testing.T) {
	okSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"pong"}}]}`))
	}))
	defer okSrv.Close()

	p := New(Config{Version: "test", ThirdPartyClient: thirdparty.NewClient()})
	do := func(body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/panel/api/thirdparty_test", bytes.NewReader([]byte(body)))
		rec := httptest.NewRecorder()
		p.ServeHTTP(rec, req)
		return rec
	}

	// 成功：provider 只需 name/base_url/models（其余缺省由 Validate 补齐）。
	rec := do(`{"provider":{"name":"fake","base_url":"` + okSrv.URL + `/v1","models":["m1"]}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("探活 code=%d body=%s", rec.Code, rec.Body)
	}
	var okResp map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &okResp)
	if okResp["ok"] != true {
		t.Fatalf("探活应成功: %s", rec.Body)
	}

	// 未声明 models 的兜底 provider 无法探活 → ok:false（附原因）。
	rec = do(`{"provider":{"name":"fake","base_url":"` + okSrv.URL + `/v1"}}`)
	var noModel map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &noModel)
	if noModel["ok"] != false {
		t.Fatalf("无 models 应返回 ok:false: %s", rec.Body)
	}

	// 非法 provider（base_url 无 scheme）→ 400。
	if rec := do(`{"provider":{"name":"x","base_url":"nope"}}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("非法 provider code=%d, want 400", rec.Code)
	}

	// 未注入 client → 501。
	p2 := New(Config{Version: "test"})
	req := httptest.NewRequest(http.MethodPost, "/panel/api/thirdparty_test", bytes.NewReader([]byte(`{}`)))
	rec2 := httptest.NewRecorder()
	p2.ServeHTTP(rec2, req)
	if rec2.Code != http.StatusNotImplemented {
		t.Fatalf("无 client code=%d, want 501", rec2.Code)
	}
}
