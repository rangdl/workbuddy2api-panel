package server

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"

	backendcb "github.com/linguo2625469/workbuddy2api-panel/internal/backend/codebuddy"
	"github.com/linguo2625469/workbuddy2api-panel/internal/gateway"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
)

// TestBackendNamesEmptyWithoutRegistry 未注入注册表时前缀解析退回原语义，
// 行为与引入多上游之前完全一致。
func TestBackendNamesEmptyWithoutRegistry(t *testing.T) {
	h := NewHandler(Config{})
	if got := h.backendNames(); got != nil {
		t.Errorf("未注册时应返回 nil，got %v", got)
	}
	if h.backendFor(nil) != nil {
		t.Error("nil 账号应返回 nil backend")
	}
}

// TestBackendNamesFromRegistry 注册后别名可用（wb → codebuddy）。
func TestBackendNamesFromRegistry(t *testing.T) {
	reg := gateway.NewRegistry()
	reg.Register(backendcb.New(nil, func() bool { return false }, nil))
	h := NewHandler(Config{Backends: reg})

	// 未启用 → Names 为空（不该把 "wb:" 解析成指定上游）。
	if got := h.backendNames(); len(got) != 0 {
		t.Errorf("未启用的上游不应出现在前缀表: %v", got)
	}
}

// TestResolveModelFullTraePrefix 前缀解析：显式锁定 / 缺省自由。
func TestResolveModelFullTraePrefix(t *testing.T) {
	backends := []string{"codebuddy", "wb", "trae"}
	cases := []struct {
		in              string
		wantBackend     string
		wantRealm       string
		wantBare        string
		wantBackendExpl bool
		wantRealmExpl   bool
	}{
		{"trae:cn:glm-5.3", "trae", "cn", "glm-5.3", true, true},
		{"trae:glm-5.3", "trae", "cn", "glm-5.3", true, false},
		{"cn:glm-5.3", "", "cn", "glm-5.3", false, true},
		{"global:glm-5.3", "", "global", "glm-5.3", false, true},
		{"glm-5.3", "", "cn", "glm-5.3", false, false},
		{"wb:global:x", "wb", "global", "x", true, true},
		// 顺序无关
		{"cn:trae:glm-5.3", "trae", "cn", "glm-5.3", true, true},
		// 未知前缀不吞：整串当模型名
		{"claude-3:5", "", "cn", "claude-3:5", false, false},
	}
	for _, c := range cases {
		got := resolveModelFull(c.in, backends)
		if got.Backend != c.wantBackend || got.Realm != c.wantRealm || got.Bare != c.wantBare ||
			got.BackendExplicit != c.wantBackendExpl || got.RealmExplicit != c.wantRealmExpl {
			t.Errorf("resolveModelFull(%q) = %+v, want backend=%q realm=%q bare=%q expl=%v/%v",
				c.in, got, c.wantBackend, c.wantRealm, c.wantBare, c.wantBackendExpl, c.wantRealmExpl)
		}
	}
}

// TestBackendFailureNotWrittenAsResponse 分类失败不写响应，交给调用方换号。
func TestBackendFailureNotWrittenAsResponse(t *testing.T) {
	h := NewHandler(Config{})
	rec := httptest.NewRecorder()
	acc := fakeGatewayAccount{uid: "u1", backend: "trae"}
	// 非分类错误 → 不写响应、返回 backendRetry（调用方继续换号）。
	outcome, err := h.handleBackendFailure(acc, "glm-5.2", errPlain{}, &chatStat{}, nil)
	if outcome != backendRetry {
		t.Errorf("非分类错误应返回 backendRetry，实际 %v", outcome)
	}
	if err == nil {
		t.Error("应回传失败原因（供末端错误透传）")
	}
	if rec.Code != 200 || rec.Body.Len() != 0 {
		t.Errorf("不应有输出: code=%d body=%s", rec.Code, rec.Body)
	}
}

type errPlain struct{}

func (errPlain) Error() string { return "network down" }

// TestBackendFailureRotateTrueRetries 可换号的失败返回 backendRetry。
//
// 对照组：Trae 的 5xx（ActionCoolMedium + Rotate=true）——换号有意义。
func TestBackendFailureRotateTrueRetries(t *testing.T) {
	p := pool.New("")
	p.Add(fakeGatewayAccount{uid: "u1", backend: "trae"})
	h := NewHandler(Config{Pool: p})
	st := &chatStat{}

	f := &gateway.Failure{
		Decision: gateway.Decision{
			Action:  gateway.ActionCoolMedium,
			Rotate:  true,
			Kind:    "server",
			Message: "upstream 5xx",
		},
		HTTPStatus: 502,
	}
	outcome, err := h.handleBackendFailure(fakeGatewayAccount{uid: "u1", backend: "trae"}, "glm-5.2", f, st, nil)
	if outcome != backendRetry {
		t.Errorf("Rotate=true 应返回 backendRetry，实际 %v", outcome)
	}
	if err == nil {
		t.Error("应回传失败原因")
	}
	if st.status != 502 {
		t.Errorf("st.status = %d, want 502", st.status)
	}
}

// TestBackendFailureRotateFalseStops 不可换号的失败返回 backendStop。
//
// Trae 的 4001/4023（模型不可用）刻意标 Rotate=false：换到哪个号都是同一个模型，
// 继续轮转只会把同一个错误烧 MaxRotate 遍。此前 dispatch 恒返回 false 让调用方
// 无条件换号，这个语义完全失效。
func TestBackendFailureRotateFalseStops(t *testing.T) {
	p := pool.New("")
	p.Add(fakeGatewayAccount{uid: "u1", backend: "trae"})
	h := NewHandler(Config{Pool: p})
	st := &chatStat{}

	f := &gateway.Failure{
		Decision: gateway.Decision{
			Action:  gateway.ActionModelCooldown,
			Rotate:  false,
			Kind:    "model_unknown",
			Code:    "4001",
			Message: "model not available",
		},
		HTTPStatus: 400,
	}
	outcome, _ := h.handleBackendFailure(fakeGatewayAccount{uid: "u1", backend: "trae"}, "glm-5.2", f, st, nil)
	if outcome != backendStop {
		t.Errorf("Rotate=false 应返回 backendStop，实际 %v", outcome)
	}
}

// TestBackendFailureModelCooldownUsesRequestModel 模型级冷却必须带上真实模型名。
//
// 回归点：applyDecision 曾收到空模型名，而 BlockModelBackoff 见空模型名直接返回，
// 于是 Trae 的 4001 永远写不进负缓存——同一个 (账号, 模型) 每次请求都要重新撞上游。
func TestBackendFailureModelCooldownUsesRequestModel(t *testing.T) {
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
			Message: "model not available",
		},
		HTTPStatus: 400,
	}
	h.handleBackendFailure(fakeGatewayAccount{uid: "u1", backend: "trae"}, model, f, &chatStat{}, nil)

	// 该 (账号, 模型) 必须进入负缓存。池里只有这一个账号，所以全池口径也应被挡住。
	bs := p.ModelBlocked(model)
	if !bs.Blocked || bs.Count != 1 {
		t.Errorf("模型 %q 应进入负缓存并挡住唯一账号，实际 %+v", model, bs)
	}
}

type fakeGatewayAccount struct {
	uid     string
	backend string
}

func (a fakeGatewayAccount) UID() string      { return a.uid }
func (a fakeGatewayAccount) Nickname() string { return "" }
func (a fakeGatewayAccount) Realm() string    { return "cn" }
func (a fakeGatewayAccount) Backend() string  { return a.backend }
func (a fakeGatewayAccount) Enterprise() bool { return false }

// 能力方法：测试替身不实现上游调用，一律返回 ErrUnsupported。
func (a fakeGatewayAccount) Credits(context.Context) (*gateway.Credits, error) {
	return nil, gateway.ErrUnsupported
}
func (a fakeGatewayAccount) Checkin(context.Context) (*gateway.CheckinResult, error) {
	return nil, gateway.ErrUnsupported
}
func (a fakeGatewayAccount) Keepalive(context.Context) error { return gateway.ErrUnsupported }

// TestShortIDNonEmpty 请求 id 生成不返回空串（chatcmpl 前缀拼接依赖它）。
func TestShortIDNonEmpty(t *testing.T) {
	if got := shortID(); got == "" {
		t.Error("shortID 不应为空")
	}
	if strings.Contains(shortID(), " ") {
		t.Error("shortID 不应含空格")
	}
}
