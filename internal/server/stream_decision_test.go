package server

import (
	"bytes"
	"context"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	backendcb "github.com/linguo2625469/workbuddy2api-panel/internal/backend/codebuddy"
	backendtrae "github.com/linguo2625469/workbuddy2api-panel/internal/backend/trae"
	"github.com/linguo2625469/workbuddy2api-panel/internal/gateway"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	traepkg "github.com/linguo2625469/workbuddy2api-panel/internal/trae"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// 流内错误必须由 backend 自己填 StreamStats.Decision。
//
// 回归点：改造前 StreamStats 只有 ErrorCode/ErrorMsg，server 只能按
// `acc.Backend() == "trae"` 分流到两套分类器（applyStreamError /
// applyTraeStreamError）。加第三个上游就要加第三个分支。现在两条路径都收敛到
// 「backend 填 Decision → server 的 applyFailure」，本文件锁死两侧都真的填了。

// TestTraeConvertFillsStreamDecision 1005（套餐额度用尽）→ 长冷却 + 换号。
func TestTraeConvertFillsStreamDecision(t *testing.T) {
	const sse = "event: error\ndata: {\"code\":1005,\"message\":\"quota exhausted\"}\n\n"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(sse))
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
	be := backendtrae.New(traepkg.NewClient(), func() *traepkg.Config { return cfg })
	acc := backendtrae.Wrap(cfg, traepkg.NewClient())[0]

	out, err := be.Chat(context.Background(), acc, &gateway.Inbound{
		Raw: []byte(`{"model":"glm-5.3","stream":true,"messages":[{"role":"user","content":"hi"}]}`),
		Ref: gateway.ModelRef{Bare: "glm-5.3", Realm: "cn"}, Stream: true,
	})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	defer out.Stream.Close()
	stats, _ := out.Convert(func([]byte) error { return nil })

	if stats.Decision == nil {
		t.Fatal("Trae 流内错误未填 Decision——server 将无法处置（号不会被冷却）")
	}
	d := stats.Decision
	if d.Action != gateway.ActionCoolLong || !d.Rotate {
		t.Errorf("1005 应长冷却+换号，实际 action=%s rotate=%v", d.Action, d.Rotate)
	}
	if d.Policy != gateway.PolicyGeneric {
		t.Errorf("Trae 应走通用策略，实际 %v（CodeBuddy 策略会放大约 10 倍冷却）", d.Policy)
	}
	if d.Kind != string(traepkg.ErrPlanLimit) {
		t.Errorf("kind = %q, want %q", d.Kind, traepkg.ErrPlanLimit)
	}
}

// TestCodeBuddyConvertFillsStreamDecision 6004 限流帧 → 必带 PolicyCodeBuddy。
//
// Policy 是这条测试的重点：流内错误若走通用 applyDecision，6004 的墙钟重置
// 对齐与 softStreak 封顶会全部丢失（降级）。
func TestCodeBuddyConvertFillsStreamDecision(t *testing.T) {
	const sse = "data: {\"error\":{\"code\":6004,\"message\":\"rate limited, will reset at 2026-11-16 09:00:00\"}}\n\n"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(sse))
	}))
	t.Cleanup(srv.Close)

	client := upstream.New()
	client.ChatBaseCN = srv.URL
	be := backendcb.New(client, func() bool { return true }, nil)
	acc := backendcb.Wrap([]*auth.Auth{{UID: "u1", AccessToken: "tok"}})[0]

	out, err := be.Chat(context.Background(), acc, &gateway.Inbound{
		Raw: []byte(`{"model":"glm-5.2","stream":true,"messages":[{"role":"user","content":"hi"}]}`),
		Ref: gateway.ModelRef{Bare: "glm-5.2", Realm: "cn"}, Stream: true,
	})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	defer out.Stream.Close()
	stats, _ := out.Convert(func([]byte) error { return nil })

	if stats.Decision == nil {
		t.Fatal("CodeBuddy 流内错误未填 Decision——server 将无法处置（号不会被冷却）")
	}
	d := stats.Decision
	if d.Policy != gateway.PolicyCodeBuddy {
		t.Errorf("CodeBuddy 流内错误必须标 PolicyCodeBuddy，实际 %v（走通用策略会丢掉 6004 的墙钟对齐）", d.Policy)
	}
	if d.Action != gateway.ActionCoolShort {
		t.Errorf("6004 应短冷却，实际 action=%s", d.Action)
	}
}

// TestApplyStreamErrorUsesDecisionNotBackendName 锁死 server 侧不再按上游名分流。
//
// 用一个 backend 名既不是 "trae" 也不是 "codebuddy" 的账号：改造前它会落进
// CodeBuddy 分支（因为只判 `== "trae"`），改造后只认 Decision。
func TestApplyStreamErrorUsesDecisionNotBackendName(t *testing.T) {
	h := NewHandler(Config{})
	p := pool.New("")
	acc := fakeGatewayAccount{uid: "u1", backend: "somefuture-upstream"}
	p.Add(acc)
	h.cfg.Pool = p

	d := gateway.Decision{
		Action: gateway.ActionCoolLong, Rotate: true,
		Kind: "plan_limit", Message: "quota exhausted",
	}
	h.applyStreamError(acc, "m1", gateway.StreamStats{
		ErrorCode: 1005, ErrorMsg: "quota exhausted", Decision: &d,
	})

	st, ok := p.Status("u1")
	if !ok {
		t.Fatal("账号应在池里")
	}
	if !st.Cooling {
		t.Errorf("带 Decision 的流内错误必须冷却账号，实际 %+v", st)
	}
}

// TestApplyStreamErrorMissingDecisionDoesNotPenalize backend 忘填 Decision 时不罚号，
// 但必须留 WARN（契约违约要可观测，不能静默）。
func TestApplyStreamErrorMissingDecisionDoesNotPenalize(t *testing.T) {
	var buf bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(prev) })

	h := NewHandler(Config{})
	p := pool.New("")
	acc := fakeGatewayAccount{uid: "u1", backend: "codebuddy"}
	p.Add(acc)
	h.cfg.Pool = p

	h.applyStreamError(acc, "m1", gateway.StreamStats{ErrorCode: -1, ErrorMsg: "boom"})

	st, _ := p.Status("u1")
	if st.Cooling {
		t.Errorf("缺 Decision 不应罚号（无法判断该罚多重），实际 %+v", st)
	}
	if !strings.Contains(buf.String(), "缺少 Decision") {
		t.Errorf("契约违约必须留 WARN，实际日志:\n%s", buf.String())
	}
}
