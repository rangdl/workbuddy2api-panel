package trae

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// profile.go 拉取 Trae 账号的用户信息（昵称）。
//
// 为什么需要独立接口：Trae 的 access_token 是 JWT，但 payload 里只有
// data.{id,source,source_id,tenant_id,type}——**没有用户名字段**。而面板与
// 日志都用昵称做展示（logfmt.Label 拼成 "昵称(uid8)"），没有它只能看到一串数字。
//
// 端点规格来自 bluechonk 的实测（docs/FLOW.md §9.2 / traeUpstream.ts）：
//
//	POST {oauthHost}/cloudide/api/v3/trae/GetUserInfo
//	头：x-cloudide-token（裸 token，不带 "Cloud-IDE-JWT " 前缀）+ UA
//	体：{"ReqSource":"IDE","IDEVersion":"<版本>"}
//	回：Result.{UserID, ScreenName}（也可能在 result 小写键下）
//
// 注意它走 **OAuth 域**（api.trae.com.cn），不是签到域（api.trae.cn），
// 也不是对话域（mchost.guru）——三者互不相通。

// EndpointGetUserInfo 用户信息端点。
const EndpointGetUserInfo = "/cloudide/api/v3/trae/GetUserInfo"

// profileTimeout 单次查询超时（登录流程里同步调用，不宜过长）。
const profileTimeout = 15 * time.Second

// UserInfo 用户信息（只保留展示需要的两个字段）。
type UserInfo struct {
	UID      string `json:"uid"`
	Nickname string `json:"nickname"`
}

// GetUserInfo 查询账号的用户信息。
//
// 失败不算致命：uid 还能从 JWT 取，昵称缺失只是展示上退化成 uid 短号。
// 调用方应当把错误当成"取不到昵称"而不是"登录失败"。
func (c *Client) GetUserInfo(ctx context.Context, cfg *Config, acc *Account) (*UserInfo, error) {
	if cfg == nil || acc == nil {
		return nil, fmt.Errorf("trae: nil config or account")
	}
	token := acc.AccessTokenHeader()
	if token == "" {
		return nil, fmt.Errorf("trae: 账号 %s 无 access_token", acc.UID)
	}
	// 端点要裸 token（参考实现直接传 access_token，不带前缀）。
	token = strings.TrimPrefix(token, "Cloud-IDE-JWT ")

	body, err := json.Marshal(map[string]any{
		"ReqSource":  "IDE",
		"IDEVersion": ideVersionOrDefault(cfg),
	})
	if err != nil {
		return nil, err
	}

	ctx, cancel := context.WithTimeout(ctx, profileTimeout)
	defer cancel()

	url := cfg.oauthHost() + EndpointGetUserInfo
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-cloudide-token", token)
	req.Header.Set("User-Agent", "TraeClient/TTNet")
	req.Header.Set("Accept", "application/json")

	resp, err := c.jsonHTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBodyBytes))
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("GetUserInfo HTTP %d", resp.StatusCode)
	}

	var parsed map[string]any
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, fmt.Errorf("GetUserInfo 响应非 JSON")
	}
	// 业务错误信封（ResponseMetadata.Error）优先。
	if meta, ok := parsed["ResponseMetadata"].(map[string]any); ok {
		if e, ok := meta["Error"].(map[string]any); ok {
			if msg := str(e["Message"]); msg != "" {
				return nil, fmt.Errorf("GetUserInfo: %s", msg)
			}
		}
	}
	// Result 大写优先，兼容小写 result。
	result, _ := parsed["Result"].(map[string]any)
	if result == nil {
		result, _ = parsed["result"].(map[string]any)
	}
	if result == nil {
		return nil, fmt.Errorf("GetUserInfo: 响应缺少 Result")
	}
	return &UserInfo{
		UID:      firstNonEmpty(str(result["UserID"]), str(result["userId"]), str(result["user_id"])),
		Nickname: firstNonEmpty(str(result["ScreenName"]), str(result["screenName"]), str(result["nickname"])),
	}, nil
}

// ideVersionOrDefault 取生效的 IDE 版本（缺省用内置值）。
func ideVersionOrDefault(cfg *Config) string {
	if cfg != nil && strings.TrimSpace(cfg.IDEVersion) != "" {
		return cfg.IDEVersion
	}
	return DefaultIDEVersion
}
