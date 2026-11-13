package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/trae"
)

// newTraeHandlerWithStats 构造带统计表的 handler，返回统计表以便断言。
func newTraeHandlerWithStats(t *testing.T, upstreamURL string) (*Handler, *trae.Stats) {
	t.Helper()
	stats := trae.NewStats()
	cfg := &trae.Config{
		Enabled:        true,
		ChatBase:       upstreamURL,
		IDEVersion:     trae.DefaultIDEVersion,
		IDEVersionCode: trae.DefaultIDEVersionCode,
		AppID:          trae.DefaultAppID,
		Accounts: []trae.Account{{
			UID: "u1", Name: "trae-1", AccessToken: "tok-1",
			DeviceID: "dev-1", MachineID: "machine-1",
		}},
	}
	h := NewHandler(Config{Trae: cfg, TraeClient: trae.NewClient(), TraeStats: stats})
	return h, stats
}

// TestTraeStatsRecordedNonStream 非流式成功：成功 +1、用量累计、在途归零。
func TestTraeStatsRecordedNonStream(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(traeSSE))
	}))
	defer srv.Close()

	h, stats := newTraeHandlerWithStats(t, srv.URL)
	req := httptest.NewRequest(http.MethodPost, "/trae/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body)
	}

	v := stats.Get("u1")
	if v.SuccessCount != 1 || v.ErrTotal != 0 {
		t.Errorf("成功/失败 = %d/%d, want 1/0", v.SuccessCount, v.ErrTotal)
	}
	if v.InFlight != 0 {
		t.Errorf("在途 = %d, want 0（defer 未结算）", v.InFlight)
	}
	if v.TokenUsage.TotalTokens != 8 {
		t.Errorf("total_tokens = %d, want 8", v.TokenUsage.TotalTokens)
	}
	if v.TokenUsage.PromptTokens != 3 || v.TokenUsage.CompletionTokens != 5 {
		t.Errorf("token 明细不符: %+v", v.TokenUsage)
	}
	if v.TokenUsage.LastModel != "glm-5.2" {
		t.Errorf("last_model = %q", v.TokenUsage.LastModel)
	}
	if v.LastSuccess == nil {
		t.Error("last_success 应有值")
	}
}

// TestTraeStatsRecordedStream 流式成功：同样计入成功并累计用量。
func TestTraeStatsRecordedStream(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(traeSSE))
	}))
	defer srv.Close()

	h, stats := newTraeHandlerWithStats(t, srv.URL)
	req := httptest.NewRequest(http.MethodPost, "/trae/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}

	v := stats.Get("u1")
	if v.SuccessCount != 1 {
		t.Errorf("成功数 = %d, want 1", v.SuccessCount)
	}
	if v.InFlight != 0 {
		t.Errorf("在途 = %d, want 0", v.InFlight)
	}
	if v.TokenUsage.TotalTokens != 8 {
		t.Errorf("total_tokens = %d, want 8", v.TokenUsage.TotalTokens)
	}
	// 流式的延迟记的是 TTFB，必须有值（成功且产出内容）。
	if v.LastLatencyMs <= 0 {
		t.Errorf("last_latency_ms = %d, 流式应记录 TTFB", v.LastLatencyMs)
	}
	// 吞吐不在这里断言：本地假上游秒回，生成期远低于 100ms 阈值，
	// 按设计不记录（由 TestDeltaFromStreamSkipsAbsurdTPS 覆盖该策略）。
}

// TestTraeStatsRecordedResponses Responses 非流式路径同样记账。
func TestTraeStatsRecordedResponses(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(traeSSE))
	}))
	defer srv.Close()

	h, stats := newTraeHandlerWithStats(t, srv.URL)
	body := `{"model":"glm-5.2","input":[{"role":"user","content":[{"type":"input_text","text":"hi"}]}]}`
	req := httptest.NewRequest(http.MethodPost, "/trae/v1/responses", strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body)
	}
	v := stats.Get("u1")
	if v.SuccessCount != 1 {
		t.Errorf("成功数 = %d, want 1", v.SuccessCount)
	}
	if v.TokenUsage.TotalTokens != 8 {
		t.Errorf("total_tokens = %d, want 8", v.TokenUsage.TotalTokens)
	}
}

// TestTraeStatsInlineErrorCountsAsFailure 流内首个事件即 error（额度不足等）：
// 计入失败且不留在途。这是最容易写错的一条——HTTP 200 但业务上完全失败。
func TestTraeStatsInlineErrorCountsAsFailure(t *testing.T) {
	sse := "event: metadata\ndata: {\"session_id\":\"s1\"}\n\n" +
		"event: error\ndata: {\"code\":4008,\"message\":\"quota exhausted\"}\n\n"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK) // 上游用 200 掩盖拒绝
		_, _ = w.Write([]byte(sse))
	}))
	defer srv.Close()

	h, stats := newTraeHandlerWithStats(t, srv.URL)
	req := httptest.NewRequest(http.MethodPost, "/trae/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	v := stats.Get("u1")
	if v.ErrTotal != 1 {
		t.Errorf("失败数 = %d, want 1", v.ErrTotal)
	}
	if v.SuccessCount != 0 {
		t.Errorf("成功数 = %d, want 0", v.SuccessCount)
	}
	if v.InFlight != 0 {
		t.Errorf("在途 = %d, want 0", v.InFlight)
	}
	if v.LastErr == nil {
		t.Error("last_err 应有值")
	}
}

// TestTraeStatsUpstream4xxCountsAsFailure 上游 HTTP 4xx 计失败且在途归零。
func TestTraeStatsUpstream4xxCountsAsFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"code":401,"message":"token expired"}`))
	}))
	defer srv.Close()

	h, stats := newTraeHandlerWithStats(t, srv.URL)
	req := httptest.NewRequest(http.MethodPost, "/trae/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	v := stats.Get("u1")
	if v.ErrTotal != 1 {
		t.Errorf("失败数 = %d, want 1", v.ErrTotal)
	}
	if v.InFlight != 0 {
		t.Errorf("在途 = %d, want 0", v.InFlight)
	}
}

// TestTraeStatsNoAccountNotCounted 无可用账号时不该凭空产生统计条目。
func TestTraeStatsNoAccountNotCounted(t *testing.T) {
	stats := trae.NewStats()
	cfg := &trae.Config{
		Enabled:        true,
		ChatBase:       "http://127.0.0.1:1", // 不会真的连上
		IDEVersion:     trae.DefaultIDEVersion,
		IDEVersionCode: trae.DefaultIDEVersionCode,
		AppID:          trae.DefaultAppID,
		Accounts:       nil,
	}
	h := NewHandler(Config{Trae: cfg, TraeClient: trae.NewClient(), TraeStats: stats})
	req := httptest.NewRequest(http.MethodPost, "/trae/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
	if got := stats.Snapshot(); len(got) != 0 {
		t.Errorf("无账号时不应产生统计条目: %+v", got)
	}
}

// TestTraeStatsNotifyRemainRecorded notify_usage 的余额写入统计表，供面板展示。
func TestTraeStatsNotifyRemainRecorded(t *testing.T) {
	sse := "event: output\ndata: {\"response\":\"hi\"}\n\n" +
		"event: notify_usage\ndata: {\"cn_credits_remain_info\":{\"ide_credits\":777,\"work_credits\":88}}\n\n" +
		"event: done\ndata: {\"finish_reason\":\"stop\"}\n\n"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(sse))
	}))
	defer srv.Close()

	h, stats := newTraeHandlerWithStats(t, srv.URL)
	req := httptest.NewRequest(http.MethodPost, "/trae/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}

	v := stats.Get("u1")
	if v.CreditsIDE != 777 || v.CreditsWork != 88 {
		t.Errorf("余额未记录: ide=%v work=%v", v.CreditsIDE, v.CreditsWork)
	}
	if v.CreditsAt == nil {
		t.Error("CreditsAt 应有值")
	}
	// 没有查过积分接口 → 上限未知，前端据此不画进度条。
	if v.CreditsLimit != 0 {
		t.Errorf("credits_limit 应为 0（未知），实际 %v", v.CreditsLimit)
	}
}

// TestTraeStatsDeltasFromStreamAndResponse 两个 delta 提取函数的字段映射。
func TestTraeStatsDeltasFromStream(t *testing.T) {
	res := trae.StreamResult{Usage: map[string]any{
		"prompt_tokens":     float64(10),
		"completion_tokens": float64(20),
		"total_tokens":      float64(30),
	}}
	d := deltaFromStream(res, 500*1e6, 2e9) // 500ms TTFB, 2s 总耗时
	if !d.HasPromptTokens || d.PromptTokens != 10 {
		t.Errorf("prompt = %+v", d)
	}
	if !d.HasCompletionTokens || d.CompletionTokens != 20 {
		t.Errorf("completion = %+v", d)
	}
	if !d.HasTotalTokens || d.TotalTokens != 30 {
		t.Errorf("total = %+v", d)
	}
	if !d.HasLatencyMs || d.LatencyMs != 500 {
		t.Errorf("latency = %+v", d)
	}
	if !d.HasTokensPerSecond {
		t.Error("应记录吞吐")
	}
	// 生成期 = 2s - 500ms = 1.5s；20 tokens / 1.5s ≈ 13.33 tok/s。
	// 分母刻意排除 TTFB：等首字节的时间与解码速度无关。
	if d.TokensPerSecond < 13.3 || d.TokensPerSecond > 13.4 {
		t.Errorf("吞吐 = %v, want ~13.33", d.TokensPerSecond)
	}
}

// TestDeltaFromStreamSkipsAbsurdTPS 生成期过短时不记录吞吐：
// 假上游/极短回复会让「tokens ÷ 极小耗时」除出百万级数字，展示上纯属噪声。
func TestDeltaFromStreamSkipsAbsurdTPS(t *testing.T) {
	res := trae.StreamResult{Usage: map[string]any{
		"completion_tokens": float64(340),
	}}
	// 总耗时 5ms、TTFB 2ms → 生成期 3ms，远低于 100ms 阈值。
	d := deltaFromStream(res, 2*1e6, 5*1e6)
	if d.HasTokensPerSecond {
		t.Errorf("生成期过短时不应记录吞吐，实际 %v tok/s", d.TokensPerSecond)
	}
	if !d.HasLatencyMs || d.LatencyMs != 2 {
		t.Errorf("TTFB 仍应记录: %+v", d)
	}
}

// TestDeltaFromStreamNoTPSWithoutElapsed 缺耗时信息时不硬算吞吐。
func TestDeltaFromStreamNoTPSWithoutElapsed(t *testing.T) {
	res := trae.StreamResult{Usage: map[string]any{"completion_tokens": float64(100)}}
	if d := deltaFromStream(res, 0, 0); d.HasTokensPerSecond {
		t.Errorf("无耗时时不应记录吞吐: %v", d.TokensPerSecond)
	}
}

// TestTraeStatsDeltasFromResponse usage 缺失时不产生任何 Has* 标记。
func TestTraeStatsDeltasFromResponseMissing(t *testing.T) {
	d := deltaFromResponse(map[string]any{}, "glm-5.2")
	if d.HasPromptTokens || d.HasCompletionTokens || d.HasTotalTokens {
		t.Errorf("无 usage 时不应标记字段: %+v", d)
	}
}

// TestUsageIntTolerant 上游 token 字段可能是多种数值类型。
func TestUsageIntTolerant(t *testing.T) {
	cases := []struct {
		in   any
		want int64
		ok   bool
	}{
		{float64(42), 42, true},
		{int64(42), 42, true},
		{42, 42, true},
		{"42", 42, true},
		{" 42 ", 42, true},
		{"abc", 0, false},
		{nil, 0, false},
	}
	for _, c := range cases {
		got, ok := usageInt(c.in)
		if got != c.want || ok != c.ok {
			t.Errorf("usageInt(%v) = (%d,%v), want (%d,%v)", c.in, got, ok, c.want, c.ok)
		}
	}
}
