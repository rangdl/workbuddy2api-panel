package trae

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"time"
)

// jwt.go Trae access token 的 JWT 解析。
//
// 只做一件必要的事：取 exp（过期时间）——网关据此决定何时刷新。
// 不做签名校验（网关不是签发方，也不该持有校验密钥）。

// ExpiryFromJWT 从 JWT 的 payload 段提取 exp（Unix 秒）。
// token 可能带 "Cloud-IDE-JWT " 前缀；解析失败返回 0（调用方视为"未知过期时间"）。
func ExpiryFromJWT(token string) int64 {
	token = strings.TrimSpace(token)
	if i := strings.LastIndex(token, " "); i >= 0 {
		// "Cloud-IDE-JWT eyJ..." → 取最后一段
		token = token[i+1:]
	}
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return 0
	}
	payload, err := base64.RawURLEncoding.DecodeString(strings.TrimSpace(parts[1]))
	if err != nil {
		// 兼容带填充的标准 base64。
		payload, err = base64.StdEncoding.DecodeString(parts[1])
		if err != nil {
			return 0
		}
	}
	var claims struct {
		Exp int64 `json:"exp"`
		// 部分签发形态把过期时间放在自定义字段。
		ExpiresAt   int64 `json:"expires_at"`
		ExpiresAtMs int64 `json:"expires_at_ms"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return 0
	}
	switch {
	case claims.Exp > 0:
		return claims.Exp
	case claims.ExpiresAtMs > 0:
		return claims.ExpiresAtMs / 1000
	case claims.ExpiresAt > 0:
		if claims.ExpiresAt > 1e12 {
			return claims.ExpiresAt / 1000
		}
		return claims.ExpiresAt
	default:
		return 0
	}
}

// JWTUserID 从 JWT payload 提取 user_id（登录后落账号 uid 用）。
// 依次尝试常见键名；缺失返回空串。
func JWTUserID(token string) string {
	claims := jwtClaims(token)
	for _, k := range []string{"user_id", "userId", "uid", "sub", "id"} {
		if v, ok := claims[k].(string); ok && strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	// 部分形态嵌在 data 对象里。
	if data, ok := claims["data"].(map[string]any); ok {
		for _, k := range []string{"id", "user_id", "userId"} {
			if v, ok := data[k].(string); ok && strings.TrimSpace(v) != "" {
				return strings.TrimSpace(v)
			}
		}
	}
	return ""
}

// jwtClaims 解析 JWT payload 为 map（失败返回空 map）。
func jwtClaims(token string) map[string]any {
	token = strings.TrimSpace(token)
	if i := strings.LastIndex(token, " "); i >= 0 {
		token = token[i+1:]
	}
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return map[string]any{}
	}
	payload, err := base64.RawURLEncoding.DecodeString(strings.TrimSpace(parts[1]))
	if err != nil {
		if payload, err = base64.StdEncoding.DecodeString(parts[1]); err != nil {
			return map[string]any{}
		}
	}
	var m map[string]any
	if json.Unmarshal(payload, &m) != nil {
		return map[string]any{}
	}
	return m
}

// NeedsRefresh 判断 token 是否需要在 within 窗口内刷新。
// ExpiresAt 未知（0）时返回 true——宁可多刷一次，也不要拿过期 token 打上游。
func (a *Account) NeedsRefresh(within time.Duration) bool {
	if strings.TrimSpace(a.AccessToken) == "" {
		return true
	}
	if a.ExpiresAt == 0 {
		return true
	}
	return time.Now().Add(within).Unix() >= a.ExpiresAt
}
