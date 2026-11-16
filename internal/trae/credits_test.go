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
	var claimed bool
	srv, seen := newAPIServer(t, func(path string, _ map[string]any) (int, string) {
		switch path {
		case EndpointCheckinStatus:
			// 回查确认会再查一次 status：claim 之后 checked_in 必须翻转，
			// 否则会被判 unverified（模拟真实上游的"到账"）。
			if claimed {
				return 200, `{"code":0,"checked_in":true}`
			}
			return 200, `{"code":0,"checked_in":false}`
		default:
			claimed = true
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
	if res.Unverified {
		t.Fatalf("回查已翻转，不应判未确认: %+v", res)
	}
	if res.Reward != 150 {
		t.Errorf("Reward = %d, want 150（包裹层里的 reward_credits）", res.Reward)
	}
	if res.Message != "签到成功" {
		t.Errorf("Message = %q", res.Message)
	}
	// status → claim → status（回查确认）
	if len(*seen) != 3 {
		t.Errorf("应发三次请求（status + claim + 回查 status），实际 %d", len(*seen))
	}
	// body 必须是 {}（上游要求空对象）
	if b, _ := json.Marshal((*seen)[1]); string(b) != "{}" {
		t.Errorf("claim body = %s, want {}", b)
	}
}

// TestCheckinDeviceAlreadyFromClaim 9095（设备今日已签）→ Already + DeviceAlready，
// 不是错误（这是多账号共用设备号时最常见的返回）。
func TestCheckinDeviceAlreadyFromClaim(t *testing.T) {
	srv, _ := newAPIServer(t, func(path string, _ map[string]any) (int, string) {
		if path == EndpointCheckinStatus {
			return 200, `{"code":0,"checked_in":false}`
		}
		return 200, `{"code":9095,"message":"当前设备今日已经签到，请明日再来哦～"}`
	})
	res, err := NewClient().Checkin(context.Background(), apiCfg(srv.URL), testAccount())
	if err != nil {
		t.Fatalf("9095 不应返回错误: %v", err)
	}
	if !res.Already || !res.DeviceAlready {
		t.Fatalf("res = %+v, want Already+DeviceAlready", res)
	}
	if res.Message == "" {
		t.Error("Message 应保留上游原文")
	}
}

// TestCheckinAlreadyFromStatusDidCheckedIn status 只回 did_checked_in=true（设备维度）
// 时也应判为已签，避免对已签设备重复 claim。
func TestCheckinAlreadyFromStatusDidCheckedIn(t *testing.T) {
	var claimCalled bool
	srv, _ := newAPIServer(t, func(path string, _ map[string]any) (int, string) {
		if path == EndpointCheckinClaim {
			claimCalled = true
		}
		return 200, `{"code":0,"did_checked_in":true}`
	})
	res, err := NewClient().Checkin(context.Background(), apiCfg(srv.URL), testAccount())
	if err != nil {
		t.Fatal(err)
	}
	if !res.Already || !res.DeviceAlready {
		t.Fatalf("res = %+v, want Already+DeviceAlready", res)
	}
	if claimCalled {
		t.Error("did_checked_in=true 时不应再 claim")
	}
}

// TestCheckinUnverified claim 成功但回查未翻转 → Unverified（不能报成功）。
func TestCheckinUnverified(t *testing.T) {
	srv, _ := newAPIServer(t, func(path string, _ map[string]any) (int, string) {
		if path == EndpointCheckinStatus {
			return 200, `{"code":0,"checked_in":false}` // 回查仍为 false
		}
		return 200, `{"code":0,"data":{"reward":150}}`
	})
	res, err := NewClient().Checkin(context.Background(), apiCfg(srv.URL), testAccount())
	if err != nil {
		t.Fatal(err)
	}
	if !res.Unverified {
		t.Fatalf("res = %+v, want Unverified", res)
	}
}

// TestCheckinVerifyDisabled VerifyClaim=false 时不做回查（只发 status + claim）。
func TestCheckinVerifyDisabled(t *testing.T) {
	cfgOff := false
	var claimCalled bool
	srv, seen := newAPIServer(t, func(path string, _ map[string]any) (int, string) {
		if path == EndpointCheckinStatus {
			return 200, `{"code":0,"checked_in":false}`
		}
		claimCalled = true
		return 200, `{"code":0,"data":{"reward":150}}`
	})
	cfg := apiCfg(srv.URL)
	cfg.VerifyClaim = &cfgOff
	res, err := NewClient().Checkin(context.Background(), cfg, testAccount())
	if err != nil {
		t.Fatal(err)
	}
	if !claimCalled || res.Unverified || res.Reward != 150 {
		t.Fatalf("res = %+v", res)
	}
	if len(*seen) != 2 {
		t.Errorf("请求数 = %d, want 2（关闭回查）", len(*seen))
	}
}

// TestQueryCredits 用**实测结构**（dsh 的 USAGE_API_RESEARCH 记录）验证解析。
//
// 关键点：credits_limit 在 entitlement_base_info.quota 里，不在 base 顶层；
// 会员包还要再深一层 product_extra.package_extra.quota。
func TestQueryCredits(t *testing.T) {
	srv, seen := newAPIServer(t, func(path string, _ map[string]any) (int, string) {
		if path != EndpointEntUsage {
			t.Errorf("意外路径 %s", path)
		}
		return 200, `{"code":0,"data":{
			"usage_summary":{"total_amount":1800,"consumed_amount":500,"consumption_ratio":0.27},
			"user_entitlement_pack_list":[
			{"entitlement_base_info":{"product_id":208,"quota":{"credits_limit":1000},
			  "start_time":"2026-09-14 15:52:38","end_time":"2026-10-14 15:52:38"},
			 "usage":{"credits_amount":400}},
			{"entitlement_base_info":{"product_id":209,"quota":{"credits_limit":500},
			  "display_desc":"会员积分"},
			 "usage":{"credits_amount":100}},
			{"entitlement_base_info":{"product_id":221,"quota":{"credits_limit":300}}},
			{"entitlement_base_info":{"product_id":208,
			  "product_extra":{"package_extra":{"quota":{"credits_limit":200}}}},
			 "usage":{"credits_amount":50}}
		]}}`
	})
	sum, err := NewClient().QueryCredits(context.Background(), apiCfg(srv.URL), testAccount())
	if err != nil {
		t.Fatal(err)
	}
	// 208: 600 + 150(package_extra 200-50) = 750；221: 300 → 通用 1050
	if sum.General != 1050 {
		t.Errorf("General = %v, want 1050", sum.General)
	}
	if sum.Work != 400 {
		t.Errorf("Work = %v, want 400", sum.Work)
	}
	// Limit/Total 取上游 usage_summary（1800 / 1300）：逐包求和会漏包。
	if sum.Limit != 1800 || sum.Total != 1300 {
		t.Errorf("Limit/Total = %v/%v, want 1800/1300", sum.Limit, sum.Total)
	}
	if !sum.TotalFromSummary {
		t.Error("TotalFromSummary 应为 true")
	}
	if len(sum.Packs) != 4 {
		t.Fatalf("Packs = %d, want 4", len(sum.Packs))
	}
	if sum.Packs[0].EndTime != "2026-10-14 15:52:38" {
		t.Errorf("EndTime = %q", sum.Packs[0].EndTime)
	}
	// package_extra 里的额度必须被读到（旧实现读不到 → 该包 0 额度被丢弃）。
	if sum.Packs[3].CreditsLimit != 200 {
		t.Errorf("package_extra 额度未解析: %+v", sum.Packs[3])
	}
	if (*seen)[0]["require_usage"] != true {
		t.Errorf("请求体缺 require_usage: %v", (*seen)[0])
	}
}

// TestQueryCreditsFallsBackToAlias 主端点无 pack 列表时走别名端点。
func TestQueryCreditsFallsBackToAlias(t *testing.T) {
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == EndpointEntUsageAlias {
			_, _ = w.Write([]byte(`{"code":0,"user_entitlement_pack_list":[
				{"entitlement_base_info":{"product_id":208,"quota":{"credits_limit":100}},
				 "usage":{"credits_amount":10}}]}`))
			return
		}
		// 主端点：200 但没有 pack 列表（旧 ide_user_ent_usage 的真实表现）
		_, _ = w.Write([]byte(`{"code":0,"data":{}}`))
	}))
	defer srv.Close()

	sum, err := NewClient().QueryCredits(context.Background(), apiCfg(srv.URL), testAccount())
	if err != nil {
		t.Fatalf("应回落到别名端点: %v", err)
	}
	if len(paths) != 2 || paths[1] != EndpointEntUsageAlias {
		t.Errorf("请求路径 = %v", paths)
	}
	if sum.General != 90 {
		t.Errorf("General = %v, want 90", sum.General)
	}
}

// TestQueryCreditsNoUsageSummary usage_summary 缺失时退回逐包求和。
func TestQueryCreditsNoUsageSummary(t *testing.T) {
	srv, _ := newAPIServer(t, func(string, map[string]any) (int, string) {
		return 200, `{"code":0,"user_entitlement_pack_list":[
			{"entitlement_base_info":{"product_id":208,"quota":{"credits_limit":100}},
			 "usage":{"credits_amount":40}}]}`
	})
	sum, err := NewClient().QueryCredits(context.Background(), apiCfg(srv.URL), testAccount())
	if err != nil {
		t.Fatal(err)
	}
	if sum.TotalFromSummary {
		t.Error("无 usage_summary 时 TotalFromSummary 应为 false")
	}
	if sum.Limit != 100 || sum.Total != 60 {
		t.Errorf("Limit/Total = %v/%v, want 100/60", sum.Limit, sum.Total)
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

// TestAPIRequestHeaders 签到接口的头族（2026-11 合并客户端形态的 UA + market/session 头）。
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
		"User-Agent":         "Trae/0.1.52",
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

// TestCheckinHeaderOverrides 配置覆盖 UA（回退旧形态的出口）。
func TestCheckinHeaderOverrides(t *testing.T) {
	var got http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":0}`))
	}))
	defer srv.Close()

	cfg := apiCfg(srv.URL)
	cfg.CheckinUserAgent = LegacyCheckinUserAgent
	if _, err := NewClient().CheckinStatus(context.Background(), cfg, testAccount()); err != nil {
		t.Fatal(err)
	}
	if v := got.Get("User-Agent"); v != LegacyCheckinUserAgent {
		t.Errorf("User-Agent = %q, want %q", v, LegacyCheckinUserAgent)
	}
	// X-Market-Client-Id 不随 UA 覆盖变化（保持真实指纹）。
	if v := got.Get("X-Market-Client-Id"); v != "VSCode 1.107.1" {
		t.Errorf("X-Market-Client-Id = %q", v)
	}
}

// TestCheckinReqSourceOverride req_source 缺省发 {}，配置后发 {"req_source":N}。
func TestCheckinReqSourceOverride(t *testing.T) {
	srv, seen := newAPIServer(t, func(string, map[string]any) (int, string) {
		return 200, `{"code":0,"checked_in":true}`
	})
	cfg := apiCfg(srv.URL)
	cfg.CheckinReqSource = 2
	if _, err := NewClient().CheckinStatus(context.Background(), cfg, testAccount()); err != nil {
		t.Fatal(err)
	}
	if len(*seen) != 1 {
		t.Fatalf("请求数 = %d", len(*seen))
	}
	if v, ok := (*seen)[0]["req_source"]; !ok || v != float64(2) {
		t.Errorf("body = %v, want req_source=2", (*seen)[0])
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

// TestCreditsSummaryJSONKeys 序列化用小写字段名（前端读 c.general）。
//
// 默认序列化会用 Go 字段名（General/Work/...），前端拿到 undefined，
// Math.round(undefined) → NaN，正是面板显示 NaN 的根因。
func TestCreditsSummaryJSONKeys(t *testing.T) {
	sum := CreditsSummary{General: 1.5, Work: 2, Total: 3.5, Limit: 10,
		Packs: []CreditPack{{ProductID: 208, CreditsLimit: 10}}}
	b, err := json.Marshal(sum)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"general", "work", "total", "limit", "packs"} {
		if _, ok := m[k]; !ok {
			t.Errorf("缺少小写字段 %q（实际: %s）", k, b)
		}
	}
	if _, ok := m["General"]; ok {
		t.Errorf("不应输出大写字段名: %s", b)
	}
	packs := m["packs"].([]any)
	pack := packs[0].(map[string]any)
	for _, k := range []string{"product_id", "credits_limit"} {
		if _, ok := pack[k]; !ok {
			t.Errorf("积分包缺少小写字段 %q: %v", k, pack)
		}
	}
}

// TestAsStringTolerant 上游时间字段可能是字符串或数字。
//
// 回归：旧的 str() 只做 v.(string) 断言，数字直接返回空串，
// 于是面板上 Trae 的发放/到期日期全是空白（实测确认 base.start_time
// 与 base.end_time 在某些账号下是数字时间戳）。
func TestAsStringTolerant(t *testing.T) {
	cases := []struct {
		in   any
		want string
	}{
		{"2026-09-14 15:52:38", "2026-09-14 15:52:38"},
		{"  1866", "1866"},                  // 去空白
		{float64(1790000000), "1790000000"}, // Unix 秒
		{float64(1790000000.5), "1790000000.5"},
		{int64(42), "42"},
		{42, "42"},
		{nil, ""},
		{map[string]any{}, ""},
	}
	for _, c := range cases {
		if got := asString(c.in); got != c.want {
			t.Errorf("asString(%v) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestParseCreditPackNumericTime 数字类型的 start_time/end_time 也要解析出来。
func TestParseCreditPackNumericTime(t *testing.T) {
	pack := map[string]any{
		"entitlement_base_info": map[string]any{
			"product_id": float64(208),
			"quota":      map[string]any{"credits_limit": float64(500)},
			"usage":      map[string]any{"credits_amount": float64(100)},
			"start_time": float64(1790000000),
			"end_time":   float64(1792592000),
		},
	}
	p := parseCreditPack(pack)
	if p.StartTime != "1790000000" || p.EndTime != "1792592000" {
		t.Errorf("数字时间戳未解析: start=%q end=%q", p.StartTime, p.EndTime)
	}
}
