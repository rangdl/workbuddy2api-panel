// trae_checkiner.go 把 internal/trae 的签到与余额能力适配成 scheduler.TraeCheckiner。
//
// 为什么需要这层：Trae 账号已经在同一个调度池里（选号、面板、冷却、暂停/禁用
// 全部共用），但 internal/trae 的 API 签名是 (cfg, *Account)，而调度器面对的是
// 池里的 uid。这里负责「uid → trae.Config 里的账号」这层查找，让签到排程不必
// 认识 Trae 的配置结构。
package main

import (
	"context"
	"errors"
	"sort"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/scheduler"
	"github.com/linguo2625469/workbuddy2api-panel/internal/trae"
)

// traeCheckinTimeout 单账号签到超时（含预检、续期与回查确认，三次上游往返）。
const traeCheckinTimeout = 60 * time.Second

// traeCreditsTimeout 单账号余额查询超时。
const traeCreditsTimeout = 30 * time.Second

// traeCheckiner 实现 scheduler.TraeCheckiner。
type traeCheckiner struct {
	// configFn 每次调用重新读盘：运行期可能登录新账号或改设备号，
	// 缓存会让签到用到旧凭证。
	configFn func() *trae.Config
	client   *trae.Client
}

// newTraeCheckiner 构造适配器。configFn 返回 nil 表示当前无可用配置。
func newTraeCheckiner(configFn func() *trae.Config, client *trae.Client) *traeCheckiner {
	return &traeCheckiner{configFn: configFn, client: client}
}

func (t *traeCheckiner) account(uid string) (*trae.Config, *trae.Account) {
	cfg := t.configFn()
	if cfg == nil {
		return nil, nil
	}
	return cfg, cfg.AccountByUID(uid)
}

// CheckinTrae 对单个 Trae 账号执行签到。
//
// 与面板单号签到走同一条链路（trae.Client.Checkin：预检 → claim → 回查确认），
// 区别只是错误以 error 返回而不是 HTTP 响应。
func (t *traeCheckiner) CheckinTrae(ctx context.Context, uid string) (*scheduler.TraeCheckinResult, error) {
	cfg, acc := t.account(uid)
	if cfg == nil {
		return nil, errors.New("trae 配置不可用")
	}
	if acc == nil {
		return nil, errors.New("账号不存在: " + uid)
	}
	ctx, cancel := context.WithTimeout(ctx, traeCheckinTimeout)
	defer cancel()
	if cfg.RefreshBeforeCheckinEnabled() {
		if _, err := t.client.EnsureTokenFresh(ctx, cfg, acc, 2*time.Hour); err != nil {
			return nil, errors.New("签到前续期失败: " + err.Error())
		}
	}
	res, err := t.client.Checkin(ctx, cfg, acc)
	if err != nil {
		// 9004/9095 不是失败：上游按「已签」语义拒绝，转成 Already 结果。
		if apiErr, ok := err.(*trae.APIError); ok {
			if cls := apiErr.Classification(); cls.Already {
				return &scheduler.TraeCheckinResult{
					Already:       true,
					DeviceAlready: cls.Code == "9095" || cls.Code == "9004",
					Message:       cls.Message,
				}, nil
			}
		}
		return nil, err
	}
	return &scheduler.TraeCheckinResult{
		Already:       res.Already,
		DeviceAlready: res.DeviceAlready,
		Unverified:    res.Unverified,
		Reward:        res.Reward,
		Message:       res.Message,
	}, nil
}

// TraeCredits 查询余额并换算成 pool 口径。
//
// Remain 取 General（product_id != 209）：对话只消耗 IDE 额度，用 Total 会让
// 快耗尽的号看起来健康（与 backend/trae.Credits 同一口径）。
func (t *traeCheckiner) TraeCredits(ctx context.Context, uid string) (pool.CreditsSnapshot, error) {
	cfg, acc := t.account(uid)
	if cfg == nil {
		return pool.CreditsSnapshot{}, errors.New("trae 配置不可用")
	}
	if acc == nil {
		return pool.CreditsSnapshot{}, errors.New("账号不存在: " + uid)
	}
	ctx, cancel := context.WithTimeout(ctx, traeCreditsTimeout)
	defer cancel()
	sum, err := t.client.QueryCredits(ctx, cfg, acc)
	if err != nil {
		return pool.CreditsSnapshot{}, err
	}
	return traeCreditsSnapshot(sum), nil
}

// traeCreditsSnapshot 把 CreditsSummary 换算成池的余额快照（含按日到期批次）。
//
// 口径对齐 upstream.UserResourceSnapshot：
//   - 只统计通用积分（Work 积分不参与对话，进批次会让最早到期路由把请求导向
//     实际不可用的额度）；
//   - 按自然日（UTC+8）聚合，同日多个包合计，ExpireAt 取当日最早；
//   - 已过期或零剩余的包不计入批次。
func traeCreditsSnapshot(sum *trae.CreditsSummary) pool.CreditsSnapshot {
	if sum == nil {
		return pool.CreditsSnapshot{}
	}
	snap := pool.CreditsSnapshot{
		Remain: int64(sum.General + 0.5),
		Total:  int64(sum.Limit + 0.5),
	}
	now := time.Now()
	byDate := make(map[string]*pool.ExpiryBucket, len(sum.Packs))
	for _, p := range sum.Packs {
		if p.IsWork() {
			continue
		}
		remain := p.Remaining()
		if remain <= 0 {
			continue
		}
		end, ok := trae.ParsePackTime(p.EndTime)
		if !ok || !end.After(now) {
			continue
		}
		// 按**本地时区**切自然日：trae.ParsePackTime 对无时区串就是用
		// time.Local 解释的（见其注释），再换算到别的时区会把同一天拆成两天。
		day := end.In(time.Local).Format("2006-01-02")
		b, seen := byDate[day]
		if !seen {
			b = &pool.ExpiryBucket{Date: day, ExpireAt: end}
			byDate[day] = b
		} else if end.Before(b.ExpireAt) {
			b.ExpireAt = end
		}
		b.Remain += int64(remain + 0.5)
	}
	if len(byDate) == 0 {
		return snap
	}
	dates := make([]string, 0, len(byDate))
	for d := range byDate {
		dates = append(dates, d)
	}
	sort.Strings(dates)
	snap.ExpiryBuckets = make([]pool.ExpiryBucket, 0, len(dates))
	for _, d := range dates {
		snap.ExpiryBuckets = append(snap.ExpiryBuckets, *byDate[d])
	}
	snap.EarliestAt = snap.ExpiryBuckets[0].ExpireAt
	snap.EarliestRemaining = snap.ExpiryBuckets[0].Remain
	return snap
}
