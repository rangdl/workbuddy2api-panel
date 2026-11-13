// blindbox.go growth 域「Buddy 能量盲盒」接口：配额查询 / 开盒。
//
// 背景：项目里 energy 长期只进不出（连登兑换发 energy，但没有任何消耗出口），
// 实测账号 125 能量闲置。盲盒是唯一的能量消耗口：每次开盒消耗 cost_per_open 能量，
// 产出猫（buddy）或积分。开盒要求猫处于 idle（旅行中/到站未领时不能开）。
package upstream

import (
	"encoding/json"
	"net/http"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// buddy 盲盒路径（growth 域，growthJSON 走 chatBase，无 /v2 前缀）。
const (
	buddyQuotaPath = "/activity/growth/buddy/quota"
	buddyOpenPath  = "/activity/growth/buddy/open"
)

// BuddyQuota 盲盒开盒配额（实测 GET /activity/growth/buddy/quota）：
//
//	{"affordable":5,"balance":125,"cost_per_open":10,"max_open_count":5}
//
// Affordable = 当前能量可开次数（balance/cost_per_open，服务端已按 max_open_count 钳过）；
// Balance = 能量余额；CostPerOpen = 单次消耗；MaxOpenCount = 单次调用允许的最大次数。
type BuddyQuota struct {
	Affordable   int   `json:"affordable"`
	Balance      int64 `json:"balance"`
	CostPerOpen  int64 `json:"cost_per_open"`
	MaxOpenCount int   `json:"max_open_count"`
}

// BuddyQuota 查询盲盒配额。
func (c *Client) BuddyQuota(a *auth.Auth) (*BuddyQuota, error) {
	data, err := c.growthJSON(a, http.MethodGet, buddyQuotaPath, nil)
	if err != nil {
		return nil, err
	}
	// balance 与 energy 两个名字都试：实测返回 balance，参考实现读 energy，
	// 活动期换字段名时另一侧仍能取到值（缺字段为 0，不影响 affordable 判据）。
	var raw struct {
		Affordable   int   `json:"affordable"`
		Balance      int64 `json:"balance"`
		Energy       int64 `json:"energy"`
		CostPerOpen  int64 `json:"cost_per_open"`
		MaxOpenCount int   `json:"max_open_count"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, err
	}
	q := &BuddyQuota{
		Affordable:   raw.Affordable,
		Balance:      raw.Balance,
		CostPerOpen:  raw.CostPerOpen,
		MaxOpenCount: raw.MaxOpenCount,
	}
	if q.Balance == 0 {
		q.Balance = raw.Energy
	}
	return q, nil
}

// BuddyOpen 开 count 次盲盒，返回原始载荷（buddy 名称/奖励形状随活动期变化，
// 透传给调用方解析，避免强类型随活动改版失效）。
//
// client_token 为前端生成的幂等令牌（服务端只做去重），实测形态 "u-<uuid4>"。
func (c *Client) BuddyOpen(a *auth.Auth, count int) (json.RawMessage, error) {
	return c.growthJSON(a, http.MethodPost, buddyOpenPath, map[string]any{
		"count":        count,
		"client_token": "u-" + clientToken(),
	})
}
