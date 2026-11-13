package scheduler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// travelStub 模拟 growth 域全部端点，记录调用次数与请求参数。
type travelStub struct {
	buddy       string // /buddy/info 的 data.buddy 原文（"null" 或对象）
	state       string // /travel/status 首次及默认的 data 原文
	stateAfter  string // 非空：第 2 次及以后的 /travel/status 响应（模拟 claim 后状态变化）
	firstStatus int    // /buddy/first 的 HTTP 状态码（非 0 时按业务错误返回）
	quota       string // /buddy/quota 的 data 原文；空则用默认（affordable=5/balance=125/cost=10/max=5）
	openReply   string // /buddy/open 的 data 原文；空则用默认 {"buddy":{"name":"量子喵"}}
	afterClaim  func() // claim 成功后回调（测试用来取消 ctx 等）

	infoCalls, statusCalls, departCalls atomic.Int32
	claimCalls, firstCalls, agreeCalls  atomic.Int32
	quotaCalls, openCalls               atomic.Int32
	location, record, openCount         atomic.Int64
}

// defaultQuota 实测形态（2026-11-12，真实账号 125 能量）。
const defaultQuota = `{"affordable":5,"balance":125,"cost_per_open":10,"max_open_count":5}`

func (s *travelStub) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/activity/growth/buddy/info":
			s.infoCalls.Add(1)
			fmt.Fprintf(w, `{"code":0,"msg":"ok","data":{"buddy":%s}}`, s.buddy)
		case "/activity/growth/buddy/travel/status":
			n := s.statusCalls.Add(1)
			data := s.state
			if n > 1 && s.stateAfter != "" {
				data = s.stateAfter
			}
			fmt.Fprintf(w, `{"code":0,"msg":"ok","data":%s}`, data)
		case "/activity/growth/buddy/travel/depart":
			s.departCalls.Add(1)
			var body struct {
				LocationID int `json:"location_id"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			s.location.Store(int64(body.LocationID))
			w.Write([]byte(`{"code":0,"msg":"ok","data":{}}`))
		case "/activity/growth/buddy/travel/claim":
			s.claimCalls.Add(1)
			var body struct {
				RecordID int64 `json:"record_id"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			s.record.Store(body.RecordID)
			w.Write([]byte(`{"code":0,"msg":"ok","data":{"reward_credit":9}}`))
			if s.afterClaim != nil {
				s.afterClaim()
			}
		case "/activity/growth/buddy/quota":
			s.quotaCalls.Add(1)
			q := s.quota
			if q == "" {
				q = defaultQuota
			}
			fmt.Fprintf(w, `{"code":0,"msg":"ok","data":%s}`, q)
		case "/activity/growth/buddy/open":
			s.openCalls.Add(1)
			var body struct {
				Count       int    `json:"count"`
				ClientToken string `json:"client_token"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			s.openCount.Store(int64(body.Count))
			if body.ClientToken == "" {
				w.WriteHeader(400)
				w.Write([]byte(`{"code":400,"msg":"missing client_token"}`))
				return
			}
			reply := s.openReply
			if reply == "" {
				reply = `{"buddy":{"name":"量子喵"}}`
			}
			fmt.Fprintf(w, `{"code":0,"msg":"ok","data":%s}`, reply)
		case "/activity/growth/buddy/first":
			s.firstCalls.Add(1)
			if s.firstStatus >= 400 {
				w.WriteHeader(s.firstStatus)
				w.Write([]byte(`{"code":400,"msg":"first_buddy task not completed yet"}`))
				return
			}
			w.Write([]byte(`{"code":0,"msg":"ok","data":{"buddy":{"id":1,"name":"档案喵"}}}`))
		case "/activity/growth/buddy/agreement":
			s.agreeCalls.Add(1)
			w.Write([]byte(`{"code":0,"msg":"ok","data":{"agreed":true}}`))
		default:
			http.Error(w, "not found", 404)
		}
	})
}

func (s *travelStub) server() *httptest.Server {
	return httptest.NewServer(s.handler())
}

// billingAndGrowthServer 同时模拟 billing（签到/余额/刷新）与 growth（旅行）端点，
// 供「签到收尾顺带跑旅行」这类跨域用例使用。
func billingAndGrowthServer(stub *travelStub) *httptest.Server {
	growth := stub.handler()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/daily-checkin"):
			w.Write([]byte(`{"code":0,"msg":"ok","data":{}}`))
		case strings.HasSuffix(r.URL.Path, "/get-user-resource"):
			w.Write([]byte(`{"code":0,"data":{"Response":{"Data":{"Accounts":[{"CycleCapacitySize":1000,"CycleCapacityRemain":500,"CycleCapacityUsed":0}]}}}}`))
		case strings.HasSuffix(r.URL.Path, "/token/refresh"):
			w.Write([]byte(`{"code":0,"data":{"accessToken":"new","expiresIn":3600}}`))
		default:
			growth.ServeHTTP(w, r)
		}
	}))
}

// fastTravel 关闭账号间限速，避免测试白等 800ms。
func fastTravel(t *testing.T) {
	t.Helper()
	old := travelAccountDelay
	travelAccountDelay = 0
	t.Cleanup(func() { travelAccountDelay = old })
}

// fastTravelTimer 把该调度器的到站定时器参数调到毫秒级（只影响本实例，
// 不碰包级状态——定时器 goroutine 与测试并发时不产生数据竞争）。
func fastTravelTimer(t *testing.T, s *Scheduler) {
	t.Helper()
	s.travelTimerCfg = travelTimerParams{
		poll:    50 * time.Millisecond,
		step:    5 * time.Millisecond,
		minWait: 0,
	}
}

// startTravelTimer 启动定时器并在用例结束时取消 + 等 goroutine 退出。
func startTravelTimer(t *testing.T, s *Scheduler) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { s.RunTravelTimer(ctx); close(done) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Error("RunTravelTimer 未在 ctx 取消后返回")
		}
	})
}

// newTravelScheduler 构造 travel 相关依赖齐全的调度器。
func newTravelScheduler(t *testing.T, srv *httptest.Server, uids ...string) (*Scheduler, *pool.Pool) {
	t.Helper()
	p := pool.New("")
	for _, uid := range uids {
		p.Add(&auth.Auth{UID: uid, AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	}
	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	return New(Config{Pool: p, Upstream: up, CheckinHours: []int{9, 21}, KeepaliveHours: []int{22}}), p
}

// TestRunCheckinNowNoLongerTriggersTravel 签到收尾不再跑旅行（旅行已剥离为独立排程）。
// 旅行由独立时点（travel_hours）触发，与签到解耦。
func TestRunCheckinNowNoLongerTriggersTravel(t *testing.T) {
	fastTravel(t)
	stub := &travelStub{buddy: "null"}
	srv := billingAndGrowthServer(stub)
	defer srv.Close()

	s, _ := newTravelScheduler(t, srv, "u1")
	s.RunCheckinNow()

	// 签到不再顺带跑旅行：buddy/info 不应被调用。
	if n := stub.infoCalls.Load(); n != 0 {
		t.Errorf("buddy/info calls=%d want 0（旅行已从签到剥离）", n)
	}
}

// TestRunTravelNowAdoptsOnNoBuddy 旅行独立排程：无猫 → 同意协议 + 领养。
func TestRunTravelNowAdoptsOnNoBuddy(t *testing.T) {
	fastTravel(t)
	stub := &travelStub{buddy: "null"}
	srv := stub.server()
	defer srv.Close()

	s, _ := newTravelScheduler(t, srv, "u1")
	s.RunTravelNow()

	if n := stub.infoCalls.Load(); n != 1 {
		t.Errorf("buddy/info calls=%d want 1", n)
	}
	if n := stub.firstCalls.Load(); n != 1 {
		t.Errorf("buddy/first calls=%d want 1（无猫应尝试领养）", n)
	}
	if n := stub.agreeCalls.Load(); n != 1 {
		t.Errorf("buddy/agreement calls=%d want 1", n)
	}
}

// TestRunTravelCoversIdleAccount 旅行独立排程覆盖空闲账号并派出。
func TestRunTravelCoversIdleAccount(t *testing.T) {
	fastTravel(t)
	stub := &travelStub{buddy: `{"id":7,"name":"档案喵"}`,
		state: `{"state":"idle","daily_limit_reached":false}`}
	srv := stub.server()
	defer srv.Close()

	s, _ := newTravelScheduler(t, srv, "u1")

	s.RunTravelNow()

	// 有猫 + idle + 未达上限 → 派出。
	if n := stub.departCalls.Load(); n != 1 {
		t.Errorf("depart calls=%d want 1", n)
	}
}

// TestRunKeepaliveDoesNotTriggerTravel 22 点保活不触发旅行：旅行独立排程。
func TestRunKeepaliveDoesNotTriggerTravel(t *testing.T) {
	fastTravel(t)
	stub := &travelStub{buddy: "null"}
	srv := billingAndGrowthServer(stub)
	defer srv.Close()

	s, _ := newTravelScheduler(t, srv, "u1")
	s.RunKeepaliveNow()

	if n := stub.infoCalls.Load(); n != 0 {
		t.Errorf("buddy/info calls=%d want 0（保活不触发旅行）", n)
	}
	if n := stub.firstCalls.Load(); n != 0 {
		t.Errorf("buddy/first calls=%d want 0", n)
	}
}

// TestRunTravelStateMachine 表驱动覆盖状态机全部分支。
// 注意「arrived-领奖」：claim 成功后会**重查 status**（是为了决定能否接着派出），
// 因此 status 调用数是 2；旧语义「每趟只做一个动作」已被本次改造取代。
func TestRunTravelStateMachine(t *testing.T) {
	const buddyPresent = `{"id":7,"name":"档案喵 R"}`

	cases := []struct {
		name         string
		buddy        string
		state        string
		stateAfter   string
		firstStatus  int
		wantInfo     int32
		wantStatus   int32
		wantDepart   int32
		wantClaim    int32
		wantFirst    int32
		wantAgree    int32
		wantLocation int64
		wantRecord   int64
	}{
		{
			name: "无猫-领养成功", buddy: "null",
			wantInfo: 1, wantFirst: 1, wantAgree: 1,
		},
		{
			name: "无猫-门槛未达-静默跳过", buddy: "null", firstStatus: 400,
			wantInfo: 1, wantFirst: 1, wantAgree: 1,
		},
		{
			name: "有猫-idle-未达上限-派出", buddy: buddyPresent,
			state:    `{"state":"idle","daily_limit_reached":false}`,
			wantInfo: 1, wantStatus: 1, wantDepart: 1, wantLocation: 4,
		},
		{
			name: "有猫-idle-已达上限-跳过", buddy: buddyPresent,
			state:    `{"state":"idle","daily_limit_reached":true}`,
			wantInfo: 1, wantStatus: 1,
		},
		{
			name: "有猫-traveling-跳过", buddy: buddyPresent,
			state:    `{"state":"traveling","record_id":42}`,
			wantInfo: 1, wantStatus: 1,
		},
		{
			name: "有猫-arrived-领奖", buddy: buddyPresent,
			state:    `{"state":"arrived","record_id":42,"reward_credit":9}`,
			wantInfo: 1, wantStatus: 2, wantClaim: 1, wantRecord: 42,
		},
		{
			// 本次改造主场景：到站领奖 → 重查变 idle → 同趟接着派出（闭环不再跨窗口）。
			name: "有猫-arrived-领奖后idle-继续派出", buddy: buddyPresent,
			state:      `{"state":"arrived","record_id":42,"reward_credit":9}`,
			stateAfter: `{"state":"idle","daily_limit_reached":false}`,
			wantInfo:   1, wantStatus: 2, wantClaim: 1, wantDepart: 1, wantLocation: 4, wantRecord: 42,
		},
		{
			// 领奖后仍 arrived：不重复领、不盲发 depart。
			name: "有猫-arrived-领奖后仍arrived-停手", buddy: buddyPresent,
			state:      `{"state":"arrived","record_id":42}`,
			stateAfter: `{"state":"arrived","record_id":42}`,
			wantInfo:   1, wantStatus: 2, wantClaim: 1, wantRecord: 42,
		},
		{
			// 领奖后 idle 但已达当日上限：不再派出。
			name: "有猫-arrived-领奖后idle-已达上限-不派出", buddy: buddyPresent,
			state:      `{"state":"arrived","record_id":42}`,
			stateAfter: `{"state":"idle","daily_limit_reached":true}`,
			wantInfo:   1, wantStatus: 2, wantClaim: 1, wantRecord: 42,
		},
		{
			name: "有猫-arrived-缺record_id-跳过领奖", buddy: buddyPresent,
			state:    `{"state":"arrived","record_id":0}`,
			wantInfo: 1, wantStatus: 1,
		},
		{
			name: "有猫-未知状态-跳过", buddy: buddyPresent,
			state:    `{"state":"teleporting"}`,
			wantInfo: 1, wantStatus: 1,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fastTravel(t)
			stub := &travelStub{buddy: tc.buddy, state: tc.state, stateAfter: tc.stateAfter, firstStatus: tc.firstStatus}
			srv := stub.server()
			defer srv.Close()

			s, _ := newTravelScheduler(t, srv, "u1")
			s.RunTravelNow()

			got := map[string]int32{
				"info": stub.infoCalls.Load(), "status": stub.statusCalls.Load(),
				"depart": stub.departCalls.Load(), "claim": stub.claimCalls.Load(),
				"first": stub.firstCalls.Load(), "agreement": stub.agreeCalls.Load(),
			}
			want := map[string]int32{
				"info": tc.wantInfo, "status": tc.wantStatus,
				"depart": tc.wantDepart, "claim": tc.wantClaim,
				"first": tc.wantFirst, "agreement": tc.wantAgree,
			}
			for k, w := range want {
				if got[k] != w {
					t.Errorf("%s calls=%d want %d", k, got[k], w)
				}
			}
			if tc.wantDepart > 0 && stub.location.Load() != tc.wantLocation {
				t.Errorf("location_id=%d want %d", stub.location.Load(), tc.wantLocation)
			}
			if tc.wantClaim > 0 && stub.record.Load() != tc.wantRecord {
				t.Errorf("record_id=%d want %d", stub.record.Load(), tc.wantRecord)
			}
		})
	}
}

// TestRunTravelAdoptThresholdTriedOncePerDay 门槛未达（400）当日只试一次，后续巡检静默跳过。
func TestRunTravelAdoptThresholdTriedOncePerDay(t *testing.T) {
	fastTravel(t)
	stub := &travelStub{buddy: "null", firstStatus: 400}
	srv := stub.server()
	defer srv.Close()

	s, _ := newTravelScheduler(t, srv, "u1")
	s.RunTravelNow()
	s.RunTravelNow()
	s.RunTravelNow()

	if n := stub.firstCalls.Load(); n != 1 {
		t.Errorf("first calls=%d want 1（当日只试一次）", n)
	}
	if n := stub.agreeCalls.Load(); n != 1 {
		t.Errorf("agreement calls=%d want 1", n)
	}
	if n := stub.infoCalls.Load(); n != 3 {
		t.Errorf("info calls=%d want 3（仍每趟查有无猫）", n)
	}
}

// TestRunTravelAdoptTriedExpiresNextDay 跨自然日后允许重新尝试领养。
func TestRunTravelAdoptTriedExpiresNextDay(t *testing.T) {
	fastTravel(t)
	stub := &travelStub{buddy: "null", firstStatus: 400}
	srv := stub.server()
	defer srv.Close()

	s, _ := newTravelScheduler(t, srv, "u1")
	s.markAdoptTried("u1") // 当日已试过
	s.RunTravelNow()
	if n := stub.firstCalls.Load(); n != 0 {
		t.Fatalf("first calls=%d want 0（当日已试过）", n)
	}

	s.adoptTried["u1"] = "2000-01-01" // 模拟昨日记录
	s.RunTravelNow()
	if n := stub.firstCalls.Load(); n != 1 {
		t.Errorf("first calls=%d want 1（跨日应重试）", n)
	}
}

// TestRunTravelSkipsDisabledAndFailedAccounts 禁用账号不查；单账号出错不影响后续账号。
func TestRunTravelSkipsDisabledAndFailedAccounts(t *testing.T) {
	fastTravel(t)
	var deadCalls, okDepart, disabledCalls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Header.Get("X-User-Id") == "disabled":
			disabledCalls.Add(1)
			w.WriteHeader(401)
			w.Write([]byte(`{"code":12153,"msg":"Offline user session not found"}`))
		case r.Header.Get("X-User-Id") == "dead":
			deadCalls.Add(1)
			w.WriteHeader(401)
			w.Write([]byte(`{"code":12153,"msg":"Offline user session not found"}`))
		case r.URL.Path == "/activity/growth/buddy/info":
			w.Write([]byte(`{"code":0,"data":{"buddy":{"id":1}}}`))
		case r.URL.Path == "/activity/growth/buddy/travel/status":
			w.Write([]byte(`{"code":0,"data":{"state":"idle","daily_limit_reached":false}}`))
		case r.URL.Path == "/activity/growth/buddy/travel/depart":
			okDepart.Add(1)
			w.Write([]byte(`{"code":0,"data":{}}`))
		default:
			http.Error(w, "not found", 404)
		}
	}))
	defer srv.Close()

	s, p := newTravelScheduler(t, srv, "disabled", "dead", "ok")
	p.Disable("disabled", "test")
	s.RunTravelNow()

	if n := deadCalls.Load(); n != 1 {
		t.Errorf("dead account calls=%d want 1（401 跳过本轮，不强刷 token）", n)
	}
	if n := disabledCalls.Load(); n != 0 {
		t.Errorf("disabled account calls=%d want 0", n)
	}
	// 前列账号失败不应中断遍历：末位 ok 账号照常完成派出。
	if n := okDepart.Load(); n != 1 {
		t.Errorf("ok account depart calls=%d want 1（失败账号不影响后续遍历）", n)
	}
	st, ok := p.Status("ok")
	if !ok {
		t.Fatal("ok 账号应在池中")
	}
	if st.Disabled {
		t.Errorf("ok 账号不应被影响: %+v", st)
	}
}

// TestRunTravelDisabledAccountSkipsAllCalls 禁用账号一个请求都不发。
func TestRunTravelDisabledAccountSkipsAllCalls(t *testing.T) {
	fastTravel(t)
	stub := &travelStub{buddy: "null"}
	srv := stub.server()
	defer srv.Close()

	s, p := newTravelScheduler(t, srv, "u1")
	p.Disable("u1", "test")
	s.RunTravelNow()

	if n := stub.infoCalls.Load(); n != 0 {
		t.Errorf("info calls=%d want 0（禁用账号应跳过）", n)
	}
}

// TestRunTravelActionErrorsDoNotAbort 各环节上游报错只影响本账号本轮：不 panic、不中断遍历。
func TestRunTravelActionErrorsDoNotAbort(t *testing.T) {
	fastTravel(t)
	var okDepart atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		uid := r.Header.Get("X-User-Id")
		fail := func() {
			w.WriteHeader(500)
			w.Write([]byte(`{"code":500,"msg":"boom"}`))
		}
		switch {
		case uid == "bdinfo": // 查有无猫失败
			fail()
		case uid == "status" && r.URL.Path == "/activity/growth/buddy/travel/status": // 查状态失败
			fail()
		case uid == "depart" && r.URL.Path == "/activity/growth/buddy/travel/depart": // 派出发失败
			fail()
		case uid == "claim" && r.URL.Path == "/activity/growth/buddy/travel/claim": // 领奖失败
			fail()
		case uid == "agree" && r.URL.Path == "/activity/growth/buddy/agreement": // 同意协议失败
			fail()
		case uid == "first" && r.URL.Path == "/activity/growth/buddy/first": // 领养非门槛类失败
			fail()
		case r.URL.Path == "/activity/growth/buddy/info":
			if uid == "claim" {
				w.Write([]byte(`{"code":0,"data":{"buddy":{"id":1}}}`))
				return
			}
			if uid == "depart" || uid == "status" || uid == "ok" {
				w.Write([]byte(`{"code":0,"data":{"buddy":{"id":1}}}`))
				return
			}
			w.Write([]byte(`{"code":0,"data":{"buddy":null}}`))
		case r.URL.Path == "/activity/growth/buddy/travel/status":
			if uid == "claim" {
				w.Write([]byte(`{"code":0,"data":{"state":"arrived","record_id":42}}`))
				return
			}
			w.Write([]byte(`{"code":0,"data":{"state":"idle","daily_limit_reached":false}}`))
		case r.URL.Path == "/activity/growth/buddy/travel/depart":
			okDepart.Add(1)
			w.Write([]byte(`{"code":0,"data":{}}`))
		case r.URL.Path == "/activity/growth/buddy/agreement":
			w.Write([]byte(`{"code":0,"data":{"agreed":true}}`))
		default:
			http.Error(w, "not found", 404)
		}
	}))
	defer srv.Close()

	s, _ := newTravelScheduler(t, srv, "agree", "bdinfo", "claim", "depart", "first", "ok", "status")
	s.RunTravelNow() // 不应 panic

	// 末位账号（uid 排序后 ok 排在 status 前，但都在失败账号之后）照常完成派出。
	if n := okDepart.Load(); n == 0 {
		t.Errorf("depart calls=%d want >0（个别账号失败不应中断遍历）", n)
	}
}

// TestRunTravelLoopCancelsWhenDisabled 无任何整点任务时 Run 不空转，ctx 取消即返回。
func TestRunTravelLoopCancelsWhenDisabled(t *testing.T) {
	s := &Scheduler{cfg: Config{}, adoptTried: map[string]string{}}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { s.Run(ctx); close(done) }()
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run 未在 ctx 取消后返回")
	}
}

// TestRunTravelLoopStopsOnCancel 有排程在等计时器时 ctx 取消应立即退出。
func TestRunTravelLoopStopsOnCancel(t *testing.T) {
	s := New(Config{CheckinHours: []int{9}, KeepaliveHours: []int{22}})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { s.Run(ctx); close(done) }()
	time.Sleep(20 * time.Millisecond) // 让 Run 进入 select 等待
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run 未在 ctx 取消后返回")
	}
}

// TestTravelBlindBoxGating 盲盒门控与次数钳制（表驱动）。
//
// 关键不变量：
//   - 旅行非 idle 时一次 quota/open 都不发（上游会拒绝，且开盒要求 idle）；
//   - affordable<=0 时只查 quota、不发 open；
//   - count = min(affordable, max_open_count, 配置上限)。
func TestTravelBlindBoxGating(t *testing.T) {
	const buddyPresent = `{"id":7,"name":"档案喵"}`
	cases := []struct {
		name       string
		state      string
		quota      string
		maxPerRun  int
		wantQuota  int32
		wantOpen   int32
		wantCount  int64
		wantDepart int32
	}{
		{
			name:  "idle-能量充足-开盒取三者最小",
			state: `{"state":"idle","daily_limit_reached":true}`,
			quota: `{"affordable":5,"balance":125,"cost_per_open":10,"max_open_count":5}`,
			// daily_limit_reached=true 阻止派出，隔离出「只开盒」这一路。
			maxPerRun: 3, wantQuota: 1, wantOpen: 1, wantCount: 3,
		},
		{
			name:      "idle-上游可开次数小于配置上限",
			state:     `{"state":"idle","daily_limit_reached":true}`,
			quota:     `{"affordable":2,"balance":25,"cost_per_open":10,"max_open_count":5}`,
			maxPerRun: 5, wantQuota: 1, wantOpen: 1, wantCount: 2,
		},
		{
			name:      "idle-上游max_open_count小于affordable",
			state:     `{"state":"idle","daily_limit_reached":true}`,
			quota:     `{"affordable":9,"balance":95,"cost_per_open":10,"max_open_count":4}`,
			maxPerRun: 20, wantQuota: 1, wantOpen: 1, wantCount: 4,
		},
		{
			name:      "idle-能量不足-不发open",
			state:     `{"state":"idle","daily_limit_reached":true}`,
			quota:     `{"affordable":0,"balance":5,"cost_per_open":10,"max_open_count":5}`,
			maxPerRun: 5, wantQuota: 1, wantOpen: 0,
		},
		{
			name:      "traveling-不开盒",
			state:     `{"state":"traveling","record_id":42}`,
			quota:     `{"affordable":5,"balance":125,"cost_per_open":10,"max_open_count":5}`,
			maxPerRun: 5, wantQuota: 0, wantOpen: 0,
		},
		{
			name:      "arrived-不开盒",
			state:     `{"state":"arrived","record_id":42}`,
			quota:     `{"affordable":5,"balance":125,"cost_per_open":10,"max_open_count":5}`,
			maxPerRun: 5, wantQuota: 0, wantOpen: 0,
		},
		{
			// 盲盒必须排在派出之前：派出后猫进入 traveling，开盒会被上游拒绝。
			name:      "idle-开盒后接着派出",
			state:     `{"state":"idle","daily_limit_reached":false}`,
			quota:     `{"affordable":5,"balance":125,"cost_per_open":10,"max_open_count":5}`,
			maxPerRun: 5, wantQuota: 1, wantOpen: 1, wantCount: 5, wantDepart: 1,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fastTravel(t)
			stub := &travelStub{buddy: buddyPresent, state: tc.state, quota: tc.quota}
			srv := stub.server()
			defer srv.Close()

			s, _ := newTravelScheduler(t, srv, "u1")
			s.SetTravelBlindbox(true, tc.maxPerRun)
			s.RunTravelNow()

			if got := stub.quotaCalls.Load(); got != tc.wantQuota {
				t.Errorf("quota calls=%d want %d", got, tc.wantQuota)
			}
			if got := stub.openCalls.Load(); got != tc.wantOpen {
				t.Errorf("open calls=%d want %d", got, tc.wantOpen)
			}
			if tc.wantOpen > 0 && stub.openCount.Load() != tc.wantCount {
				t.Errorf("open count=%d want %d", stub.openCount.Load(), tc.wantCount)
			}
			if got := stub.departCalls.Load(); got != tc.wantDepart {
				t.Errorf("depart calls=%d want %d", got, tc.wantDepart)
			}
		})
	}
}

// TestTravelBlindBoxDisabledByDefault 开关关闭时（默认）一个盲盒请求都不发。
func TestTravelBlindBoxDisabledByDefault(t *testing.T) {
	fastTravel(t)
	stub := &travelStub{buddy: `{"id":7}`, state: `{"state":"idle","daily_limit_reached":true}`}
	srv := stub.server()
	defer srv.Close()

	s, _ := newTravelScheduler(t, srv, "u1")
	s.RunTravelNow() // 未调用 SetTravelBlindbox：零值 = 关闭

	if n := stub.quotaCalls.Load(); n != 0 {
		t.Errorf("quota calls=%d want 0（默认关闭）", n)
	}
	if n := stub.openCalls.Load(); n != 0 {
		t.Errorf("open calls=%d want 0（默认关闭）", n)
	}
}

// TestSetTravelBlindboxDefaults 非法上限回落默认值（配置层 <=0 也应视作未配置）。
func TestSetTravelBlindboxDefaults(t *testing.T) {
	s := newTestScheduler()
	s.SetTravelBlindbox(true, 0) // 0 = 未配置
	enabled, max := s.travelBlindboxCfg()
	if !enabled || max != blindboxDefaultMaxPerRun {
		t.Errorf("enabled=%v max=%d want true/%d", enabled, max, blindboxDefaultMaxPerRun)
	}
	s.SetTravelBlindbox(true, 3)
	if _, max := s.travelBlindboxCfg(); max != 3 {
		t.Errorf("max=%d want 3", max)
	}
}

// TestRunBlindboxNowDryRunDoesNotOpen 面板手动入口默认 dry-run：只查 quota，不消耗能量。
func TestRunBlindboxNowDryRunDoesNotOpen(t *testing.T) {
	fastTravel(t)
	stub := &travelStub{buddy: `{"id":7}`, state: `{"state":"idle","daily_limit_reached":true}`}
	srv := stub.server()
	defer srv.Close()

	s, _ := newTravelScheduler(t, srv, "u1")
	s.RunBlindboxNow(false, 5)

	if n := stub.quotaCalls.Load(); n != 1 {
		t.Errorf("quota calls=%d want 1（dry-run 要展示配额）", n)
	}
	if n := stub.openCalls.Load(); n != 0 {
		t.Errorf("open calls=%d want 0（dry-run 不得消耗能量）", n)
	}
	if n := stub.departCalls.Load(); n != 0 {
		t.Errorf("depart calls=%d want 0（手动开盒不派出）", n)
	}

	s.RunBlindboxNow(true, 5)
	if n := stub.openCalls.Load(); n != 1 {
		t.Errorf("open calls=%d want 1（apply=true 才真正开盒）", n)
	}
}

// TestRunBlindboxNowReturnsPerAccountResults 面板要直接展示逐账号结果，
// 不能只返回「已触发」——用户需要看到谁、能量多少、为什么不能开。
func TestRunBlindboxNowReturnsPerAccountResults(t *testing.T) {
	fastTravel(t)
	stub := &travelStub{buddy: `{"id":7}`, state: `{"state":"idle","daily_limit_reached":true}`}
	srv := stub.server()
	defer srv.Close()

	s, _ := newTravelScheduler(t, srv, "u1")
	results := s.RunBlindboxNow(false, 5)

	if len(results) != 1 {
		t.Fatalf("results=%d want 1", len(results))
	}
	r := results[0]
	if r.UID != "u1" {
		t.Errorf("uid=%q want u1", r.UID)
	}
	if r.TravelState != "idle" {
		t.Errorf("travel_state=%q want idle", r.TravelState)
	}
	// stub 的 quota 返回 balance=125 affordable=5 cost=10 max=5
	if r.Energy != 125 || r.Affordable != 5 {
		t.Errorf("energy=%d affordable=%d want 125/5", r.Energy, r.Affordable)
	}
	if r.WillOpen != 5 {
		t.Errorf("will_open=%d want 5", r.WillOpen)
	}
	if r.Decision == "" {
		t.Error("decision 不能为空（面板要展示不能开的原因）")
	}
	if r.Opened {
		t.Error("dry-run 不应标记 opened")
	}

	// apply 后应带回奖品名与 opened 标记
	results = s.RunBlindboxNow(true, 5)
	if len(results) != 1 || !results[0].Opened {
		t.Fatalf("apply 后 opened 应为 true: %+v", results)
	}
	if results[0].Prize == "" {
		t.Error("apply 后应带回奖品名")
	}
}

// TestRunBlindboxNowReportsBlockReason 不能开时要给出可读理由，而不是静默跳过。
func TestRunBlindboxNowReportsBlockReason(t *testing.T) {
	fastTravel(t)

	t.Run("旅行中", func(t *testing.T) {
		stub := &travelStub{buddy: `{"id":7}`, state: `{"state":"traveling","record_id":9,"arrive_at":9999999999}`}
		srv := stub.server()
		defer srv.Close()
		s, _ := newTravelScheduler(t, srv, "u1")
		r := s.RunBlindboxNow(false, 5)[0]
		if r.WillOpen != 0 || !strings.Contains(r.Decision, "空闲") {
			t.Errorf("traveling 应拒绝并说明原因: %+v", r)
		}
	})

	t.Run("能量不足", func(t *testing.T) {
		stub := &travelStub{buddy: `{"id":7}`, state: `{"state":"idle"}`}
		stub.quota = `{"affordable":0,"balance":3,"cost_per_open":10,"max_open_count":5}`
		srv := stub.server()
		defer srv.Close()
		s, _ := newTravelScheduler(t, srv, "u1")
		r := s.RunBlindboxNow(false, 5)[0]
		if r.WillOpen != 0 || !strings.Contains(r.Decision, "能量不足") {
			t.Errorf("能量不足应拒绝并说明: %+v", r)
		}
	})
}

// TestTravelDueTime 待唤醒时刻换算：traveling 以「ArriveAt-ServerNow」为准（免疫本机
// 时钟偏差）；arrived 立即触发（启动补跑）。
func TestTravelDueTime(t *testing.T) {
	now := time.Date(2026, 11, 12, 20, 0, 0, 0, time.UTC)
	local := now.Unix()

	// 上游口径：还有 7200s 到站；本机 Unix 时刻故意与上游差 1 天（模拟时钟漂移）。
	ts := &upstream.TravelState{
		State:     "traveling",
		RecordID:  1,
		ServerNow: local + 86400,
		ArriveAt:  local + 86400 + 7200,
	}
	at, key, ok := travelDueTime(ts, now)
	if !ok {
		t.Fatal("traveling 且有 arrive_at 应返回到站时刻")
	}
	if d := at.Sub(now); d != 7200*time.Second {
		t.Errorf("剩余=%v want 2h（应按 ArriveAt-ServerNow 换算，不受本机时钟影响）", d)
	}
	if key != ts.ArriveAt {
		t.Errorf("去重键=%d want arrive_at=%d（同一趟行程身份稳定）", key, ts.ArriveAt)
	}

	// 上游未回 server_now：退回本机时钟。
	noServer := &upstream.TravelState{State: "traveling", ArriveAt: local + 600}
	if at, _, ok := travelDueTime(noServer, now); !ok || at.Sub(now) != 600*time.Second {
		t.Errorf("缺 server_now 应退回本机时钟，got %v ok=%v", at, ok)
	}

	// 已过点：立即唤醒（at 不晚于 now）。
	past := &upstream.TravelState{State: "traveling", ServerNow: local + 100, ArriveAt: local + 50}
	if at, _, ok := travelDueTime(past, now); !ok || at.After(now) {
		t.Errorf("已过点应返回不晚于 now 的时刻，got %v ok=%v", at, ok)
	}

	// arrived：立即触发（启动补跑——关机期间到站的行程，重启后马上领奖）。
	arrived := &upstream.TravelState{State: "arrived", RecordID: 28367071}
	at, key, ok = travelDueTime(arrived, now)
	if !ok || at.After(now) {
		t.Errorf("arrived 应立即触发，got at=%v ok=%v", at, ok)
	}
	if key != 28367071 {
		t.Errorf("arrived 去重键=%d want record_id=28367071", key)
	}
	// arrived 但缺 record_id：无法领奖，不参与定时（交给整点巡检记日志）。
	if _, _, ok := travelDueTime(&upstream.TravelState{State: "arrived"}, now); ok {
		t.Error("arrived 缺 record_id 不应产生定时")
	}

	// 其余状态不参与定时。
	for _, bad := range []*upstream.TravelState{
		nil,
		{State: "idle", ArriveAt: local + 7200, ServerNow: local},
		{State: "traveling"}, // 无 arrive_at
		{State: "teleporting", ArriveAt: local + 7200},
	} {
		if _, _, ok := travelDueTime(bad, now); ok {
			t.Errorf("state=%+v 不应产生定时", bad)
		}
	}
}

// TestRunTravelTimerWakesOnArrival 到站定时器：arrive_at 在未来 → 到点唤醒并领奖。
func TestRunTravelTimerWakesOnArrival(t *testing.T) {
	fastTravel(t)
	stub := &travelStub{
		buddy: `{"id":7}`,
		// 用「上游口径」给 1s 后到站（上游时间戳只有秒级精度，本机 now 无关，
		// travelArriveTime 只算差值）；到点后上游状态变 arrived（真实上游的行为），
		// 定时器随即领奖。
		state:      `{"state":"traveling","record_id":42,"server_now":1000,"arrive_at":1001}`,
		stateAfter: `{"state":"arrived","record_id":42}`,
	}
	srv := stub.server()
	defer srv.Close()

	s, _ := newTravelScheduler(t, srv, "u1")
	fastTravelTimer(t, s)
	startTravelTimer(t, s)

	// 等唤醒后 travelOne 真正执行。
	deadline := time.After(3 * time.Second)
	for stub.claimCalls.Load() == 0 {
		select {
		case <-deadline:
			t.Fatalf("定时器到点后未领奖：status=%d claim=%d", stub.statusCalls.Load(), stub.claimCalls.Load())
		case <-time.After(5 * time.Millisecond):
		}
	}
	if got := stub.record.Load(); got != 42 {
		t.Errorf("claim record_id=%d want 42", got)
	}
}

// TestRunTravelTimerClaimsPastArrivalImmediately 启动补跑：arrive_at 已过点 → 立即领奖
// （覆盖服务关机期间到站的行程，19 天未领的场景）。
func TestRunTravelTimerClaimsPastArrivalImmediately(t *testing.T) {
	fastTravel(t)
	stub := &travelStub{
		buddy:      `{"id":7}`,
		state:      `{"state":"traveling","record_id":77,"server_now":999999,"arrive_at":1}`,
		stateAfter: `{"state":"arrived","record_id":77}`,
	}
	srv := stub.server()
	defer srv.Close()

	s, _ := newTravelScheduler(t, srv, "u1")
	fastTravelTimer(t, s)
	startTravelTimer(t, s)

	deadline := time.After(2 * time.Second)
	for stub.claimCalls.Load() == 0 {
		select {
		case <-deadline:
			t.Fatalf("已过点行程未立即领取：status=%d claim=%d", stub.statusCalls.Load(), stub.claimCalls.Load())
		case <-time.After(5 * time.Millisecond):
		}
	}
}

// TestRunTravelTimerNoBusyLoop 上游持续返回「已过点但仍是 traveling」的行程时，
// 同一趟行程只触发一次：不得以 travelTimerMinWait 的节奏无限重查（空转上游）。
func TestRunTravelTimerNoBusyLoop(t *testing.T) {
	fastTravel(t)
	stub := &travelStub{
		buddy: `{"id":7}`,
		state: `{"state":"traveling","record_id":42,"server_now":999999,"arrive_at":1}`,
	}
	srv := stub.server()
	defer srv.Close()

	s, _ := newTravelScheduler(t, srv, "u1")
	fastTravelTimer(t, s)
	startTravelTimer(t, s)

	// 首轮扫描 + 一次触发（触发后 travelOne 里还有 buddy/info + status）。
	deadline := time.After(2 * time.Second)
	for stub.infoCalls.Load() == 0 {
		select {
		case <-deadline:
			t.Fatal("已过点行程未触发 travelOne")
		case <-time.After(5 * time.Millisecond):
		}
	}
	// 之后应退回轮询间隔（50ms），200ms 内最多再多 5 轮扫描；留足余量断言 < 20。
	time.Sleep(200 * time.Millisecond)
	if n := stub.statusCalls.Load(); n > 20 {
		t.Errorf("status calls=%d（空转：已触发的行程被反复重查）", n)
	}
}

// TestRunTravelTimerClaimsArrivedOnStartup 启动补跑：上游已返回 arrived（关机期间到站、
// 未领奖）→ 定时器首轮立即领奖，不等下一个 travel_hours 整点。
//
// 线上实证：固定 [9,21] 窗口常年错过，某账号到站奖励挂了 19 天。若 arrived 也走
// 「等整点」路径，本修复等于没做。
func TestRunTravelTimerClaimsArrivedOnStartup(t *testing.T) {
	fastTravel(t)
	// 全程 arrived（真上游在 claim 成功前不会变状态）：定时器扫描/travelOne/claim 后
	// 重查都看到 arrived，因此只领取一次、不重复派出。
	stub := &travelStub{
		buddy: `{"id":7}`,
		state: `{"state":"arrived","record_id":28367071,"reward_credit":8}`,
	}
	srv := stub.server()
	defer srv.Close()

	s, _ := newTravelScheduler(t, srv, "u1")
	// 默认 poll 10 分钟：若 arrived 不立即触发，本用例会超时。
	startTravelTimer(t, s)

	deadline := time.After(2 * time.Second)
	for stub.claimCalls.Load() == 0 {
		select {
		case <-deadline:
			t.Fatalf("启动时 arrived 未立即领奖：status=%d claim=%d", stub.statusCalls.Load(), stub.claimCalls.Load())
		case <-time.After(5 * time.Millisecond):
		}
	}
	if got := stub.record.Load(); got != 28367071 {
		t.Errorf("claim record_id=%d want 28367071", got)
	}
	// 领奖后重查仍 arrived → 停手，不盲发 depart。
	if n := stub.departCalls.Load(); n != 0 {
		t.Errorf("depart calls=%d want 0（仍 arrived 不应派出）", n)
	}
}

// TestRunTravelTimerRearm 手动触发（PokeTravelTimer）立即唤醒重算，不必等满到站时刻。
func TestRunTravelTimerRearm(t *testing.T) {
	fastTravel(t)
	stub := &travelStub{
		buddy: `{"id":7}`,
		// 1 小时后到站：若没有 rearm，本用例会超时。
		state: `{"state":"traveling","record_id":42,"server_now":1000,"arrive_at":1000 + 3600}`,
	}
	srv := stub.server()
	defer srv.Close()

	s, _ := newTravelScheduler(t, srv, "u1")
	fastTravelTimer(t, s)
	startTravelTimer(t, s)

	// 等待首轮扫描完成（至少查过一次 status）。
	deadline := time.After(2 * time.Second)
	for stub.statusCalls.Load() == 0 {
		select {
		case <-deadline:
			t.Fatal("定时器未开始扫描")
		case <-time.After(2 * time.Millisecond):
		}
	}
	before := stub.statusCalls.Load()
	s.PokeTravelTimer()
	deadline = time.After(2 * time.Second)
	for stub.statusCalls.Load() <= before {
		select {
		case <-deadline:
			t.Fatal("PokeTravelTimer 未触发重算（仍在睡 1 小时的到站时刻）")
		case <-time.After(5 * time.Millisecond):
		}
	}
}

// TestRunTravelTimerCancels 无在途行程时也响应 ctx 取消（不卡在兜底轮询）。
func TestRunTravelTimerCancels(t *testing.T) {
	fastTravel(t)
	stub := &travelStub{buddy: `{"id":7}`, state: `{"state":"idle","daily_limit_reached":true}`}
	srv := stub.server()
	defer srv.Close()

	s, _ := newTravelScheduler(t, srv, "u1")
	fastTravelTimer(t, s)
	startTravelTimer(t, s) // cleanup 里 cancel + 等退出：未返回即用例失败
	time.Sleep(30 * time.Millisecond)
}

// TestTravelDayAlignsCST 每日重置按 CST 自然日判定（UTC 17:00 已是次日 CST）。
func TestTravelDayAlignsCST(t *testing.T) {
	cases := []struct {
		name string
		in   time.Time
		want string
	}{
		{"UTC 凌晨 = CST 当日", time.Date(2026, 9, 11, 2, 0, 0, 0, time.UTC), "2026-09-11"},
		{"UTC 16:00 = CST 次日 00:00", time.Date(2026, 9, 11, 16, 0, 0, 0, time.UTC), "2026-09-12"},
		{"UTC 15:59 仍在 CST 当日", time.Date(2026, 9, 11, 15, 59, 0, 0, time.UTC), "2026-09-11"},
	}
	for _, c := range cases {
		if got := travelDay(c.in); got != c.want {
			t.Errorf("%s: travelDay=%s want %s", c.name, got, c.want)
		}
	}
}
