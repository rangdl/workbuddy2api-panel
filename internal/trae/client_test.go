package trae

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestChatStreamHeaders 出站头族必须齐备（Trae 上游对头敏感，缺项会得到风控 4xx）。
func TestChatStreamHeaders(t *testing.T) {
	var got http.Header
	var gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		raw, _ := io.ReadAll(r.Body)
		gotBody = string(raw)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("event: done\ndata: {\"finish_reason\":\"stop\"}\n\n"))
	}))
	defer srv.Close()

	c := NewClient()
	cfg := &Config{ChatBase: srv.URL, IDEVersion: DefaultIDEVersion, IDEVersionCode: DefaultIDEVersionCode, AppID: DefaultAppID}
	acc := &Account{UID: "u1", AccessToken: "eyJraw", DeviceID: "dev-1", MachineID: "m-1"}

	rc, status, _, err := c.ChatStream(context.Background(), cfg, acc, []byte(`{"config_name":"x"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	if status != http.StatusOK {
		t.Fatalf("status = %d", status)
	}
	if gotBody != `{"config_name":"x"}` {
		t.Errorf("请求体未原样送达: %s", gotBody)
	}

	checks := map[string]string{
		"Authorization":        "Cloud-IDE-JWT eyJraw",
		"X-Ide-Token":          "eyJraw", // 裸 token
		"X-App-Id":             DefaultAppID,
		"X-Ide-Version":        DefaultIDEVersion,
		"X-Ide-Version-Code":   DefaultIDEVersionCode,
		"X-Device-Id":          "dev-1",
		"X-Machine-Id":         "m-1",
		"X-Device-Type":        "windows",
		"Package-Type":         "stable_cn",
		"Request-Traffic-Type": "prod",
		"User-Agent":           "TraeClient/TTNet",
		"Content-Type":         "application/json",
	}
	for k, want := range checks {
		if v := got.Get(k); v != want {
			t.Errorf("头 %s = %q, want %q", k, v, want)
		}
	}
	if got.Get("X-Tt-Trace-Id") == "" || got.Get("X-Request-Id") == "" {
		t.Error("追踪头未设置")
	}
	if !strings.HasPrefix(got.Get("Referer"), srv.URL) {
		t.Errorf("Referer = %q", got.Get("Referer"))
	}
}

// TestChatStreamUpstreamError 上游 >=400：返回 (nil, status, body)，不返回 err。
func TestChatStreamUpstreamError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"code":401,"message":"token expired"}`))
	}))
	defer srv.Close()

	c := NewClient()
	cfg := &Config{ChatBase: srv.URL}
	acc := &Account{UID: "u1", AccessToken: "t"}
	rc, status, body, err := c.ChatStream(context.Background(), cfg, acc, []byte(`{}`))
	if err != nil {
		t.Fatalf("上游 4xx 不应返回 err: %v", err)
	}
	if rc != nil {
		t.Error("错误路径不应返回响应体读取器")
	}
	if status != http.StatusUnauthorized {
		t.Errorf("status = %d", status)
	}
	if !strings.Contains(string(body), "token expired") {
		t.Errorf("错误原文未透传: %s", body)
	}
}

// TestChatStreamNoToken 无 access_token 直接报错（不发请求）。
func TestChatStreamNoToken(t *testing.T) {
	c := NewClient()
	if _, _, _, err := c.ChatStream(context.Background(), &Config{ChatBase: "http://127.0.0.1:1"}, &Account{UID: "u1"}, []byte(`{}`)); err == nil {
		t.Error("无 token 应报错")
	}
	// nil 参数
	if _, _, _, err := c.ChatStream(context.Background(), nil, nil, nil); err == nil {
		t.Error("nil config/account 应报错")
	}
}

// TestRetryAfter Retry-After 的秒数与 HTTP 日期两种形态。
func TestRetryAfter(t *testing.T) {
	h := http.Header{}
	h.Set("Retry-After", "42")
	if d := RetryAfter(h); d != 42*time.Second {
		t.Errorf("秒数形态 = %v", d)
	}
	h.Set("Retry-After", time.Now().Add(30*time.Second).UTC().Format(http.TimeFormat))
	if d := RetryAfter(h); d <= 0 || d > 31*time.Second {
		t.Errorf("日期形态 = %v", d)
	}
	h.Del("Retry-After")
	if d := RetryAfter(h); d != 0 {
		t.Errorf("缺失应为 0，得到 %v", d)
	}
	h.Set("Retry-After", "garbage")
	if d := RetryAfter(h); d != 0 {
		t.Errorf("非法应为 0，得到 %v", d)
	}
}
