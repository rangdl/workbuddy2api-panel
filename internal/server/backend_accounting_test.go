package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	backendtrae "github.com/linguo2625469/workbuddy2api-panel/internal/backend/trae"
	"github.com/linguo2625469/workbuddy2api-panel/internal/gateway"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	traepkg "github.com/linguo2625469/workbuddy2api-panel/internal/trae"
)

const traeSSEForAccounting = "event: output\ndata: {\"response\":\"hi\"}\n\n" +
	"event: token_usage\ndata: {\"prompt_tokens\":100,\"completion_tokens\":50,\"total_tokens\":150,\"cache_read_input_tokens\":80}\n\n" +
	"event: done\ndata: {\"finish_reason\":\"stop\"}\n\n"

// newAccountingHandler 构造带 Trae backend 的 handler（真实发送链路）。
func newAccountingHandler(t *testing.T) (*Handler, *traepkg.Config, *pool.Pool) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(traeSSEForAccounting))
	}))
	t.Cleanup(srv.Close)

	cfg := &traepkg.Config{
		Enabled: true, ChatBase: srv.URL, APITraeBase: srv.URL,
		IDEVersion: traepkg.DefaultIDEVersion, IDEVersionCode: traepkg.DefaultIDEVersionCode,
		AppID: traepkg.DefaultAppID,
		Accounts: []traepkg.Account{{
			UID: "t1", Name: "T", AccessToken: "tok",
			DeviceID: "1234567890123456", MachineID: "m1",
		}},
	}
	reg := gateway.NewRegistry()
	reg.Register(backendtrae.New(traepkg.NewClient(), func() *traepkg.Config { return cfg }))

	p := pool.New("")
	p.Add(backendtrae.Wrap(cfg)[0])

	h := NewHandler(Config{
		Pool:       p,
		Backends:   reg,
		Trae:       cfg,
		TraeClient: traepkg.NewClient(),
	})
	return h, cfg, p
}

// TestBackendAccountingNonStream 非流式也要记成功/失败与用量次数。
//
// 回归：dispatch 此前只调 NoteSuccess（清冷却），没调 RecordTokenUsage，
// 于是 Trae 账号在面板上「成功/失败」「用量·次数」恒为 0。
func TestBackendAccountingNonStream(t *testing.T) {
	h, _, p := newAccountingHandler(t)
	body := `{"model":"trae:cn:glm-5.2","messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body)
	}

	st, ok := p.Status("t1")
	if !ok {
		t.Fatal("账号应在池内")
	}
	if st.SuccessCount != 1 {
		t.Errorf("成功数 = %d, want 1（非流式也必须记账）", st.SuccessCount)
	}
	if st.TokenUsage.RequestCount != 1 {
		t.Errorf("用量次数 = %d, want 1", st.TokenUsage.RequestCount)
	}
	if st.TokenUsage.TotalTokens != 150 {
		t.Errorf("total_tokens = %d, want 150", st.TokenUsage.TotalTokens)
	}
	if st.LastSuccessTime.IsZero() {
		t.Error("最近成功时间应被记录")
	}
}

// TestBackendAccountingStream 流式同样记账。
func TestBackendAccountingStream(t *testing.T) {
	h, _, p := newAccountingHandler(t)
	body := `{"model":"trae:cn:glm-5.2","stream":true,"messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	st, _ := p.Status("t1")
	if st.SuccessCount != 1 {
		t.Errorf("成功数 = %d, want 1", st.SuccessCount)
	}
	if st.TokenUsage.RequestCount != 1 {
		t.Errorf("用量次数 = %d, want 1", st.TokenUsage.RequestCount)
	}
	if st.TokenUsage.TotalTokens != 150 {
		t.Errorf("total_tokens = %d, want 150", st.TokenUsage.TotalTokens)
	}
}

// TestBackendAccountingFailureCounted 失败也要计一次请求（否则重试放大看不见）。
func TestBackendAccountingFailureCounted(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"code":401,"message":"token expired"}`))
	}))
	defer srv.Close()
	cfg := &traepkg.Config{
		Enabled: true, ChatBase: srv.URL, APITraeBase: srv.URL,
		IDEVersion: traepkg.DefaultIDEVersion, IDEVersionCode: traepkg.DefaultIDEVersionCode,
		AppID: DefaultTraeAppIDForTest(),
		Accounts: []traepkg.Account{{
			UID: "t1", AccessToken: "tok", DeviceID: "1234567890123456",
		}},
	}
	reg := gateway.NewRegistry()
	reg.Register(backendtrae.New(traepkg.NewClient(), func() *traepkg.Config { return cfg }))
	p := pool.New("")
	p.Add(backendtrae.Wrap(cfg)[0])
	h := NewHandler(Config{Pool: p, Backends: reg, Trae: cfg, TraeClient: traepkg.NewClient()})

	body := `{"model":"trae:cn:glm-5.2","messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	st, _ := p.Status("t1")
	if st.SuccessCount != 0 {
		t.Errorf("失败请求不应计成功，got %d", st.SuccessCount)
	}
}

// DefaultTraeAppIDForTest 测试用 AppID。
func DefaultTraeAppIDForTest() string { return "6eefa01c-1036-4c7e-9ca5-d891f63bfcd8" }

var _ = json.Marshal
