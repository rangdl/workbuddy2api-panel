package server

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	backendcb "github.com/linguo2625469/workbuddy2api-panel/internal/backend/codebuddy"
	"github.com/linguo2625469/workbuddy2api-panel/internal/gateway"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/prompt"
	"github.com/linguo2625469/workbuddy2api-panel/internal/reqlog"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// chat_parity_test.go 聊天链路的两条路径差分测试。
//
// 背景：CodeBuddy 目前走 handler 内联（Upstream.ChatStreamContext），而
// backend/codebuddy.Backend.Chat 是只被测试覆盖的并行实现。收敛前必须先证明
// 两者对同一输入产生一致的结果，否则无法安全删除内联路径。
//
// 本文件直接对比两个「执行器」的规范化输出，不依赖 HTTP 层：
//   - 内联路径：Client.ChatStreamContext + upstream.StreamHint 的等价调用
//   - backend 路径：Backend.Chat → Outbound.Convert
//
// 覆盖 6 类典型上游行为。

// streamingBackendAccount 构造一个走 backend 路径的 CodeBuddy 账号。
func streamingBackendAccount(up *upstream.Client, a *auth.Auth) gateway.Account {
	return backendcb.WrapWithClient([]*auth.Auth{a}, up, nil)[0]
}

// collectInline 走内联路径，返回规范化后的 SSE 字节与统计。
func collectInline(t *testing.T, up *upstream.Client, a *auth.Auth, body []byte) (string, error) {
	t.Helper()
	rc, status, respBody, err := up.ChatStreamContext(context.Background(), a, body, "", upstream.ChatMeta{})
	if err != nil {
		if rc != nil {
			rc.Close()
		}
		return string(respBody), err
	}
	if status >= 400 {
		defer rc.Close()
		return string(respBody), errParityStatus(status)
	}
	defer rc.Close()
	var sb strings.Builder
	serr := upstream.StreamTo(func(frame []byte) error {
		sb.Write(frame)
		return nil
	}, rc, func(string) string { return "" })
	return sb.String(), serr
}

// collectBackend 走 backend 路径。
func collectBackend(t *testing.T, up *upstream.Client, acc gateway.Account, body []byte) (string, error) {
	t.Helper()
	// backend 必须持有与内联路径同一个 client：两者对请求体的预处理要一致，
	// 否则差异来自 client 而不是路径本身。
	be := backendcb.New(up, func() bool { return true }, nil)
	in := &gateway.Inbound{
		Raw:    body,
		Ref:    gateway.ModelRef{Bare: "glm-5.2", Realm: "cn"},
		Stream: true,
		ChatID: "chatcmpl-parity",
	}
	out, err := be.Chat(context.Background(), acc, in)
	if err != nil {
		var f *gateway.Failure
		if f = gateway.AsFailure(err); f != nil {
			return string(f.Body), errParityStatus(f.HTTPStatus)
		}
		return "", err
	}
	if out.Status >= 400 {
		b, _ := io.ReadAll(out.Stream)
		out.Stream.Close()
		return string(b), errParityStatus(out.Status)
	}
	defer out.Stream.Close()
	var sb strings.Builder
	_, cerr := out.Convert(func(frame []byte) error {
		sb.Write(frame)
		return nil
	})
	return sb.String(), cerr
}

type errParityStatus int

func (e errParityStatus) Error() string { return "upstream status error" }

// TestChatParityBothPaths 两条路径对同一上游行为产出一致的规范化 SSE。
//
// 注意：这是**收敛前的验证工具**，不是断言两条路径已经等价（它们仍有差异，
// 例如 backend 路径当前不注入 prompt_cache_key）。测试只断言「内容帧一致」，
// 用于在搬迁过程中快速发现行为漂移。
func TestChatParityBothPaths(t *testing.T) {
	body := []byte(`{"model":"glm-5.2","stream":true,"messages":[{"role":"user","content":"hi"}]}`)

	cases := []struct {
		name     string
		behavior func(string) (int, string, bool)
	}{
		{"200 正常流", func(string) (int, string, bool) { return 200, sseOK, true }},
		{"200+error 帧", func(string) (int, string, bool) {
			return 200, "data: {\"error\":{\"code\":6004,\"message\":\"rate limited\"}}\n\ndata: [DONE]\n\n", true
		}},
		{"400 硬额度", func(string) (int, string, bool) {
			return 402, `{"code":14018,"msg":"credits exhausted"}`, false
		}},
		{"429 限流", func(string) (int, string, bool) {
			return 429, `{"code":6004,"msg":"rate limited"}`, false
		}},
		{"500 服务端错误", func(string) (int, string, bool) {
			return 500, `{"msg":"internal error"}`, false
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			up := newFakeUpstream(t, tc.behavior)
			a := &auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999, Domain: "www.workbuddy.cn"}

			inlineOut, inlineErr := collectInline(t, up, a, body)
			backendOut, backendErr := collectBackend(t, up, streamingBackendAccount(up, a), body)

			// 错误类别必须一致（成功 / 同一个上游状态码）。
			if (inlineErr == nil) != (backendErr == nil) {
				t.Fatalf("错误类别不一致: inline err=%v backend err=%v", inlineErr, backendErr)
			}
			// 成功时内容帧必须一致；失败时错误体必须一致。
			if inlineErr == nil && inlineOut != backendOut {
				t.Errorf("SSE 内容不一致:\n inline=%q\nbackend=%q", inlineOut, backendOut)
			}
			if inlineErr != nil && inlineOut != backendOut {
				t.Errorf("错误体不一致:\n inline=%q\nbackend=%q", inlineOut, backendOut)
			}
		})
	}
}

// TestChatParityBackendWritesModelCooldown backend 路径的模型级冷却带真实模型名。
//
// 这是阶段 2 修复的回归保护：applyDecision 曾收到空模型名，
// BlockModelBackoff 见空名直接 return，导致 Trae 的 4001 永不写负缓存。
func TestChatParityBackendWritesModelCooldown(t *testing.T) {
	const model = "glm-5.2"
	p := pool.New("")
	p.Add(fakeGatewayAccount{uid: "u1", backend: "trae"})
	h := NewHandler(Config{Pool: p})

	f := &gateway.Failure{
		Decision: gateway.Decision{
			Action: gateway.ActionModelCooldown,
			Rotate: false,
			Kind:   "model_unknown",
			Code:   "4001",
		},
		HTTPStatus: 400,
	}
	h.handleBackendFailure(fakeGatewayAccount{uid: "u1", backend: "trae"}, model, f, &chatStat{}, nil)
	if bs := p.ModelBlocked(model); !bs.Blocked {
		t.Errorf("模型 %q 应进入负缓存，实际 %+v", model, bs)
	}
}

// TestApplyFailureRoutesCodeBuddyKindToDetailedPolicy CodeBuddy 的失败必须走
// 精细策略 applyErrorPolicy，而不是固定时长的 applyDecision。
//
// 回归点：dispatch 路径原先把所有失败都交给 applyDecision，其冷却时长只有
// 固定 1m/5m/12h。若把 CodeBuddy 也切到这条路径，6004 的墙钟重置对齐、
// 11102 的指数负缓存、softStreak 封顶这些精细语义会全部丢失（降级）。
func TestApplyFailureRoutesCodeBuddyKindToDetailedPolicy(t *testing.T) {
	h := NewHandler(Config{})
	acc := fakeGatewayAccount{uid: "u1", backend: "codebuddy"}

	// ErrModelBlocked（11102）走精细策略 → BlockModelBackoff 写负缓存。
	f := &gateway.Failure{
		Decision: gateway.Decision{
			Kind:    upstream.ErrModelBlocked.String(),
			Action:  gateway.ActionModelCooldown,
			Rotate:  false,
			Message: "11102 model not found",
		},
		HTTPStatus: 400,
	}
	p := pool.New("")
	p.Add(acc)
	h.cfg.Pool = p
	h.applyFailure(acc, "glm-5.2", f)

	if bs := p.ModelBlocked("glm-5.2"); !bs.Blocked {
		t.Errorf("ErrModelBlocked 应写模型负缓存，实际 %+v", bs)
	}
}

// TestApplyFailureNonCodeBuddyKindFallsBack Trae 的 Kind 不匹配 CodeBuddy 枚举，
// 必须回落到通用 applyDecision。
func TestApplyFailureNonCodeBuddyKindFallsBack(t *testing.T) {
	p := pool.New("")
	p.Add(fakeGatewayAccount{uid: "t1", backend: "trae"})
	h := NewHandler(Config{Pool: p})
	acc := fakeGatewayAccount{uid: "t1", backend: "trae"}

	// Trae 的 plan_limit → ActionCoolLong + 12h 固定冷却。
	f := &gateway.Failure{
		Decision: gateway.Decision{
			Kind:    "plan_limit",
			Action:  gateway.ActionCoolLong,
			Rotate:  true,
			Message: "1005 quota exhausted",
		},
		HTTPStatus: 402,
	}
	h.applyFailure(acc, "glm-5.2", f)

	st, ok := p.Status("t1")
	if !ok {
		t.Fatal("账号应在池中")
	}
	if !st.Cooling || st.CoolKind != "hard_credit" {
		t.Errorf("plan_limit 应进入 CoolHard，实际 cooling=%v kind=%q", st.Cooling, st.CoolKind)
	}
}

// TestUnifiedPathServesCodeBuddy CodeBuddy 现在也走 backend 分发。
//
// 这是收敛完成的验收点：内联路径已删除，若 backend 注册或 Unwrap 有任何问题，
// 所有 CodeBuddy 请求都会失败。用最简单的 200 流做冒烟。
func TestUnifiedPathServesCodeBuddy(t *testing.T) {
	up := newFakeUpstream(t, func(string) (int, string, bool) { return 200, sseOK, true })
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: up,
	})

	// 流式
	req := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("stream: code=%d body=%s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "你好") {
		t.Errorf("stream body missing content: %s", rec.Body)
	}

	// 非流式
	req = httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","stream":false,"messages":[{"role":"user","content":"hi"}]}`))
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("sync: code=%d body=%s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "你好") {
		t.Errorf("sync body missing content: %s", rec.Body)
	}
}

// TestUnifiedPathRecordsUsage 统一路径必须记账（token / 请求数 / 缓存）。
//
// 收敛过程中最容易漏的就是记账——它原来散在内联路径的 recordAttempt 里，
// 删掉内联路径后必须由 backend 路径补上，否则面板「用量」整页归零。
func TestUnifiedPathRecordsUsage(t *testing.T) {
	up := newFakeUpstream(t, func(string) (int, string, bool) { return 200, sseOK, true })
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up})

	req := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}

	st, ok := p.Status("u1")
	if !ok {
		t.Fatal("account missing")
	}
	if st.TokenUsage.RequestCount != 1 {
		t.Errorf("RequestCount=%d, want 1", st.TokenUsage.RequestCount)
	}
	if st.TokenUsage.UsageCount != 1 {
		t.Errorf("UsageCount=%d, want 1（token 观测必须进账号级计数）", st.TokenUsage.UsageCount)
	}
	if st.TokenUsage.TotalTokens != 2 {
		t.Errorf("TotalTokens=%d, want 2", st.TokenUsage.TotalTokens)
	}
	if st.SuccessCount != 1 {
		t.Errorf("SuccessCount=%d, want 1", st.SuccessCount)
	}
}

// TestHealthzNoWarnOnDeletedEndpoints 已删除的三条协议端点必须 404。
//
// 阶段 1 的回归保护：确保删除是完整的（路由不再注册）。
func TestDeletedEndpointsReturn404(t *testing.T) {
	h := NewHandler(Config{Pool: testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999})})
	for _, path := range []string{
		"/v1/responses",
		"/v1/messages",
		"/v1/v1/messages",
		"/tp/v1/chat/completions",
		"/tp/v1/models",
	} {
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{}`))
		req.Header.Set("Authorization", "Bearer k")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s status=%d, want 404", path, rec.Code)
		}
	}
}

// ── 聊天链路收敛的语义保留回归 ──────────────────────────────────────
//
// 这三条都是「删除内联路径时被漏掉」的行为，必须钉住：
// 收敛后它们不再有内联实现兜底，只能靠 dispatch 路径自己保证。

// TestWafIpFailFastStillArmed WAF IP 级 fail-fast 的计数入口必须仍被调用。
//
// 回归点：noteWaf 原先只在旧内联路径的 WAF 处置分支里调用，删掉内联后
// 零调用——IP 级拦截判定彻底失效，网关会持续换号打同一出口 IP。
func TestWafIpFailFastStillArmed(t *testing.T) {
	var hits int
	up := &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			hits++
			return &http.Response{
				StatusCode: 403,
				Header:     http.Header{"Content-Type": []string{"text/html"}},
				Body:       io.NopCloser(strings.NewReader(`<html>blocked by waf</html>`)),
			}, nil
		})},
		ChatBaseCN: "https://fake.example",
	}
	p := testPoolWith(
		&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999},
		&auth.Auth{UID: "u2", AccessToken: "at2", ExpiresAt: 9999999999},
	)
	h := NewHandler(Config{Pool: p, Upstream: up})

	req := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	// 两个不同账号都撞 WAF 403 → 应激活 IP 级判定并终止轮转，
	// 而不是把 MaxRotate 次全打完。
	if hits >= 3 {
		t.Errorf("upstream hits=%d（WAF IP 级 fail-fast 未生效，轮转打满了）", hits)
	}
	if !h.wafIP.active() {
		t.Error("两个不同账号 WAF 403 后应激活 IP 级拦截判定")
	}
}

// TestEmptyUpstreamStreamMarkedAsError 200 + 零有效帧必须标成流错误。
//
// 回归点：旧内联路径用 IsEmptyStreamError 单独判定并收敛到 502 观测；
// 删掉内联后若不补，失败流会被记成 200 成功（运维看到假成功）。
func TestEmptyUpstreamStreamMarkedAsError(t *testing.T) {
	const emptySSE = "data: [DONE]\n\n"
	up := newFakeUpstream(t, func(string) (int, string, bool) { return 200, emptySSE, true })
	reqLog := reqlog.New(reqlog.Config{})
	h := NewHandler(Config{
		Pool:       testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream:   up,
		RequestLog: reqLog,
	})

	req := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	s := reqLog.Snapshot()
	if s.Completed == 0 || len(s.Recent) == 0 {
		t.Fatal("应有请求日志事件")
	}
	e := s.Recent[len(s.Recent)-1]
	// HTTP 状态码只能是 200（头已在开流时发出，无法回改），但**观测口径**
	// 必须收敛到失败：outcome=stream_error 且 OK=false。
	if e.OK {
		t.Errorf("空流不应标记成功，实际 status=%d OK=%v outcome=%s", e.Status, e.OK, e.Outcome)
	}
	if e.Outcome != reqlog.OutcomeStreamError {
		t.Errorf("空流 outcome=%s, want %s", e.Outcome, reqlog.OutcomeStreamError)
	}
}

// TestClientDisconnectMarkedInterrupted 客户端断连不能记成 success。
//
// 回归点：旧内联路径对写失败单独归为 OutcomeInterrupted。收敛后若不区分，
// 断连会被记成成功，污染成功率统计。
func TestClientDisconnectMarkedInterrupted(t *testing.T) {
	// 用一个写到一半就报错的 ResponseWriter 模拟客户端断连。
	up := newFakeUpstream(t, func(string) (int, string, bool) { return 200, sseOK, true })
	reqLog := reqlog.New(reqlog.Config{})
	h := NewHandler(Config{
		Pool:       testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream:   up,
		RequestLog: reqLog,
	})

	req := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	rec := &brokenWriter{ResponseRecorder: httptest.NewRecorder()}
	h.ServeHTTP(rec, req)

	s := reqLog.Snapshot()
	if s.Completed == 0 || len(s.Recent) == 0 {
		t.Fatal("应有请求日志事件")
	}
	e := s.Recent[len(s.Recent)-1]
	if e.Outcome == reqlog.OutcomeSuccess {
		t.Errorf("客户端断连不应记为 success，实际 outcome=%s", e.Outcome)
	}
}

// brokenWriter 第一次 Write 就失败，模拟客户端已断开。
type brokenWriter struct {
	*httptest.ResponseRecorder
	writes int
}

func (b *brokenWriter) Write(p []byte) (int, error) {
	b.writes++
	if b.writes > 1 {
		return 0, io.ErrClosedPipe
	}
	return b.ResponseRecorder.Write(p)
}

func (b *brokenWriter) Flush() {}

// TestTraeStatsStillRecorded Trae 账号的运行期统计必须仍被写入。
//
// 回归点：旧 `internal/server/trae.go` 在请求入口调 `h.traeStats.Begin`
// （在途+1，结束时结算）。9a55778 删掉独立 Trae 端点时连带删了这两处调用，
// 统一链路没补回——面板「Trae 签到」区块的 成功/失败/在途 三列因此恒为 0。
func TestTraeStatsStillRecorded(t *testing.T) {
	h, _, _, stats := newAccountingHandler(t)

	body := `{"model":"trae:cn:glm-5.2","messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	v := stats.Get("t1")
	if v.InFlight != 0 {
		t.Errorf("请求结束后在途=%d, want 0（finish 未结算）", v.InFlight)
	}
	if v.SuccessCount == 0 && v.ErrTotal == 0 {
		t.Error("成功/失败计数都未增加，统计链路断了")
	}
}

// TestDegradedRetryWorksWithInjectedRegistry 生产形态（显式注入 Registry）也必须能降级重试。
//
// 回归点：ensureCodeBuddyBackend 的兼容垫片在「已有注册表」时直接 return，
// 而 main 用 backendcb.New 装配（不设 Retry）。于是测试走垫片（有 Retry）、
// 生产走显式装配（无 Retry）——内容拦截误报后直接 400，降级重试完全失效。
// 这条用例用 NewCodeBuddyBackend 复刻生产装配，断言降级重试真的发生。
func TestDegradedRetryWorksWithInjectedRegistry(t *testing.T) {
	var bodies [][]byte
	up := &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			raw, _ := io.ReadAll(r.Body)
			bodies = append(bodies, raw)
			if len(bodies) == 1 {
				return &http.Response{
					StatusCode: 400,
					Header:     http.Header{"Content-Type": []string{"application/json"}},
					Body:       io.NopCloser(strings.NewReader(`{"code":"11-128","msg":"blocked by security policy"}`)),
				}, nil
			}
			return &http.Response{
				StatusCode: 200,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:       io.NopCloser(strings.NewReader(sseOK)),
			}, nil
		})},
		ChatBaseCN: "https://fake.example",
	}
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	gate := NewDegradeGate()
	reg := gateway.NewRegistry()
	reg.Register(NewCodeBuddyBackend(up, func() bool { return true }, nil, gate, "passthrough"))

	h := NewHandler(Config{
		Pool:        p,
		Upstream:    up,
		Backends:    reg,
		DegradeGate: gate,
		PromptMode:  "passthrough",
	})

	req := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","stream":true,"messages":[{"role":"system","content":"原始指纹"},{"role":"user","content":"hi"}]}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s（生产装配下应降级重试成功）", rec.Code, rec.Body)
	}
	if len(bodies) != 2 {
		t.Fatalf("应打 2 次上游（首次 400 + 降级重试），实际 %d", len(bodies))
	}
	if !strings.Contains(string(bodies[1]), prompt.Degraded) {
		t.Errorf("第二次出站 body 应使用 Degraded 提示词: %s", bodies[1])
	}
	if !gate.Active() {
		t.Error("降级状态机应被触发（否则后续请求仍会先撞 400）")
	}
}

// TestModelBlockedRotatesToNextAccount 11158（账号类型级权限拒绝）必须换号。
//
// 回归点：ErrModelBlocked 曾被标 Rotate=false，dispatch 直接 backendStop——
// 而 11158 是**账号类型**级拒绝，同域另一个有权限的号本来能成功。
// 默认部署（cross_domain_fallback 关闭）会让「首个号 11158」直接 400。
func TestModelBlockedRotatesToNextAccount(t *testing.T) {
	var calls int
	up := newFakeUpstream(t, func(auth string) (int, string, bool) {
		calls++
		if auth == "Bearer at-bad" {
			return 400, `{"code":11158,"msg":"该账号类型无此模型权限"}`, false
		}
		return 200, sseOK, true
	})
	p := testPoolWith(
		&auth.Auth{UID: "bad", AccessToken: "at-bad", ExpiresAt: 9999999999},
		&auth.Auth{UID: "good", AccessToken: "at-good", ExpiresAt: 9999999999},
	)
	h := NewHandler(Config{Pool: p, Upstream: up})

	req := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","stream":false,"messages":[{"role":"user","content":"hi"}]}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s（应换到 good 号成功）", rec.Code, rec.Body)
	}
	if calls < 2 {
		t.Errorf("上游调用 %d 次，应至少 2 次（bad 11158 → good 成功）", calls)
	}
	// 注意：good 号成功后会对**自己的能力**调 BlockModelClear，但 bad 号的
	// 负缓存按 (账号,模型) 独立存储，仍应保留——它下次选号要避开。
	// 这里只能间接验证：bad 号被 tried 排除且请求最终成功，说明它确实被判过。
	st, ok := p.Status("bad")
	if !ok {
		t.Fatal("bad 号应在池中")
	}
	if st.SuccessCount != 0 {
		t.Errorf("bad 号不应记成功，实际 %d", st.SuccessCount)
	}
}

// TestTraeFailureNotRoutedToCodeBuddyPolicy Trae 的失败不得走 CodeBuddy 精细策略。
//
// 回归点：applyFailure 曾按 Decision.Kind **字符串**反查 CodeBuddy 的 ErrKind。
// Trae 的 soft_rate / session_dead / server / client 与 CodeBuddy 枚举逐字同名，
// 于是 Trae 的 429 用了 CodeBuddy 的 softCooldown（默认 600s）而不是自己的 60s，
// 5xx 也不再中冷却（applyErrorPolicy 的 ErrServer 只 NoteError）。
func TestTraeFailureNotRoutedToCodeBuddyPolicy(t *testing.T) {
	p := pool.New("")
	p.Add(fakeGatewayAccount{uid: "t1", backend: "trae"})
	h := NewHandler(Config{Pool: p})
	acc := fakeGatewayAccount{uid: "t1", backend: "trae"}

	// Trae 的 soft_rate：ActionCoolShort，Cooldown 60s（Trae 自己的口径）。
	f := &gateway.Failure{
		Decision: gateway.Decision{
			Kind:     "soft_rate",
			Policy:   gateway.PolicyGeneric, // Trae 的 Decide 不设 Policy
			Action:   gateway.ActionCoolShort,
			Cooldown: 60 * time.Second,
			Rotate:   true,
			Message:  "429 rate limited",
		},
		HTTPStatus: 429,
	}
	h.applyFailure(acc, "glm-5.2", f)

	st, ok := p.Status("t1")
	if !ok {
		t.Fatal("账号应在池中")
	}
	if !st.Cooling {
		t.Fatal("Trae 的 429 应让账号进入冷却")
	}
	// 60s 而非 600s：若走了 CodeBuddy 策略，冷却会是 h.softCooldown()（默认 600s）。
	if st.CoolRemaining > 120 {
		t.Errorf("冷却剩余 %ds，应接近 60s（说明被路由到了 CodeBuddy 的 softCooldown）", st.CoolRemaining)
	}
}

// TestCodeBuddyFailureUsesDetailedPolicy CodeBuddy 的失败仍走精细策略（对照组）。
func TestCodeBuddyFailureUsesDetailedPolicy(t *testing.T) {
	p := pool.New("")
	p.Add(fakeGatewayAccount{uid: "c1", backend: "codebuddy"})
	h := NewHandler(Config{Pool: p})
	acc := fakeGatewayAccount{uid: "c1", backend: "codebuddy"}

	// CodeBuddy 的 ErrModelBlocked：走 applyErrorPolicy → BlockModelBackoff。
	f := &gateway.Failure{
		Decision: gateway.Decision{
			Kind:   upstream.ErrModelBlocked.String(),
			Policy: gateway.PolicyCodeBuddy,
			Action: gateway.ActionModelCooldown,
			Rotate: true,
		},
		HTTPStatus: 400,
	}
	h.applyFailure(acc, "glm-5.2", f)

	if bs := p.ModelBlocked("glm-5.2"); !bs.Blocked {
		t.Error("CodeBuddy 的 ErrModelBlocked 应写模型负缓存（精细策略）")
	}
}
