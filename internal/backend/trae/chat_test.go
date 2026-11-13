package trae

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/gateway"
	traepkg "github.com/linguo2625469/workbuddy2api-panel/internal/trae"
)

const testSSE = "event: metadata\ndata: {\"session_id\":\"s1\"}\n\n" +
	"event: output\ndata: {\"response\":\"你好\"}\n\n" +
	"event: output\ndata: {\"response\":\"世界\"}\n\n" +
	"event: token_usage\ndata: {\"prompt_tokens\":3,\"completion_tokens\":5,\"total_tokens\":8}\n\n" +
	"event: notify_usage\ndata: {\"cn_credits_remain_info\":{\"ide_credits\":777,\"work_credits\":88}}\n\n" +
	"event: done\ndata: {\"finish_reason\":\"stop\"}\n\n"

// newChatBackend 构造指向假上游的 backend。
func newChatBackend(t *testing.T, handler http.HandlerFunc) (*Backend, *traepkg.Config) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	cfg := &traepkg.Config{
		Enabled:        true,
		ChatBase:       srv.URL,
		APITraeBase:    srv.URL,
		IDEVersion:     traepkg.DefaultIDEVersion,
		IDEVersionCode: traepkg.DefaultIDEVersionCode,
		AppID:          traepkg.DefaultAppID,
		Accounts: []traepkg.Account{{
			UID: "u1", Name: "甲", AccessToken: "tok",
			DeviceID: "1234567890123456", MachineID: "m1",
		}},
	}
	return New(traepkg.NewClient(), func() *traepkg.Config { return cfg }), cfg
}

// TestChatEndToEnd 完整链路：客户端 OpenAI 请求 → Trae payload → 上游 SSE
// → 规范化回程。验证请求改写、token 三处头、回程内容与用量。
func TestChatEndToEnd(t *testing.T) {
	var gotPath string
	var gotBody map[string]any
	var gotAuth, gotCloudide string

	b, cfg := newChatBackend(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		gotCloudide = r.Header.Get("X-Cloudide-Token")
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &gotBody)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(testSSE))
	})

	in := &gateway.Inbound{
		Raw: []byte(`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`),
		Ref: gateway.ModelRef{Realm: "cn", Bare: "glm-5.2"},
	}
	out, err := b.Chat(context.Background(), Account{A: &cfg.Accounts[0], realm: "cn"}, in)
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	defer out.Stream.Close()

	// 请求改写正确。
	if gotPath != traepkg.EndpointLLMChat {
		t.Errorf("上游路径 = %q", gotPath)
	}
	if gotBody["config_name"] != "glm-5.2" || gotBody["model"] != "glm-5.2" {
		t.Errorf("config_name/model 未改写: %v/%v", gotBody["config_name"], gotBody["model"])
	}
	if gotBody["stream"] != true {
		t.Error("stream 必须为 true")
	}
	if gotAuth != "Cloud-IDE-JWT tok" {
		t.Errorf("Authorization = %q", gotAuth)
	}
	if gotCloudide != "tok" {
		t.Errorf("X-Cloudide-Token = %q（token 必须三处同设）", gotCloudide)
	}

	// 回程：内容、用量、余额。
	var frames []string
	stats, err := out.Convert(func(f []byte) error {
		frames = append(frames, string(f))
		return nil
	})
	if err != nil {
		t.Fatalf("Convert: %v", err)
	}
	all := strings.Join(frames, "")
	if !strings.Contains(all, "你好") || !strings.Contains(all, "世界") {
		t.Errorf("内容未透出: %s", all)
	}
	if !strings.Contains(all, "data: [DONE]") {
		t.Error("缺少 [DONE]")
	}
	if !stats.SentAny {
		t.Error("SentAny 应为 true")
	}
	if stats.Usage == nil || stats.Usage.TotalTokens != 8 {
		t.Errorf("usage = %+v", stats.Usage)
	}
	if stats.Remain == nil || stats.Remain.Available != 777 || stats.Remain.Work != 88 {
		t.Errorf("remain = %+v（IDE 与 Work 必须分开）", stats.Remain)
	}
}

// TestChatInlineError 上游 200 但流内 error：Convert 要报出错误码，
// 且 SentAny 反映是否已下发内容（决定能否重试）。
func TestChatInlineError(t *testing.T) {
	sse := "event: metadata\ndata: {\"session_id\":\"s1\"}\n\n" +
		"event: error\ndata: {\"code\":4008,\"message\":\"quota exhausted\"}\n\n"
	b, cfg := newChatBackend(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK) // 用 200 掩盖拒绝
		_, _ = w.Write([]byte(sse))
	})
	in := &gateway.Inbound{
		Raw: []byte(`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`),
		Ref: gateway.ModelRef{Realm: "cn", Bare: "glm-5.2"},
	}
	out, err := b.Chat(context.Background(), Account{A: &cfg.Accounts[0], realm: "cn"}, in)
	if err != nil {
		t.Fatalf("Chat 不应在 HTTP 层报错: %v", err)
	}
	defer out.Stream.Close()
	stats, _ := out.Convert(func([]byte) error { return nil })
	if stats.ErrorCode != 4008 {
		t.Errorf("ErrorCode = %d, want 4008", stats.ErrorCode)
	}
	if stats.SentAny {
		t.Error("未下发任何内容时 SentAny 应为 false（允许换号重试）")
	}
}

// TestChatHTTPError HTTP 4xx 要走 *gateway.Failure 并带分类。
func TestChatHTTPError(t *testing.T) {
	b, cfg := newChatBackend(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"code":401,"message":"token expired"}`))
	})
	in := &gateway.Inbound{
		Raw: []byte(`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`),
		Ref: gateway.ModelRef{Realm: "cn", Bare: "glm-5.2"},
	}
	_, err := b.Chat(context.Background(), Account{A: &cfg.Accounts[0], realm: "cn"}, in)
	f := gateway.AsFailure(err)
	if f == nil {
		t.Fatalf("应返回 *gateway.Failure，got %v", err)
	}
	if f.HTTPStatus != http.StatusUnauthorized {
		t.Errorf("HTTPStatus = %d", f.HTTPStatus)
	}
	if f.Action != gateway.ActionDisable {
		t.Errorf("401 应判 ActionDisable，got %v", f.Action)
	}
	if !f.Rotate {
		t.Error("401 应允许换号")
	}
}

// TestChatRejectsWrongBackend 传错上游的账号要明确报错，而不是静默失败。
func TestChatRejectsWrongBackend(t *testing.T) {
	b, _ := newChatBackend(t, func(w http.ResponseWriter, r *http.Request) {})
	_, err := b.Chat(context.Background(), fakeAcc{}, &gateway.Inbound{})
	if err == nil {
		t.Fatal("应报错")
	}
}

type fakeAcc struct{}

func (fakeAcc) UID() string      { return "x" }
func (fakeAcc) Nickname() string { return "" }
func (fakeAcc) Realm() string    { return "cn" }
func (fakeAcc) Backend() string  { return "codebuddy" }
func (fakeAcc) Enterprise() bool { return false }
