package upstream

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// TestBuddyQuotaParsesFields 实测形态：{"affordable":5,"balance":125,"cost_per_open":10,"max_open_count":5}。
func TestBuddyQuotaParsesFields(t *testing.T) {
	c := testClient(func(r *http.Request) (*http.Response, error) {
		if r.Method != http.MethodGet {
			return nil, errors.New("want GET")
		}
		if err := travelPath(r, "/activity/growth/buddy/quota"); err != nil {
			return nil, err
		}
		return jsonResp(200, `{"code":0,"msg":"ok","data":{"affordable":5,"balance":125,"cost_per_open":10,"max_open_count":5}}`), nil
	})
	q, err := c.BuddyQuota(&auth.Auth{AccessToken: "at", UID: "u1"})
	if err != nil {
		t.Fatalf("quota: %v", err)
	}
	if q.Affordable != 5 || q.Balance != 125 || q.CostPerOpen != 10 || q.MaxOpenCount != 5 {
		t.Errorf("quota=%+v", q)
	}
}

// TestBuddyQuotaEnergyFallback 参考实现读 energy 而非 balance：两个名字都要能取到值。
func TestBuddyQuotaEnergyFallback(t *testing.T) {
	c := testClient(func(r *http.Request) (*http.Response, error) {
		return jsonResp(200, `{"code":0,"msg":"ok","data":{"affordable":2,"energy":25,"cost_per_open":10,"max_open_count":5}}`), nil
	})
	q, err := c.BuddyQuota(&auth.Auth{AccessToken: "at", UID: "u1"})
	if err != nil {
		t.Fatalf("quota: %v", err)
	}
	if q.Balance != 25 {
		t.Errorf("balance=%d want 25（energy 字段兜底）", q.Balance)
	}
}

// TestBuddyOpenSendsCountAndToken 请求体必须带 count 与形如 u-<uuid> 的 client_token。
func TestBuddyOpenSendsCountAndToken(t *testing.T) {
	var got map[string]any
	c := testClient(func(r *http.Request) (*http.Response, error) {
		if r.Method != http.MethodPost {
			return nil, errors.New("want POST")
		}
		if err := travelPath(r, "/activity/growth/buddy/open"); err != nil {
			return nil, err
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			return nil, err
		}
		return jsonResp(200, `{"code":0,"msg":"ok","data":{"buddy":{"name":"量子喵"}}}`), nil
	})
	raw, err := c.BuddyOpen(&auth.Auth{AccessToken: "at", UID: "u1"}, 3)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if n, _ := got["count"].(float64); int(n) != 3 {
		t.Errorf("count=%v want 3 (body=%v)", got["count"], got)
	}
	tok, _ := got["client_token"].(string)
	if !strings.HasPrefix(tok, "u-") || len(tok) < 10 {
		t.Errorf("client_token=%q want u-<uuid> 形态", tok)
	}
	// 载荷透传：调用方自己解析（形状随活动期变化）。
	var parsed struct {
		Buddy struct {
			Name string `json:"name"`
		} `json:"buddy"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil || parsed.Buddy.Name != "量子喵" {
		t.Errorf("raw=%s err=%v", raw, err)
	}
}

// TestBuddyOpenBusinessError 能量不足时上游返回业务错误，应归一为 *Error（调用方记日志跳过）。
func TestBuddyOpenBusinessError(t *testing.T) {
	c := testClient(func(r *http.Request) (*http.Response, error) {
		return jsonResp(400, `{"code":400,"msg":"insufficient energy"}`), nil
	})
	_, err := c.BuddyOpen(&auth.Auth{AccessToken: "at", UID: "u1"}, 1)
	var ue *Error
	if !errors.As(err, &ue) || ue.Status != 400 {
		t.Fatalf("err=%v want *Error(400)", err)
	}
}

// TestTravelStatusParsesScheduleTimes 行程时间字段（arrive_at/depart_at/server_now）解析。
func TestTravelStatusParsesScheduleTimes(t *testing.T) {
	c := testClient(func(r *http.Request) (*http.Response, error) {
		return jsonResp(200, `{"code":0,"msg":"ok","data":{"state":"arrived","record_id":28367071,
			"location":{"id":4,"name":"古镇客栈","duration_hours":2},
			"depart_at":1792803603,"arrive_at":1792810803,"server_now":1794495151,"reward_credit":8}}`), nil
	})
	st, err := c.TravelStatus(&auth.Auth{AccessToken: "at", UID: "u1"})
	if err != nil {
		t.Fatalf("travel status: %v", err)
	}
	if st.DepartAt != 1792803603 || st.ArriveAt != 1792810803 || st.ServerNow != 1794495151 {
		t.Errorf("行程时间解析错误：%+v", st)
	}
	if st.RecordID != 28367071 || st.RewardCredit != 8 {
		t.Errorf("既有字段回归：%+v", st)
	}
}
