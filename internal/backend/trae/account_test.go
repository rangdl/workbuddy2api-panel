package trae

import (
	"context"
	"testing"

	traepkg "github.com/linguo2625469/workbuddy2api-panel/internal/trae"
)

// wrapWith 构造配置并包装成池里的账号（测试用最小装配）。
func wrapWith(chatBase string, globalEnabled *bool, accs ...*traepkg.Account) []interface {
	UID() string
	Realm() string
} {
	list := make([]traepkg.Account, 0, len(accs))
	for _, a := range accs {
		list = append(list, *a)
	}
	cfg := &traepkg.Config{
		CN:       traepkg.TraeRealm{ChatBase: chatBase},
		Global:   traepkg.GlobalRealm{Enabled: globalEnabled},
		Accounts: list,
	}
	cfg.Normalize()
	out := Wrap(cfg, nil)
	res := make([]interface {
		UID() string
		Realm() string
	}, 0, len(out))
	for _, a := range out {
		res = append(res, a)
	}
	return res
}

func boolPtrTest(b bool) *bool { return &b }

// TestRealmOfBase realmOfBase 只是**老账号的兜底**：账号文件没有 host /
// user_region 字段时，域只能从配置 base 推断。真实判据见 traepkg.RegionOfCredential。
//
// 关键修正：改造前这张表把 `api.trae.com` 判成 global。国际版的真实域名是
// `.trae.ai`（coresg-normal.trae.ai / growsg-normal.trae.ai / api-sg-central.trae.ai），
// `trae.com` 不是 Trae 的服务域——按它判域会把一个不存在的域名当成国际版。
func TestRealmOfBase(t *testing.T) {
	cases := []struct {
		chat, api, want string
	}{
		{"https://trae-api-cn.mchost.guru", "https://api.trae.cn", "cn"},
		{"https://api.trae.com.cn", "", "cn"},
		{"", "https://api.trae.cn", "cn"},
		// 国际版（实测 host）
		{"https://coresg-normal.trae.ai", "", "global"},
		{"", "https://api-sg-central.trae.ai", "global"},
		{"", "https://growsg-normal.trae.ai", "global"},
		// 无法判定 → 缺省 cn（与升级前导入的账号一致）
		{"https://example.com", "", "cn"},
		{"", "", "cn"},
	}
	for _, c := range cases {
		if got := realmOfBase(c.chat, c.api); got != c.want {
			t.Errorf("realmOfBase(%q,%q) = %q, want %q", c.chat, c.api, got, c.want)
		}
	}
}

// TestRealmNeverEmpty 契约：Realm() 必须非空——空串会让 gateSpec 的域过滤失效。
func TestRealmNeverEmpty(t *testing.T) {
	for _, chat := range []string{"", "https://x", "https://trae-api-cn.mchost.guru"} {
		if got := realmOfBase(chat, ""); got == "" {
			t.Errorf("realmOfBase(%q) 返回空串，会静默关掉域隔离", chat)
		}
	}
}

// TestWrapUsesAccountRegion 域判定优先用账号自身的凭证，配置 base 只是兜底。
//
// 这是本次改造的核心：改造前 Wrap 用 realmOfBase(cfg.ChatBase, ...) 给**所有**
// 账号算同一个域，于是国际账号与国内账号混在一份 trae.json 里时无法区分。
func TestWrapUsesAccountRegion(t *testing.T) {
	accs := wrapWith("https://trae-api-cn.mchost.guru", nil,
		&traepkg.Account{UID: "cn1", AccessToken: "t", Host: "https://api.trae.cn", UserRegion: "CN"},
		&traepkg.Account{UID: "sg1", AccessToken: "t", Host: "https://growsg-normal.trae.ai", UserRegion: "SG"},
		&traepkg.Account{UID: "old", AccessToken: "t"}, // 老账号：无凭证判据
	)
	got := map[string]string{}
	for _, a := range accs {
		got[a.UID()] = a.Realm()
	}
	if got["cn1"] != "cn" {
		t.Errorf("cn1 realm = %q, want cn", got["cn1"])
	}
	if got["sg1"] != "global" {
		t.Errorf("sg1 realm = %q, want global（凭证声明 SG，配置 base 是 CN 也不能覆盖）", got["sg1"])
	}
	if got["old"] != "cn" {
		t.Errorf("old realm = %q, want cn（无判据时回落配置 base）", got["old"])
	}
}

// TestWrapExcludesUSRegion 美国区账号不得进池。
//
// US 是真实存在但未实现的区域：进池会被当成普通账号选中，然后 401，
// 再被记成一次上游故障污染熔断判据（参考项目 issue #66）。
func TestWrapExcludesUSRegion(t *testing.T) {
	accs := wrapWith("https://trae-api-cn.mchost.guru", nil,
		&traepkg.Account{UID: "us1", AccessToken: "t", Host: "https://grow-normal.traeapi.us", UserRegion: "US"},
		&traepkg.Account{UID: "cn1", AccessToken: "t", Host: "https://api.trae.cn", UserRegion: "CN"},
	)
	if len(accs) != 1 {
		t.Fatalf("池中账号数 = %d, want 1（US 账号应被排除）", len(accs))
	}
	if accs[0].UID() != "cn1" {
		t.Errorf("保留的账号是 %s, want cn1", accs[0].UID())
	}
}

// TestGlobalEscapeHatch global.enabled=false 时国际账号降级为 cn（纯 CN 锁定）。
func TestGlobalEscapeHatch(t *testing.T) {
	accs := wrapWith("https://trae-api-cn.mchost.guru", boolPtrTest(false),
		&traepkg.Account{UID: "sg1", AccessToken: "t", Host: "https://growsg-normal.trae.ai", UserRegion: "SG"},
	)
	if len(accs) != 1 {
		t.Fatalf("账号数 = %d, want 1", len(accs))
	}
	if got := accs[0].Realm(); got != "cn" {
		t.Errorf("逃生门关闭时 realm = %q, want cn", got)
	}
}

// TestCheckinSkippedForGlobalDomain 国际域签到返回 Skipped，且不打上游。
//
// 为什么必须是 Skipped 而不是 ErrUnsupported：
//   - Skipped = 「账号正常，只是这类账号不参与」→ 池正常记账、面板能读到
//     （panel 的 checkin_done 只看 Skipped），可显示「该区域无签到」；
//   - ErrUnsupported = 「上游没实现」→ 池静默跳过，用户什么也看不到。
//
// 与 codebuddy 企业版的处理一致。参考实现实测国际网关
// /trae/api/v2/ug/* 整族 404，所以这里根本不该发请求。
func TestCheckinSkippedForGlobalDomain(t *testing.T) {
	accs := wrapWith("https://trae-api-cn.mchost.guru", nil,
		&traepkg.Account{UID: "sg1", AccessToken: "t",
			Host: "https://growsg-normal.trae.ai", UserRegion: "SG"},
	)
	if len(accs) != 1 {
		t.Fatalf("账号数 = %d, want 1", len(accs))
	}
	// wrapWith 返回的是精简接口；这里需要能力方法，重新构一次。
	cfg := &traepkg.Config{
		CN:       traepkg.TraeRealm{ChatBase: "https://trae-api-cn.mchost.guru"},
		Accounts: []traepkg.Account{{UID: "sg1", AccessToken: "t", Host: "https://growsg-normal.trae.ai", UserRegion: "SG"}},
	}
	cfg.Normalize()
	wrapped := Wrap(cfg, nil)
	if len(wrapped) != 1 {
		t.Fatalf("wrapped 账号数 = %d", len(wrapped))
	}
	res, err := wrapped[0].(Account).Checkin(context.Background())
	if err != nil {
		t.Fatalf("国际域签到不应返回错误: %v", err)
	}
	if res == nil || !res.Skipped {
		t.Fatalf("国际域签到应返 Skipped，得到 %+v", res)
	}
	if res.Message == "" {
		t.Error("Skipped 必须带说明（面板要显示原因）")
	}
}
