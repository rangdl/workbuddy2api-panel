package panel

import (
	"encoding/json"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/trae"
)

// newTraePanelWithStats 构造带运行期计数表的面板。
func newTraePanelWithStats(t *testing.T, dir string) (*Panel, string, *trae.Stats) {
	t.Helper()
	path := filepath.Join(dir, "trae.json")
	stats := trae.NewStats()
	p := New(Config{
		Version:       "test",
		APIKey:        "test-key",
		TraePath:      path,
		TraeClient:    trae.NewClient(),
		TraeCooldowns: trae.NewCooldowns(),
		TraeStats:     stats,
	})
	return p, path, stats
}

// TestTraeConfigIncludesStats 账号视图应带运行期计数（成功/失败/在途/用量）。
func TestTraeConfigIncludesStats(t *testing.T) {
	dir := t.TempDir()
	p, path, stats := newTraePanelWithStats(t, dir)
	writeTraeTestConfig(t, path, &trae.Config{
		Enabled: true,
		Accounts: []trae.Account{
			{UID: "u1", Name: "甲", AccessToken: "tok-1", DeviceID: "1234567890123456"},
		},
	})

	stats.Begin("u1")(true, trae.UsageDelta{
		Model:               "glm-5.3",
		HasPromptTokens:     true,
		PromptTokens:        100,
		HasCompletionTokens: true,
		CompletionTokens:    200,
		HasTotalTokens:      true,
		TotalTokens:         300,
		HasLatencyMs:        true,
		LatencyMs:           850,
	})

	rec := traeDo(p, http.MethodGet, "/panel/api/trae_config?no_checkin=1", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Config struct {
			Accounts []struct {
				UID   string         `json:"uid"`
				Stats *trae.StatView `json:"stats"`
			} `json:"accounts"`
		} `json:"config"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Config.Accounts) != 1 {
		t.Fatalf("账号数 = %d", len(resp.Config.Accounts))
	}
	st := resp.Config.Accounts[0].Stats
	if st == nil {
		t.Fatal("stats 字段缺失")
	}
	if st.SuccessCount != 1 || st.ErrTotal != 0 {
		t.Errorf("成功/失败 = %d/%d", st.SuccessCount, st.ErrTotal)
	}
	if st.InFlight != 0 {
		t.Errorf("在途 = %d, want 0", st.InFlight)
	}
	if st.TokenUsage.TotalTokens != 300 {
		t.Errorf("total_tokens = %d, want 300", st.TokenUsage.TotalTokens)
	}
	if st.LastLatencyMs != 850 {
		t.Errorf("last_latency_ms = %d, want 850", st.LastLatencyMs)
	}
	if st.TokenUsage.LastModel != "glm-5.3" {
		t.Errorf("last_model = %q", st.TokenUsage.LastModel)
	}
}

// TestTraeConfigStatsAbsentWhenNotInjected 未注入统计表时不输出 stats 字段
// （老部署升级上来后行为不变）。
func TestTraeConfigStatsAbsentWhenNotInjected(t *testing.T) {
	dir := t.TempDir()
	p, path := newTraePanel(t, dir) // 不带 TraeStats
	writeTraeTestConfig(t, path, &trae.Config{
		Enabled:  true,
		Accounts: []trae.Account{{UID: "u1", AccessToken: "tok-1"}},
	})
	rec := traeDo(p, http.MethodGet, "/panel/api/trae_config?no_checkin=1", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var raw map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &raw)
	cfg, _ := raw["config"].(map[string]any)
	accs, _ := cfg["accounts"].([]any)
	if len(accs) != 1 {
		t.Fatalf("账号数 = %d", len(accs))
	}
	acc := accs[0].(map[string]any)
	if _, ok := acc["stats"]; ok {
		t.Error("未注入统计表时不应输出 stats")
	}
}

// TestTraeCreditsWritesBackToStats 面板「积分」查询后，统计表应拿到权威数据
// （含额度上限），下一次拉配置时可直接展示而不必再打上游。
func TestTraeCreditsWritesBackToStats(t *testing.T) {
	dir := t.TempDir()
	_, path, stats := newTraePanelWithStats(t, dir)
	writeTraeTestConfig(t, path, &trae.Config{
		Enabled:  true,
		Accounts: []trae.Account{{UID: "u1", AccessToken: "tok-1"}},
	})
	// 直接写统计表，模拟一次成功的积分查询（上游调用由 trae 包测试覆盖）。
	stats.NoteCredits("u1", 1200, 300, 1500, 6000)

	v := stats.Get("u1")
	if v.Credits != 1500 || v.CreditsLimit != 6000 {
		t.Fatalf("积分回写不符: %+v", v)
	}
	if v.CreditsIDE != 1200 || v.CreditsWork != 300 {
		t.Errorf("分解不符: ide=%v work=%v", v.CreditsIDE, v.CreditsWork)
	}
	if v.CreditsAt == nil || time.Since(*v.CreditsAt) > time.Minute {
		t.Error("CreditsAt 应为刚刚")
	}
}

// TestTraeRemoveAccountResetsStats 删除账号时运行期计数一并清空，
// 否则同 uid 重新登录会看到上一轮的历史数字。
func TestTraeRemoveAccountResetsStats(t *testing.T) {
	dir := t.TempDir()
	p, path, stats := newTraePanelWithStats(t, dir)
	writeTraeTestConfig(t, path, &trae.Config{
		Enabled:  true,
		Accounts: []trae.Account{{UID: "u1", AccessToken: "tok-1"}},
	})
	stats.Begin("u1")(true, trae.UsageDelta{HasTotalTokens: true, TotalTokens: 999})
	if v := stats.Get("u1"); v.SuccessCount != 1 {
		t.Fatalf("前置失败: %+v", v)
	}

	rec := traeDo(p, http.MethodPost, "/panel/api/trae/accounts/u1/remove", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	if v := stats.Get("u1"); v.SuccessCount != 0 || v.TokenUsage.TotalTokens != 0 {
		t.Errorf("删除后计数未清空: %+v", v)
	}
}

// TestTraeConfigStatsNeverLeaksCredentials 安全红线：统计字段不得带出凭证。
func TestTraeConfigStatsNeverLeaksCredentials(t *testing.T) {
	dir := t.TempDir()
	p, path, stats := newTraePanelWithStats(t, dir)
	const secret = "SUPER-SECRET-ACCESS-TOKEN-abcdef"
	writeTraeTestConfig(t, path, &trae.Config{
		Enabled: true,
		Accounts: []trae.Account{
			{UID: "u1", AccessToken: secret, RefreshToken: "REFRESH-XYZ", DeviceID: "1234567890123456"},
		},
	})
	stats.Begin("u1")(true, trae.UsageDelta{Model: "glm-5.3"})

	rec := traeDo(p, http.MethodGet, "/panel/api/trae_config?no_checkin=1", "")
	body := rec.Body.String()
	if contains(body, secret) || contains(body, "REFRESH-XYZ") {
		t.Fatalf("响应泄漏凭证: %s", body)
	}
}

func contains(haystack, needle string) bool {
	return len(needle) > 0 && len(haystack) >= len(needle) &&
		func() bool {
			for i := 0; i+len(needle) <= len(haystack); i++ {
				if haystack[i:i+len(needle)] == needle {
					return true
				}
			}
			return false
		}()
}
