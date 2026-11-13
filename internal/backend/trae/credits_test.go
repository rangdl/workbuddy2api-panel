package trae

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/gateway"
	traepkg "github.com/linguo2625469/workbuddy2api-panel/internal/trae"
)

// TestCreditsUsesGeneralNotTotal 关键正确性：Available 必须取 General
// （product_id != 209），不能用 Total——Total 含 Work 积分，而对话只消耗
// IDE 额度。用 Total 会让快耗尽的账号看起来健康。
func TestCreditsUsesGeneralNotTotal(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// 通用积分只剩 10，Work 积分还有 5000，Total = 5010。
		_ = json.NewEncoder(w).Encode(map[string]any{
			"code": 0,
			"data": map[string]any{
				"user_entitlement_pack_list": []any{
					map[string]any{
						"entitlement_base_info": map[string]any{
							"product_id": 208,
							"quota":      map[string]any{"credits_limit": 100},
							"usage":      map[string]any{"credits_amount": 90},
							"end_time":   "2026-12-31 23:59:59",
						},
					},
					map[string]any{
						"entitlement_base_info": map[string]any{
							"product_id": 209, // Work
							"quota":      map[string]any{"credits_limit": 5000},
							"usage":      map[string]any{"credits_amount": 0},
						},
					},
				},
			},
		})
	}))
	defer srv.Close()

	cfg := &traepkg.Config{
		Enabled:        true,
		ChatBase:       srv.URL,
		APITraeBase:    srv.URL,
		IDEVersion:     traepkg.DefaultIDEVersion,
		IDEVersionCode: traepkg.DefaultIDEVersionCode,
		AppID:          traepkg.DefaultAppID,
	}
	acc := &traepkg.Account{UID: "u1", AccessToken: "tok"}
	b := New(traepkg.NewClient(), func() *traepkg.Config { return cfg })

	c, err := b.Credits(context.Background(), Account{A: acc, realm: "cn"})
	if err != nil {
		t.Fatalf("Credits: %v", err)
	}
	// 通用剩 10，绝不能是 5010。
	if c.Available != 10 {
		t.Errorf("Available = %v, want 10（必须是 General，不能是含 Work 的 Total）", c.Available)
	}
	if c.Available > 1000 {
		t.Errorf("Available = %v 明显吃了 Work 积分", c.Available)
	}
}

// TestBackendAccountsRealm 账号装载后带正确的域。
func TestBackendAccountsRealm(t *testing.T) {
	cfg := &traepkg.Config{
		Enabled:     true,
		ChatBase:    "https://trae-api-cn.mchost.guru",
		APITraeBase: "https://api.trae.cn",
		Accounts: []traepkg.Account{
			{UID: "u1", Name: "甲", AccessToken: "t1"},
			{UID: "u2", Name: "乙", AccessToken: "t2", Disabled: true},
		},
	}
	b := New(traepkg.NewClient(), func() *traepkg.Config { return cfg })
	accs, err := b.Accounts()
	if err != nil {
		t.Fatal(err)
	}
	if len(accs) != 2 {
		t.Fatalf("账号数 = %d", len(accs))
	}
	for _, a := range accs {
		if a.Realm() != "cn" {
			t.Errorf("%s Realm = %q, want cn", a.UID(), a.Realm())
		}
		if a.Backend() != Name {
			t.Errorf("Backend = %q, want %q", a.Backend(), Name)
		}
	}
}

// TestBackendSupportsUsesModelPairs 内置映射里的模型应被支持。
func TestBackendSupportsUsesModelPairs(t *testing.T) {
	cfg := &traepkg.Config{Enabled: true, ChatBase: "http://127.0.0.1:1"}
	b := New(traepkg.NewClient(), func() *traepkg.Config { return cfg })
	acc := Account{A: &traepkg.Account{UID: "u1", AccessToken: "t"}, realm: "cn"}

	if !b.Supports(acc, "glm-5.2") {
		t.Error("glm-5.2 在内置映射里，应被支持")
	}
	// 未收录且目录拉不到 → 拒绝（Trae 会回 4001，提前排除更省额度）。
	if b.Supports(acc, "totally-unknown-model-xyz") {
		t.Error("未收录模型不应被支持")
	}
}

// TestWireModel 客户端名 → config_name 映射。
func TestWireModel(t *testing.T) {
	b := &Backend{}
	acc := Account{A: &traepkg.Account{UID: "u1"}, realm: "cn"}
	if got := b.WireModel(acc, "deepseek-v4-flash"); got != "DeepSeek-V4-Flash" {
		t.Errorf("WireModel = %q, want DeepSeek-V4-Flash", got)
	}
	// 无映射时原样返回。
	if got := b.WireModel(acc, "some-new-model"); got != "some-new-model" {
		t.Errorf("WireModel = %q", got)
	}
}

// 编译期：Account 满足 gateway.Account。
var _ gateway.Account = Account{}
