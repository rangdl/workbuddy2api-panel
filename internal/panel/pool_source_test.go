// pool_source_test.go 钉住「账号的事实来源是账号池」：
// 池里禁用/移除的 Trae 账号，不得再被面板任务打到上游，也不得出现在配置视图里。
package panel

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	backendtrae "github.com/linguo2625469/workbuddy2api-panel/internal/backend/trae"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/trae"
)

// poolPanel 构造带池的面板：两个 Trae 账号进池。
func poolPanel(t *testing.T, upstreamURL string) (*Panel, *pool.Pool) {
	t.Helper()
	p, path := newTraeOpsPanel(t, upstreamURL)
	cfg, err := trae.Load(path, "")
	if err != nil || cfg == nil {
		t.Fatalf("加载 trae.json: %v", err)
	}
	pl := pool.New("")
	for _, a := range backendtrae.Wrap(cfg, trae.NewClient()) {
		pl.Add(a)
	}
	p.cfg.Pool = pl
	return p, pl
}

// TestSyncProfilesSkipsPoolDisabledAccount 池里禁用的账号不应请求上游。
func TestSyncProfilesSkipsPoolDisabledAccount(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		_, _ = w.Write([]byte(`{"code":0,"data":{"name":"新名字"}}`))
	}))
	t.Cleanup(srv.Close)

	p, pl := poolPanel(t, srv.URL)
	pl.Disable("u-1", "manual disable (panel)")

	if rec := traeDo(p, http.MethodPost, "/panel/api/trae/sync_profiles", ""); rec.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body)
	}
	if hits.Load() != 0 {
		t.Fatalf("池里已禁用的账号不应打上游，实际 %d 次", hits.Load())
	}
}

// TestConfigViewHidesAccountsNotInPool 不在池里的凭证文件不出现在配置视图。
func TestConfigViewHidesAccountsNotInPool(t *testing.T) {
	p, _ := newTraeOpsPanel(t, "http://127.0.0.1:1") // 上游不可达，只看视图
	// 不建池里的账号 → 视图应为空（账号文件存在但不在池里）。
	pl := pool.New("")
	p.cfg.Pool = pl

	rec := traeDo(p, http.MethodGet, "/panel/api/trae_config?no_checkin=1", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body)
	}
	var got struct {
		Config struct {
			Accounts []struct {
				UID string `json:"uid"`
			} `json:"accounts"`
		} `json:"config"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Config.Accounts) != 0 {
		t.Fatalf("不在池里的账号不应出现在视图: %+v", got.Config.Accounts)
	}
}

// TestConfigViewReflectsPoolDisabled 池里禁用后，配置视图的 Disabled 应为真。
func TestConfigViewReflectsPoolDisabled(t *testing.T) {
	p, pl := poolPanel(t, "http://127.0.0.1:1")

	rec := traeDo(p, http.MethodGet, "/panel/api/trae_config?no_checkin=1", "")
	var before struct {
		Config struct {
			Accounts []struct {
				UID      string `json:"uid"`
				Disabled bool   `json:"disabled"`
			} `json:"accounts"`
		} `json:"config"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &before); err != nil {
		t.Fatal(err)
	}
	if len(before.Config.Accounts) != 1 || before.Config.Accounts[0].Disabled {
		t.Fatalf("初始应为一个未禁用账号: %+v", before.Config.Accounts)
	}

	pl.Disable("u-1", "manual disable (panel)")
	rec = traeDo(p, http.MethodGet, "/panel/api/trae_config?no_checkin=1", "")
	var after struct {
		Config struct {
			Accounts []struct {
				Disabled bool `json:"disabled"`
			} `json:"accounts"`
		} `json:"config"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &after); err != nil {
		t.Fatal(err)
	}
	if len(after.Config.Accounts) != 1 || !after.Config.Accounts[0].Disabled {
		t.Fatalf("池里禁用后视图应显示禁用: %+v", after.Config.Accounts)
	}
}
