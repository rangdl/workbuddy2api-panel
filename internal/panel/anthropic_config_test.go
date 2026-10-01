package panel

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// TestAnthropicConfigAPI 覆盖面板 Anthropic 配置页接口：
// 缺文件回默认值（enabled=false）、合法保存落盘 + 热重载、非法拒绝、鉴权。
func TestAnthropicConfigAPI(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "anthropic.json")
	reloaded := false
	p := New(Config{
		Version:         "test",
		APIKey:          "test-key",
		AnthropicPath:   path,
		ReloadAnthropic: func() error { reloaded = true; return nil },
	})

	do := func(method, body string, auth bool) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, "/panel/api/anthropic_config", bytes.NewReader([]byte(body)))
		if auth {
			req.Header.Set("Authorization", "Bearer test-key")
		}
		rec := httptest.NewRecorder()
		p.ServeHTTP(rec, req)
		return rec
	}

	// 1. 文件不存在 → 返回默认值（enabled 缺省 false——CC 接入默认禁用）。
	rec := do("GET", "", true)
	if rec.Code != 200 {
		t.Fatalf("GET code=%d body=%s", rec.Code, rec.Body)
	}
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("GET not json: %v", err)
	}
	cfg, _ := got["config"].(map[string]any)
	if cfg["enabled"] != false {
		t.Errorf("default enabled=%v, want false", cfg["enabled"])
	}
	// 返回 claude_models 列表（供前端预填左列）。
	if models, ok := got["claude_models"].([]any); !ok || len(models) == 0 {
		t.Errorf("claude_models missing or empty: %v", got["claude_models"])
	}

	// 2. 合法保存 → 200 且落盘 + 热重载被触发。
	body := `{"enabled":true,"model_map":{"claude-sonnet-4-5":"glm-5.2"},"default_model":"glm-5.2"}`
	if rec := do("POST", body, true); rec.Code != 200 {
		t.Fatalf("POST code=%d body=%s", rec.Code, rec.Body)
	}
	if !reloaded {
		t.Error("ReloadAnthropic should be invoked after save")
	}
	disk, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	var saved map[string]any
	json.Unmarshal(disk, &saved)
	if saved["enabled"] != true {
		t.Errorf("saved enabled=%v", saved["enabled"])
	}
	mm, _ := saved["model_map"].(map[string]any)
	if mm["claude-sonnet-4-5"] != "glm-5.2" {
		t.Errorf("saved model_map=%v", saved["model_map"])
	}

	// 3. 非法值（model_map 值非字符串）→ 400 且不落盘。
	reloaded = false
	bad := `{"enabled":true,"model_map":{"claude-sonnet-4-5":123}}`
	if rec := do("POST", bad, true); rec.Code != 400 {
		t.Fatalf("bad POST code=%d body=%s", rec.Code, rec.Body)
	}
	if reloaded {
		t.Error("rejected save must not trigger reload")
	}

	// 4. 未鉴权 → 401。
	if rec := do("GET", "", false); rec.Code != 401 {
		t.Errorf("unauth code=%d", rec.Code)
	}
}
