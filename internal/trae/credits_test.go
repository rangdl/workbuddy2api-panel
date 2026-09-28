package trae

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// newAPIServer 假 api.trae.cn：按路径返回预设响应。
func newAPIServer(t *testing.T, handler func(path string, body map[string]any) (int, string)) (*httptest.Server, *[]map[string]any) {
	t.Helper()
	var seen []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		seen = append(seen, body)
		status, resp := handler(r.URL.Path, body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(resp))
	}))
	t.Cleanup(srv.Close)
	return srv, &seen
}

func apiCfg(base string) *Config {
	return &Config{Enabled: true, ChatBase: base, APITraeBase: base, AppID: DefaultAppID}
}

func testAccount() *Account {
	return &Account{UID: "u-1", AccessToken: "tok-1", DeviceID: "dev-1"}
}

// TestCheckinStatusUnwrapsEnvelope 业务字段被 data 信封包裹时也必须取到
// （只读顶层会把已签账号误判为未签，导致重复 claim）。
func TestCheckinStatusUnwrapsEnvelope(t *testing.T) {
	srv, _ := newAPIServer(t, func(path string, _ map[string]any) (int, string) {
		if path != EndpointCheckinStatus {
			t.Errorf("意外路径 %s", path)
		}
		return 200, `{"code":0,"message":"ok","data":{"checked_in":true,"credits":1234}}`
	})
	st, err := NewClient().CheckinStatus(context.Background(), apiCfg(srv.URL), testAccount())
	if err != nil {
		t.Fatal(err)
	}
	if st.CheckedIn == nil || !*st.CheckedIn {
		t.Fatalf("checked_in 未解析: %+v", st)
	}
	if st.Credits != 1234 {
		t.Errorf("credits = %d", st.Credits)
	}
}

// TestCheckinStatusTopLevel 顶层形态同样支持；checked_in 为数字时按非 0 判定。
func TestCheckinStatusTopLevel(t *testing.T) {
	srv, _ := newAPIServer(t, func(string, map[string]any) (int, string) {
		return 200, `{"code":0,"checked_in":0,"credits":"888"}`
	})
	st, err := NewClient().CheckinStatus(context.Background(), apiCfg(srv.URL), testAccount())
	if err != nil {
		t.Fatal(err)
	}
	if st.CheckedIn == nil || *st.CheckedIn {
		t.Fatalf("checked_in=0 应判为未签: %+v", st)
	}
	if st.Credits != 888 {
		t.Errorf("字符串 credits 应被宽容解析，得到 %d", st.Credits)
	}
}

// TestCheckinAlready 预检命中已签 → 不再发 claim（省一次上游请求）。
func TestCheckinAlready(t *testing.T) {
	var claimCalled bool
	srv, _ := newAPIServer(t, func(path string, _ map[string]any) (int, string) {
		if path == EndpointCheckinClaim {
			claimCalled = true
		}
		return 200, `{"code":0,"checked_in":true}`
	})
	res, err := NewClient().Checkin(context.Background(), apiCfg(srv.URL), testAccount())
	if err != nil {
		t.Fatal(err)
	}
	if !res.Already {
		t.Errorf("应报告已签到: %+v", res)
	}
	if claimCalled {
		t.Error("已签到时不应再调 claim")
	}
}

// TestCheckinClaimReward 奖励字段提取（候选键 + 包裹层）。
func TestCheckinClaimReward(t *testing.T) {
	srv, seen := newAPIServer(t, func(path string, _ map[string]any) (int, string) {
		switch path {
		case EndpointCheckinStatus:
			return 200, `{"code":0,"checked_in":false}`
		default:
			return 200, `{"code":0,"message":"签到成功","data":{"reward_credits":150}}`
		}
	})
	res, err := NewClient().Checkin(context.Background(), apiCfg(srv.URL), testAccount())
	if err != nil {
		t.Fatal(err)
	}
	if res.Already {
		t.Fatal("未签到不应报 already")
	}
	if res.Reward != 150 {
		t.Errorf("Reward = %d, want 150（包裹层里的 reward_credits）", res.Reward)
	}
	if res.Message != "签到成功" {
		t.Errorf("Message = %q", res.Message)
	}
	if len(*seen) != 2 {
		t.Errorf("应发两次请求（status + claim），实际 %d", len(*seen))
	}
	// body 必须是 {}（上游要求空对象）
	if b, _ := json.Marshal((*seen)[1]); string(b) != "{}" {
		t.Errorf("claim body = %s, want {}", b)
	}
}

// TestQueryCredits 积分包解析：通用/Work 分类 + 剩余计算。
func TestQueryCredits(t *testing.T) {
	srv, seen := newAPIServer(t, func(path string, _ map[string]any) (int, string) {
		if path != EndpointEntUsage {
			t.Errorf("意外路径 %s", path)
		}
		return 200, `{"code":0,"data":{"user_entitlement_pack_list":[
			{"entitlement_base_info":{"product_id":208,"credits_limit":1000,"start_time":"2026-09-14 15:52:38","end_time":"2026-10-14 15:52:38"},
			 "usage":{"credits_amount":400}},
			{"entitlement_base_info":{"product_id":209,"credits_limit":500,"group_name":"会员积分"},
			 "usage":{"credits_amount":100}},
			{"entitlement_base_info":{"product_id":221,"credits_limit":300}}
		]}}`
	})
	sum, err := NewClient().QueryCredits(context.Background(), apiCfg(srv.URL), testAccount())
	if err != nil {
		t.Fatal(err)
	}
	// 208 剩 600 + 221 剩 300 = 900（通用）；209 剩 400（Work）
	if sum.General != 900 {
		t.Errorf("General = %v, want 900", sum.General)
	}
	if sum.Work != 400 {
		t.Errorf("Work = %v, want 400", sum.Work)
	}
	if sum.Total != 1300 {
		t.Errorf("Total = %v, want 1300", sum.Total)
	}
	if sum.Limit != 1800 {
		t.Errorf("Limit = %v, want 1800", sum.Limit)
	}
	if len(sum.Packs) != 3 {
		t.Fatalf("Packs = %d", len(sum.Packs))
	}
	if sum.Packs[0].EndTime != "2026-10-14 15:52:38" {
		t.Errorf("EndTime = %q", sum.Packs[0].EndTime)
	}
	// 请求体必须带 require_usage
	if (*seen)[0]["require_usage"] != true {
		t.Errorf("请求体缺 require_usage: %v", (*seen)[0])
	}
}

// TestQueryCreditsMissingList 缺字段 → 报错而非静默返回空汇总。
func TestQueryCreditsMissingList(t *testing.T) {
	srv, _ := newAPIServer(t, func(string, map[string]any) (int, string) {
		return 200, `{"code":0,"data":{}}`
	})
	if _, err := NewClient().QueryCredits(context.Background(), apiCfg(srv.URL), testAccount()); err == nil {
		t.Error("缺 user_entitlement_pack_list 应报错")
	}
}

// TestAPIBusinessCodeError 业务码非 0（含 200 + 1001 鉴权失败形态）→ APIError 并正确分类。
func TestAPIBusinessCodeError(t *testing.T) {
	srv, _ := newAPIServer(t, func(string, map[string]any) (int, string) {
		return 200, `{"code":1001,"message":"token 无效"}`
	})
	_, err := NewClient().CheckinStatus(context.Background(), apiCfg(srv.URL), testAccount())
	if err == nil {
		t.Fatal("业务码非 0 应报错")
	}
	apiErr, ok := err.(*APIError)
	if !ok {
		t.Fatalf("错误类型 = %T, want *APIError", err)
	}
	if apiErr.Code != 1001 {
		t.Errorf("Code = %d", apiErr.Code)
	}
	// 200 + 1001 不在码表里 → 归为 ErrNone（不误判为可冷却错误）
	if cls := apiErr.Classification(); cls.Kind != ErrNone {
		t.Errorf("1001 分类 = %q, want ErrNone", cls.Kind)
	}
}

// TestAPIHTTPError HTTP 层错误同样产出 APIError（供分类）。
func TestAPIHTTPError(t *testing.T) {
	srv, _ := newAPIServer(t, func(string, map[string]any) (int, string) {
		return http.StatusUnauthorized, `{"message":"unauthorized"}`
	})
	_, err := NewClient().CheckinStatus(context.Background(), apiCfg(srv.URL), testAccount())
	apiErr, ok := err.(*APIError)
	if !ok {
		t.Fatalf("错误类型 = %T", err)
	}
	if cls := apiErr.Classification(); cls.Kind != ErrSessionDead || !cls.Disable {
		t.Errorf("401 分类 = %+v, want SessionDead+Disable", cls)
	}
}

// TestAPIRequestHeaders 签到接口的头族（VSCode UA + market/session 头）。
func TestAPIRequestHeaders(t *testing.T) {
	var got http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":0}`))
	}))
	defer srv.Close()

	if _, err := NewClient().CheckinStatus(context.Background(), apiCfg(srv.URL), testAccount()); err != nil {
		t.Fatal(err)
	}
	checks := map[string]string{
		"Authorization":      "Cloud-IDE-JWT tok-1",
		"User-Agent":         checkinUserAgent,
		"X-Device-Id":        "dev-1",
		"X-User-Region":      "CN",
		"Package-Type":       "stable_cn",
		"X-Market-Client-Id": "VSCode 1.107.1",
		"App-Version":        checkinAppVersion,
	}
	for k, want := range checks {
		if v := got.Get(k); v != want {
			t.Errorf("头 %s = %q, want %q", k, v, want)
		}
	}
	if got.Get("X-Request-Id") == "" || got.Get("X-Tt-Trace-Id") == "" {
		t.Error("追踪头未设置")
	}
}

// TestUnwrapScopesAndFind 宽容解析原语。
func TestUnwrapScopesAndFind(t *testing.T) {
	body := map[string]any{
		"code": float64(0),
		"data": map[string]any{
			"result": map[string]any{"checked_in": true},
		},
	}
	scopes := unwrapScopes(body)
	if len(scopes) < 3 {
		t.Fatalf("作用域数 = %d, want >=3（顶层 + data + data.result）", len(scopes))
	}
	v, ok := findPayloadField(scopes, "checked_in")
	if !ok || v != true {
		t.Fatalf("findPayloadField = %v/%v", v, ok)
	}
	if _, ok := findPayloadField(scopes, "nope"); ok {
		t.Error("不存在的字段不应命中")
	}
}

// TestAsIntTolerant 宽容取整（含数字字符串）。
func TestAsIntTolerant(t *testing.T) {
	cases := []struct {
		in   any
		want int64
		ok   bool
	}{
		{float64(42), 42, true},
		{int64(7), 7, true},
		{"150", 150, true},
		{"", 0, false},
		{"abc", 0, false},
		{nil, 0, false},
	}
	for _, c := range cases {
		got, ok := asIntTolerant(c.in)
		if got != c.want || ok != c.ok {
			t.Errorf("asIntTolerant(%v) = %d/%v, want %d/%v", c.in, got, ok, c.want, c.ok)
		}
	}
}

// TestCreditPackClassify Work/通用分类按 product_id 动态判定（签到积分归属会变动）。
func TestCreditPackClassify(t *testing.T) {
	if !(CreditPack{ProductID: 209}).IsWork() {
		t.Error("209 应判为 Work")
	}
	for _, id := range []int64{208, 221, 0} {
		if (CreditPack{ProductID: id}).IsWork() {
			t.Errorf("%d 不应判为 Work", id)
		}
	}
	// 剩余计算不为负
	p := CreditPack{CreditsLimit: 100, CreditsUsed: 150}
	if p.Remaining() != 0 {
		t.Errorf("超额时剩余应为 0，得到 %v", p.Remaining())
	}
}
