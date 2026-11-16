package auth

import (
	"encoding/base64"
	"testing"
)

// mustJWT 用给定 payload 拼一个最小 JWT（header.payload.signature）。
// TokenSource 不校验签名，只解 payload，故签名段可为任意占位。
func mustJWT(t *testing.T, payload string) string {
	t.Helper()
	return "hdr." + base64.RawURLEncoding.EncodeToString([]byte(payload)) + ".sig"
}

// TestTokenSource JWT token_source 声明的读取（仅用于出站头对齐，不参与门控）。
// 无该声明 / 非 JWT / 空 token 一律返回空串（老凭证零回归）。
func TestTokenSource(t *testing.T) {
	cases := []struct {
		name  string
		token string
		want  string
	}{
		{"enterprise_switch", mustJWT(t, `{"sub":"u1","token_source":"enterprise_switch"}`), "enterprise_switch"},
		{"无 token_source 声明", mustJWT(t, `{"sub":"u1","scope":"openid profile"}`), ""},
		{"其他 token_source", mustJWT(t, `{"sub":"u1","token_source":"password"}`), "password"},
		{"带填充 base64url", "hdr." + base64.URLEncoding.EncodeToString([]byte(`{"token_source":"enterprise_switch"}`)) + ".sig", "enterprise_switch"},
		{"payload 非 JSON", "hdr." + base64.RawURLEncoding.EncodeToString([]byte("not-json")) + ".sig", ""},
		{"非 JWT 形态", "opaque-token", ""},
		{"空 token", "", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			a := &Auth{UID: "u1", AccessToken: c.token}
			if got := a.TokenSource(); got != c.want {
				t.Errorf("TokenSource()=%q want %q", got, c.want)
			}
		})
	}
}

// TestSupportsPersonalGrowth 企业判定只认 enterpriseId。
//
// 回归用例（2026-11-13）：JWT 带 token_source=enterprise_switch 的**个人号**
// （enterpriseId 为空）必须仍被视为「支持个人成长体系」。
//
// 历史上这里（原 IsEnterpriseSession）把它判成企业上下文，导致活跃上报 /
// 连登管家 / 夜猫子 / 旅行四条链路对一个正常个人号全线跳过——表现为服务跑了
// 一段时间（token refresh 继承该章）后任务突然全停。
// 下游 fork 亦独立得出同一结论：「企业形态一律以 enterpriseId 为准」。
func TestSupportsPersonalGrowth(t *testing.T) {
	cases := []struct {
		name         string
		enterpriseID string
		token        string
		want         bool
	}{
		{"个人号", "", mustJWT(t, `{"sub":"u1"}`), true},
		{"个人号但带 enterprise_switch 章（回归）", "", mustJWT(t, `{"sub":"u1","token_source":"enterprise_switch"}`), true},
		{"企业版", "e1", mustJWT(t, `{"sub":"u1"}`), false},
		{"企业版且带 enterprise_switch 章", "e1", mustJWT(t, `{"token_source":"enterprise_switch"}`), false},
		{"enterpriseId 仅空白", "   ", mustJWT(t, `{"sub":"u1"}`), true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			a := &Auth{UID: "u1", EnterpriseID: c.enterpriseID, AccessToken: c.token}
			if got := a.SupportsPersonalGrowth(); got != c.want {
				t.Errorf("SupportsPersonalGrowth()=%v want %v", got, c.want)
			}
		})
	}

	t.Run("token_source 不影响 IsEnterprise（计费口径隔离）", func(t *testing.T) {
		a := &Auth{UID: "u1", AccessToken: mustJWT(t, `{"token_source":"enterprise_switch"}`)}
		if a.IsEnterprise() {
			t.Error("仅 token_source=enterprise_switch 不应翻转 IsEnterprise（额度查询仍走个人口径）")
		}
	})
}
