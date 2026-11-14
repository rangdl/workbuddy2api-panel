package panel

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	backendtrae "github.com/linguo2625469/workbuddy2api-panel/internal/backend/trae"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
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
	p := New(Config{
		Version:       "test",
		APIKey:        "test-key",
		TraePath:      path,
		TraeClient:    client,
		TraeCooldowns: cool,
	})
	return p, path
}

// newTraeOpsPanelWithPool 与 newTraeOpsPanel 相同，但把 Trae 账号按真实启动
// 路径（backend/trae.Wrap）放进账号池，用于验证「签到后首页按钮变已签」。
func newTraeOpsPanelWithPool(t *testing.T, upstreamURL string) (*Panel, *pool.Pool) {
	t.Helper()
	p, path := newTraeOpsPanel(t, upstreamURL)
	cfg, err := trae.Load(path, "")
	if err != nil || cfg == nil {
		t.Fatalf("加载 trae.json: %v", err)
	}
	pl := pool.New("")
	for _, a := range backendtrae.Wrap(cfg, trae.NewClient()) {
		// 账号对象要能读到配置（Credits/Checkin 内部用它）。
		if ta, ok := a.(backendtrae.Account); ok {
			ta.ConfigFn = func() *trae.Config { return cfg }
			a = ta
		}
		pl.Add(a)
	}
	p.cfg.Pool = pl
	return p, pl
}

// newTraeOpsUpstream 假上游：签到成功 + 积分包响应。
// claim 之后 status 会翻转成 checked_in=true，满足回查确认（VerifyClaim）。
func newTraeOpsUpstream(t *testing.T) *httptest.Server {
	t.Helper()
	var claimed bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case trae.EndpointCheckinStatus:
			if claimed {
				_, _ = w.Write([]byte(`{"code":0,"checked_in":true}`))
				return
			}
			_, _ = w.Write([]byte(`{"code":0,"checked_in":false}`))
		case trae.EndpointCheckinClaim:
			claimed = true
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
	// 走池的单号入口，因此需要账号真正入池（与生产同款 backend/trae.Wrap）。
	p, _ := newTraeOpsPanelWithPool(t, srv.URL)

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
	p, _ := newTraeOpsPanelWithPool(t, srv.URL)
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

// TestTraeCheckinAllAndStatus 全量签到 + 状态查询（统一流程：结果读池状态）。
func TestTraeCheckinAllAndStatus(t *testing.T) {
	srv := newTraeOpsUpstream(t)
	p, pl := newTraeOpsPanelWithPool(t, srv.URL)

	if rec := traeDo(p, http.MethodPost, "/panel/api/trae/checkin_all", ""); rec.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body)
	}
	// 签到结果直接体现在池状态上（同一套记账，不再维护独立的结果列表）。
	if st := statusByUID(t, pl, "u-1"); !st.CheckinDone {
		t.Fatalf("批量签到后池状态应显示已签: %+v", st)
	}

	rec := traeDo(p, http.MethodGet, "/panel/api/trae/checkin_status", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var st struct {
		OK      bool `json:"ok"`
		Total   int  `json:"total"`
		Done    int  `json:"done"`
		Results []struct {
			UID    string `json:"uid"`
			Status string `json:"status"`
		} `json:"results"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &st); err != nil {
		t.Fatal(err)
	}
	if !st.OK || st.Total != 1 || st.Done != 1 || len(st.Results) != 1 || st.Results[0].Status != "already" {
		t.Fatalf("签到状态查询不符: %s", rec.Body)
	}
}

// TestTraeCheckinMarksPoolCheckinDone 签到后首页账号表格的「签到」按钮必须变「已签」。
//
// 回归：Trae 签到链路此前从不写 pool 的 last_checkin_day，于是签过之后按钮
// 永远显示「签到」（Trae 配置页走实时查询，两处口径不一致）。
func TestTraeCheckinMarksPoolCheckinDone(t *testing.T) {
	srv := newTraeOpsUpstream(t)
	p, pl := newTraeOpsPanelWithPool(t, srv.URL)

	if st := statusByUID(t, pl, "u-1"); st.CheckinDone {
		t.Fatalf("签到前不应是已签: %+v", st)
	}
	if rec := traeDo(p, http.MethodPost, "/panel/api/trae/accounts/u-1/checkin", ""); rec.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body)
	}
	if st := statusByUID(t, pl, "u-1"); !st.CheckinDone {
		t.Fatalf("单号签到后账号池应显示已签: %+v", st)
	}
}

// TestTraeCheckinAllMarksPoolCheckinDone 批量签到同样回写（面板未注入
// OnOutcomes 时就地补写，保证两条路径都覆盖）。
func TestTraeCheckinAllMarksPoolCheckinDone(t *testing.T) {
	srv := newTraeOpsUpstream(t)
	p, pl := newTraeOpsPanelWithPool(t, srv.URL)

	if rec := traeDo(p, http.MethodPost, "/panel/api/trae/checkin_all", ""); rec.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body)
	}
	if st := statusByUID(t, pl, "u-1"); !st.CheckinDone {
		t.Fatalf("批量签到后账号池应显示已签: %+v", st)
	}
}

// TestTraeCheckinDeviceAlreadyDoesNotMarkDone 设备维度已签（9095）不标记账号已签。
//
// 限额按设备算：多账号共用同一设备号时只有第一个能签成，其余账号会拿到
// 9095。若把这些号也标成「已签」，首页按钮会谎报，运维会以为每个号都签了。
func TestTraeCheckinDeviceAlreadyDoesNotMarkDone(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case trae.EndpointCheckinStatus:
			_, _ = w.Write([]byte(`{"code":0,"checked_in":false}`))
		case trae.EndpointCheckinClaim:
			_, _ = w.Write([]byte(`{"code":9095,"message":"该设备今日已签到"}`))
		default:
			_, _ = w.Write([]byte(`{"code":0}`))
		}
	}))
	t.Cleanup(srv.Close)

	p, pl := newTraeOpsPanelWithPool(t, srv.URL)
	rec := traeDo(p, http.MethodPost, "/panel/api/trae/accounts/u-1/checkin", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body)
	}
	var got struct {
		OK            bool `json:"ok"`
		Already       bool `json:"already"`
		DeviceAlready bool `json:"device_already"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if !got.OK || !got.Already || !got.DeviceAlready {
		t.Fatalf("9095 应按设备已签回传: %s", rec.Body)
	}
	if st := statusByUID(t, pl, "u-1"); st.CheckinDone {
		t.Fatalf("设备维度已签不应标记账号已签: %+v", st)
	}
}

// statusByUID 按裸 uid 取池状态（Trae 的池键是 "trae:uid"，List 输出裸 uid）。
func statusByUID(t *testing.T, pl *pool.Pool, uid string) pool.Status {
	t.Helper()
	for _, st := range pl.List() {
		if st.UID == uid {
			return st
		}
	}
	t.Fatalf("池中找不到账号 %s", uid)
	return pool.Status{}
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
	cfg, _ := trae.Load(path, "")
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

// TestTraeConfigSaveNeverTouchesAccountFiles 保存配置永远不删账号文件。
//
// 回归（用户反馈）：保存配置不该有能力删号。历史上 accounts 字段是全集语义，
// 显式传空数组会删光所有凭证文件。现在无论带不带 accounts、带什么都忽略。
func TestTraeConfigSaveNeverTouchesAccountFiles(t *testing.T) {
	srv := newTraeOpsUpstream(t)
	p, path := newTraeOpsPanel(t, srv.URL)

	for _, body := range []string{
		// 缺省
		`{"enabled":true,"chat_base":"` + srv.URL + `"}`,
		// 显式空数组（旧语义下会删光）
		`{"enabled":true,"chat_base":"` + srv.URL + `","accounts":[]}`,
		// 只列一个不存在的账号
		`{"enabled":true,"chat_base":"` + srv.URL + `","accounts":["nope"]}`,
	} {
		if rec := traeDo(p, http.MethodPost, "/panel/api/trae_config", body); rec.Code != http.StatusOK {
			t.Fatalf("body=%s status = %d body = %s", body, rec.Code, rec.Body)
		}
		saved, err := trae.Load(path, "")
		if err != nil || saved == nil {
			t.Fatalf("重新加载失败: %v", err)
		}
		if len(saved.Accounts) != 1 || saved.Accounts[0].UID != "u-1" {
			t.Fatalf("body=%s 账号应始终保留，实际 %+v", body, saved.Accounts)
		}
		if saved.Accounts[0].AccessToken != "tok-1" {
			t.Errorf("body=%s 凭证应保留，得到 %q", body, saved.Accounts[0].AccessToken)
		}
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
	saved, _ := trae.Load(path, "")
	if !saved.Schedule.Enabled || len(saved.Schedule.Hours) != 2 || saved.Schedule.Hours[0] != 7 {
		t.Fatalf("排程未保存: %+v", saved.Schedule)
	}
	// 账号凭证仍保留
	if saved.Accounts[0].AccessToken != "tok-1" {
		t.Errorf("凭证应保留，得到 %q", saved.Accounts[0].AccessToken)
	}
}
