package panel

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/trae"
)

// newTraeOpsPanel 构造带调度器与假上游的面板。
func newTraeOpsPanel(t *testing.T, upstreamURL string) (*Panel, string) {
	t.Helper()
	dir := t.TempDir()
	path := dir + "/trae.json"
	cfg := &trae.Config{
		Enabled:     true,
		ChatBase:    upstreamURL,
		APITraeBase: upstreamURL,
		Accounts: []trae.Account{{
			UID: "u-1", AccessToken: "tok-1", DeviceID: "dev-1",
		}},
	}
	writeTraeTestConfig(t, path, cfg)
	client := trae.NewClient()
	cool := trae.NewCooldowns()
	sched := trae.NewScheduler(func() *trae.Config {
		c, err := trae.Load(path)
		if err != nil {
			return nil
		}
		return c
	}, client, cool)
	p := New(Config{
		Version:       "test",
		APIKey:        "test-key",
		TraePath:      path,
		TraeClient:    client,
		TraeCooldowns: cool,
		TraeScheduler: sched,
	})
	return p, path
}

// newTraeOpsUpstream 假上游：签到成功 + 积分包响应。
func newTraeOpsUpstream(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case trae.EndpointCheckinStatus:
			_, _ = w.Write([]byte(`{"code":0,"checked_in":false}`))
		case trae.EndpointCheckinClaim:
			_, _ = w.Write([]byte(`{"code":0,"message":"签到成功","data":{"reward":150}}`))
		case trae.EndpointEntUsage:
			_, _ = w.Write([]byte(`{"code":0,"data":{"user_entitlement_pack_list":[
				{"entitlement_base_info":{"product_id":208,"credits_limit":1000},"usage":{"credits_amount":250}}
			]}}`))
		default:
			_, _ = w.Write([]byte(`{"code":0}`))
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestTraeAccountCheckin 单账号签到：成功返回奖励。
func TestTraeAccountCheckin(t *testing.T) {
	srv := newTraeOpsUpstream(t)
	p, _ := newTraeOpsPanel(t, srv.URL)

	rec := traeDo(p, http.MethodPost, "/panel/api/trae/accounts/u-1/checkin", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body)
	}
	var got struct {
		OK      bool   `json:"ok"`
		UID     string `json:"uid"`
		Already bool   `json:"already"`
		Reward  int64  `json:"reward"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if !got.OK || got.Already || got.Reward != 150 || got.UID != "u-1" {
		t.Fatalf("签到结果不符: %s", rec.Body)
	}
}

// TestTraeAccountCheckinUnknownUID 未知 uid → 404。
func TestTraeAccountCheckinUnknownUID(t *testing.T) {
	srv := newTraeOpsUpstream(t)
	p, _ := newTraeOpsPanel(t, srv.URL)
	if rec := traeDo(p, http.MethodPost, "/panel/api/trae/accounts/nope/checkin", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

// TestTraeAccountCredits 积分查询：返回通用/Work 分类汇总。
func TestTraeAccountCredits(t *testing.T) {
	srv := newTraeOpsUpstream(t)
	p, _ := newTraeOpsPanel(t, srv.URL)

	rec := traeDo(p, http.MethodPost, "/panel/api/trae/accounts/u-1/credits", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body)
	}
	var got struct {
		OK      bool `json:"ok"`
		Credits struct {
			General float64 `json:"general"`
			Total   float64 `json:"total"`
			Limit   float64 `json:"limit"`
		} `json:"credits"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if !got.OK || got.Credits.General != 750 || got.Credits.Total != 750 || got.Credits.Limit != 1000 {
		t.Fatalf("积分结果不符: %s", rec.Body)
	}
}

// TestTraeCheckinAllAndStatus 全量签到 + 结果查询。
func TestTraeCheckinAllAndStatus(t *testing.T) {
	srv := newTraeOpsUpstream(t)
	p, _ := newTraeOpsPanel(t, srv.URL)

	rec := traeDo(p, http.MethodPost, "/panel/api/trae/checkin_all", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body)
	}
	var all struct {
		OK      bool                  `json:"ok"`
		Results []trae.CheckinOutcome `json:"results"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &all); err != nil {
		t.Fatal(err)
	}
	if !all.OK || len(all.Results) != 1 || all.Results[0].Status != "success" {
		t.Fatalf("全量签到结果不符: %s", rec.Body)
	}

	// 结果可查询（同一 scheduler 实例）
	rec = traeDo(p, http.MethodGet, "/panel/api/trae/checkin_status", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var st struct {
		OK      bool                  `json:"ok"`
		LastRun string                `json:"last_run"`
		Results []trae.CheckinOutcome `json:"results"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &st); err != nil {
		t.Fatal(err)
	}
	if !st.OK || st.LastRun == "" || len(st.Results) != 1 {
		t.Fatalf("签到结果查询不符: %s", rec.Body)
	}
}

// TestTraeOpsUnavailable 未注入 scheduler / client 时返回 501。
func TestTraeOpsUnavailable(t *testing.T) {
	p := New(Config{Version: "test", APIKey: "test-key"})
	for _, c := range []struct{ method, path string }{
		{http.MethodPost, "/panel/api/trae/checkin_all"},
		{http.MethodGet, "/panel/api/trae/checkin_status"},
		{http.MethodPost, "/panel/api/trae/accounts/u/checkin"},
		{http.MethodPost, "/panel/api/trae/accounts/u/credits"},
	} {
		rec := traeDo(p, c.method, c.path, "")
		if rec.Code != http.StatusNotImplemented {
			t.Errorf("%s %s status = %d, want 501", c.method, c.path, rec.Code)
		}
	}
}

// TestTraeConfigViewIncludesSchedule 配置视图带出排程字段（面板回显用）。
func TestTraeConfigViewIncludesSchedule(t *testing.T) {
	srv := newTraeOpsUpstream(t)
	p, path := newTraeOpsPanel(t, srv.URL)
	cfg, _ := trae.Load(path)
	cfg.Schedule = trae.CheckinSchedule{Enabled: true, Hours: []int{9, 21}, GapMS: 800}
	writeTraeTestConfig(t, path, cfg)

	rec := traeDo(p, http.MethodGet, "/panel/api/trae_config", "")
	var got struct {
		Config struct {
			Schedule struct {
				Enabled bool  `json:"checkin_enabled"`
				Hours   []int `json:"checkin_hours"`
			} `json:"schedule"`
		} `json:"config"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if !got.Config.Schedule.Enabled || len(got.Config.Schedule.Hours) != 2 {
		t.Fatalf("排程未回显: %s", rec.Body)
	}
}

// TestTraeConfigSaveSchedule 保存排程字段。
func TestTraeConfigSaveSchedule(t *testing.T) {
	srv := newTraeOpsUpstream(t)
	p, path := newTraeOpsPanel(t, srv.URL)

	body := `{"enabled":true,"chat_base":"` + srv.URL + `","accounts":["u-1"],"disabled":[],
		"schedule":{"checkin_enabled":true,"checkin_hours":[7,19]}}`
	rec := traeDo(p, http.MethodPost, "/panel/api/trae_config", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body)
	}
	saved, _ := trae.Load(path)
	if !saved.Schedule.Enabled || len(saved.Schedule.Hours) != 2 || saved.Schedule.Hours[0] != 7 {
		t.Fatalf("排程未保存: %+v", saved.Schedule)
	}
	// 账号凭证仍保留
	if saved.Accounts[0].AccessToken != "tok-1" {
		t.Errorf("凭证应保留，得到 %q", saved.Accounts[0].AccessToken)
	}
}
