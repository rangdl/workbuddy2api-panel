package server

import (
	"net/http/httptest"
	"strings"
	"testing"

	backendcb "github.com/linguo2625469/workbuddy2api-panel/internal/backend/codebuddy"
	"github.com/linguo2625469/workbuddy2api-panel/internal/gateway"
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
	// 非分类错误 → 不写响应、返回 false（调用方继续换号）。
	if wrote := h.handleBackendFailure(acc, errPlain{}, &chatStat{}); wrote {
		t.Error("非分类错误不应写出响应")
	}
	if rec.Code != 200 || rec.Body.Len() != 0 {
		t.Errorf("不应有输出: code=%d body=%s", rec.Code, rec.Body)
	}
}

type errPlain struct{}

func (errPlain) Error() string { return "network down" }

type fakeGatewayAccount struct {
	uid     string
	backend string
}

func (a fakeGatewayAccount) UID() string      { return a.uid }
func (a fakeGatewayAccount) Nickname() string { return "" }
func (a fakeGatewayAccount) Realm() string    { return "cn" }
func (a fakeGatewayAccount) Backend() string  { return a.backend }
func (a fakeGatewayAccount) Enterprise() bool { return false }

// TestShortIDNonEmpty 请求 id 生成不返回空串（chatcmpl 前缀拼接依赖它）。
func TestShortIDNonEmpty(t *testing.T) {
	if got := shortID(); got == "" {
		t.Error("shortID 不应为空")
	}
	if strings.Contains(shortID(), " ") {
		t.Error("shortID 不应含空格")
	}
}
