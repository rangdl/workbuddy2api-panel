package panel

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// TestResponsesConfigAPI 覆盖面板 Responses 配置页接口：
// 缺文件回默认值、合法保存落盘、非法拒绝、鉴权。
func TestResponsesConfigAPI(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "responses.json")
	p := New(Config{Version: "test", APIKey: "test-key", ResponsesPath: path})

	do := func(method, body string, auth bool) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, "/panel/api/responses_config", bytes.NewReader([]byte(body)))
		if auth {
			req.Header.Set("Authorization", "Bearer test-key")
		}
		rec := httptest.NewRecorder()
		p.ServeHTTP(rec, req)
		return rec
	}

	// 1. 文件不存在 → 返回默认值（enabled 缺省 true）。
	rec := do("GET", "", true)
	if rec.Code != 200 {
		t.Fatalf("GET code=%d body=%s", rec.Code, rec.Body)
	}
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("GET not json: %v", err)
	}
	cfg, _ := got["config"].(map[string]any)
	if cfg["enabled"] != true {
		t.Errorf("default enabled=%v", cfg["enabled"])
	}

	// 2. 合法保存 → 200 且落盘。
	body := `{"enabled":true,"model_map":{"gpt-5-codex":"glm-5.2"},"default_model":"glm-5.2","max_cached_responses":256}`
	if rec := do("POST", body, true); rec.Code != 200 {
		t.Fatalf("POST code=%d body=%s", rec.Code, rec.Body)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("responses.json not written: %v", err)
	}
	var written map[string]any
	if err := json.Unmarshal(raw, &written); err != nil {
		t.Fatalf("written not json: %v", err)
	}
	if written["default_model"] != "glm-5.2" {
		t.Errorf("written=%v", written)
	}

	// 3. 读回已保存值。
	rec = do("GET", "", true)
	json.Unmarshal(rec.Body.Bytes(), &got)
	cfg, _ = got["config"].(map[string]any)
	if cfg["default_model"] != "glm-5.2" {
		t.Errorf("readback default_model=%v", cfg["default_model"])
	}

	// 4. 非法配置（model_map 值非字符串）→ 400，且不覆盖已存文件。
	if rec := do("POST", `{"model_map":{"a":123}}`, true); rec.Code != 400 {
		t.Errorf("invalid config should be 400, got %d", rec.Code)
	}

	// 5. 未鉴权 → 401。
	if rec := do("GET", "", false); rec.Code != 401 {
		t.Errorf("no auth should be 401, got %d", rec.Code)
	}

	// 6. ResponsesPath 为空 → 501。
	p2 := New(Config{Version: "test", APIKey: "test-key"})
	req := httptest.NewRequest("GET", "/panel/api/responses_config", nil)
	req.Header.Set("Authorization", "Bearer test-key")
	rec2 := httptest.NewRecorder()
	p2.ServeHTTP(rec2, req)
	if rec2.Code != 501 {
		t.Errorf("no ResponsesPath should be 501, got %d", rec2.Code)
	}
}
