package server

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	backendcb "github.com/linguo2625469/workbuddy2api-panel/internal/backend/codebuddy"
	"github.com/linguo2625469/workbuddy2api-panel/internal/gateway"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
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
		name string
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
			Action:  gateway.ActionModelCooldown,
			Rotate:  false,
			Kind:    "model_unknown",
			Code:    "4001",
		},
		HTTPStatus: 400,
	}
	h.handleBackendFailure(fakeGatewayAccount{uid: "u1", backend: "trae"}, model, f, &chatStat{})
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
	st := &chatStat{}

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
	h.applyFailure(acc, "glm-5.2", f, st)

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
	h.applyFailure(acc, "glm-5.2", f, &chatStat{})

	st, ok := p.Status("t1")
	if !ok {
		t.Fatal("账号应在池中")
	}
	if !st.Cooling || st.CoolKind != "hard_credit" {
		t.Errorf("plan_limit 应进入 CoolHard，实际 cooling=%v kind=%q", st.Cooling, st.CoolKind)
	}
}

// healthzNoWarnOnDeletedEndpoints 已删除的三条协议端点必须 404。
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
