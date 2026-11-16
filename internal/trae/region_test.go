package trae

import "testing"

// region_test.go 守 Trae 国际版（global 域）的域判定与 US 区防线。
//
// 历史缺陷（本次修复）：域由 realmOfBase(cfg.ChatBase, ...) 从**配置**推断，
// 于是整份 trae.json 只能是单域——国际账号与国内账号混在一份配置里时无法
// 区分。同时 US 区既不被 userRegion 也不被 host 认出，会静默回落到 global。

// TestRegionOfUserRegion userRegion 两种形态 + 大小写不敏感。
func TestRegionOfUserRegion(t *testing.T) {
	cases := []struct {
		name string
		in   any
		want ServiceRegion
		ok   bool
	}{
		// 桌面版 storage.json：对象形态
		{"object CN", map[string]any{"region": "CN", "_aiRegion": "CN"}, RegionCN, true},
		{"object SG", map[string]any{"region": "SG", "_aiRegion": "SG"}, RegionGlobal, true},
		// App 日志里的裸值，且是小写（必须大小写不敏感）
		{"bare sg lower", "sg", RegionGlobal, true},
		{"bare SG upper", "SG", RegionGlobal, true},
		{"bare ai", "ai", RegionGlobal, true},
		{"bare cn lower", "cn", RegionCN, true},
		// US 是独立区域，不能落到 global
		{"bare US", "US", RegionUS, true},
		// 认不出来
		{"empty", "", "", false},
		{"unknown", "eu", "", false},
		{"nil", nil, "", false},
	}
	for _, c := range cases {
		got, ok := RegionOfUserRegion(c.in)
		if ok != c.ok || got != c.want {
			t.Errorf("%s: RegionOfUserRegion(%v) = (%q,%v), want (%q,%v)",
				c.name, c.in, got, ok, c.want, c.ok)
		}
	}
}

// TestRegionOfHost host 后缀判定，含判定顺序陷阱。
func TestRegionOfHost(t *testing.T) {
	cases := []struct {
		host string
		want ServiceRegion
		ok   bool
	}{
		{"https://api.trae.cn", RegionCN, true},
		{"https://api.trae.com.cn", RegionCN, true}, // 必须先于 trae.com 判定
		{"https://coresg-normal.trae.ai", RegionGlobal, true},
		{"https://growsg-normal.trae.ai", RegionGlobal, true},
		{"https://api-sg-central.trae.ai", RegionGlobal, true},
		// US 是另一个集群，不是 .trae.ai 的特例
		{"https://grow-normal.traeapi.us", RegionUS, true},
		// 带路径也要能判
		{"https://coresg-normal.trae.ai/api/remote/v1", RegionGlobal, true},
		{"", "", false},
		{"https://example.com", "", false},
	}
	for _, c := range cases {
		got, ok := RegionOfHost(c.host)
		if ok != c.ok || got != c.want {
			t.Errorf("RegionOfHost(%q) = (%q,%v), want (%q,%v)", c.host, got, ok, c.want, c.ok)
		}
	}
}

// TestRegionOfCredentialPrecedence userRegion 优先于 host。
func TestRegionOfCredentialPrecedence(t *testing.T) {
	// 声明与 host 冲突时以声明为准。
	if got, _ := RegionOfCredential("https://api.trae.cn", "SG"); got != RegionGlobal {
		t.Errorf("userRegion 应优先，得到 %q", got)
	}
	// 无声明时用 host。
	if got, _ := RegionOfCredential("https://growsg-normal.trae.ai", ""); got != RegionGlobal {
		t.Errorf("无声明时应按 host 判，得到 %q", got)
	}
	// 两者都没有 → 无判据（调用方回落配置 base，不能默认成 global）。
	if _, ok := RegionOfCredential("", ""); ok {
		t.Error("无判据时应返回 ok=false")
	}
}

// TestUSIsNotServable US 不可服务，且**不能**映射成 global。
func TestUSIsNotServable(t *testing.T) {
	if RegionUS.IsServable() {
		t.Error("US 不应被标记为可服务")
	}
	if got := RegionUS.Realm(); got != "" {
		t.Errorf("US.Realm() = %q, want 空串（不能回落成 global）", got)
	}
	// 可服务的两个域必须给出非空 realm。
	for _, r := range []ServiceRegion{RegionCN, RegionGlobal} {
		if !r.IsServable() || r.Realm() == "" {
			t.Errorf("%q 应可服务且 Realm 非空", r)
		}
	}
}

// TestAccountRegionFallsBackToCN 老账号（无 host / userRegion）→ cn。
func TestAccountRegionFallsBackToCN(t *testing.T) {
	a := &Account{UID: "old"}
	if got := a.Region(nil); got != RegionCN {
		t.Errorf("无判据的账号 Region = %q, want cn", got)
	}
}

// TestGlobalEscapeHatch RegionOf cfg.Global.Enabled=false 时降级为 cn。
func TestGlobalEscapeHatch(t *testing.T) {
	sg := &Account{UID: "sg", Host: "https://growsg-normal.trae.ai", UserRegion: "SG"}
	on := true
	cfgOn := &Config{}
	cfgOn.Global.Enabled = &on
	if got := sg.RegionOf(cfgOn); got != RegionGlobal {
		t.Errorf("开关开时 = %q, want global", got)
	}
	off := false
	cfgOff := &Config{}
	cfgOff.Global.Enabled = &off
	if got := sg.RegionOf(cfgOff); got != RegionCN {
		t.Errorf("逃生门关闭时 = %q, want cn", got)
	}
	// nil 配置也视为启用（缺省 true）。
	if got := sg.RegionOf(nil); got != RegionGlobal {
		t.Errorf("nil 配置时 = %q, want global（缺省启用）", got)
	}
}

// TestChatBaseForReallyDispatches base 分派必须真正按域走。
//
// 改造前这个函数忽略 realm 参数直接返 CN 的 base——国际账号会被打到国内网关。
func TestChatBaseForReallyDispatches(t *testing.T) {
	c := &Config{}
	c.Normalize()
	if got := c.ChatBaseFor("cn"); got != DefaultChatBase {
		t.Errorf("cn chat base = %q, want %q", got, DefaultChatBase)
	}
	if got := c.ChatBaseFor("global"); got != DefaultChatBaseGlobal {
		t.Errorf("global chat base = %q, want %q", got, DefaultChatBaseGlobal)
	}
	// 未知域回落 cn。
	if got := c.ChatBaseFor("mars"); got != DefaultChatBase {
		t.Errorf("未知域 = %q, want cn base", got)
	}
}

// TestAPIBasePrefersCredentialHost 国际域的用量 base 优先用凭证自带 host。
//
// SG 侧 host 不统一（api-sg-central / growsg-normal），硬编码单一 host 会
// 让另一批账号打错网关。
func TestAPIBasePrefersCredentialHost(t *testing.T) {
	c := &Config{}
	c.Normalize()
	a := &Account{UID: "sg", Host: "https://api-sg-central.trae.ai", UserRegion: "SG"}
	if got := c.apiBaseForAccount(a); got != "https://api-sg-central.trae.ai" {
		t.Errorf("国际账号 api base = %q, want 凭证 host", got)
	}
	cn := &Account{UID: "cn", Host: "https://api.trae.cn", UserRegion: "CN"}
	if got := c.apiBaseForAccount(cn); got != DefaultAPITraeBase {
		t.Errorf("国内账号 api base = %q, want %q", got, DefaultAPITraeBase)
	}
}

// TestClientIDByPackageType ClientID 按「是否 SOLO 包」分叉，不按域。
func TestClientIDByPackageType(t *testing.T) {
	c := &Config{}
	if got := c.oauthClientIDFor(&Account{UID: "desktop"}); got != DefaultClientID {
		t.Errorf("桌面号 ClientID = %q, want %q", got, DefaultClientID)
	}
	if got := c.oauthClientIDFor(&Account{UID: "solo", PackageType: "solo"}); got != DefaultClientIDSolo {
		t.Errorf("SOLO 号 ClientID = %q, want %q", got, DefaultClientIDSolo)
	}
	// 大小写不敏感（上游/用户可能写 SOLO）。
	if got := c.oauthClientIDFor(&Account{UID: "solo2", PackageType: "SOLO"}); got != DefaultClientIDSolo {
		t.Errorf("PackageType=SOLO 应识别，得到 %q", got)
	}
	// 显式配置覆盖优先。
	c.OAuthClientID = "custom-id"
	if got := c.oauthClientIDFor(&Account{UID: "x", PackageType: "solo"}); got != "custom-id" {
		t.Errorf("配置覆盖应优先，得到 %q", got)
	}
}
