package trae

import "testing"

// TestRealmOfBase Trae 的域由 base 域名决定，不能退化成空串。
func TestRealmOfBase(t *testing.T) {
	cases := []struct {
		chat, api, want string
	}{
		{"https://trae-api-cn.mchost.guru", "https://api.trae.cn", "cn"},
		{"https://api.trae.com.cn", "", "cn"},
		{"", "https://api.trae.cn", "cn"},
		// 国际版
		{"https://trae-api.trae.ai", "", "global"},
		{"https://api.trae.com", "https://api.trae.com", "global"},
		// 无法判定 → 缺省 cn（与当前仓库唯一实现一致）
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
