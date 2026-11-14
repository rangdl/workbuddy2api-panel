// Package trae 把 internal/trae（Trae 上游）适配成 gateway.Backend。
package trae

import (
	"context"
	"errors"
	"sort"
	"strings"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/gateway"
	traepkg "github.com/linguo2625469/workbuddy2api-panel/internal/trae"
)

// Name 上游标识。
const Name = "trae"

// Account 把 *trae.Account 适配成 gateway.Account。
type Account struct {
	A *traepkg.Account
	// realm 由账号所属的 base 域名决定（见 realmOfBase）。
	// 未导出：Realm() 方法同名会冲突（Go 不允许字段与方法同名）。
	realm string
	// dir 账号凭证目录（"auths"），SetNickname 落盘用。空 = 只改内存。
	dir string
	// client / configFn 是账号**自己的能力依赖**：Credits/Checkin/Keepalive
	// 由账号对象自己实现，而不是由外部的 backend 代劳。这样池遍历账号调
	// 统一方法即可，不需要认识 Trae 协议。
	client   *traepkg.Client
	ConfigFn func() *traepkg.Config
}

func (x Account) UID() string      { return x.A.UID }
func (x Account) Nickname() string { return x.A.Name }
func (x Account) Realm() string    { return x.realm }
func (x Account) Backend() string  { return Name }

// Enterprise Trae 没有企业版概念。
func (x Account) Enterprise() bool { return false }

// Credential 实现 gateway.CredentialUnwrapper。
func (x Account) Credential() any {
	if x.A == nil {
		return nil
	}
	return x.A
}

// Unwrap 取出原始账号。
//
// 与 codebuddy.Unwrap 同口径：除本包 Account 外，也认 gateway.CredentialUnwrapper
// （占位/测试路径上的账号走这条），避免「只认具体类型」导致的静默 nil。
func Unwrap(acc gateway.Account) *traepkg.Account {
	switch a := acc.(type) {
	case Account:
		return a.A
	case *Account:
		return a.A
	}
	if u, ok := acc.(gateway.CredentialUnwrapper); ok {
		if a, ok := u.Credential().(*traepkg.Account); ok {
			return a
		}
	}
	return nil
}

// realmOfBase 按上游域名判定账号域。
//
// Trae 与 CodeBuddy 一样有 cn / global 两个域，只是判定依据不同：
//   - CodeBuddy 看 auth 文件里的 realm 字段或 domain 后缀；
//   - Trae 看对话/接口 base 的域名——本仓库目前只实现了 CN（mchost.guru /
//     trae.cn / trae.com.cn），其余域名一律视为 global。
//
// 为什么必须返回非空：空串在 gateSpec 里表示「不过滤域」，静默返回空会让
// 域隔离失效，cn 请求可能落到 global 账号上。
func realmOfBase(chatBase, apiBase string) string {
	for _, u := range []string{chatBase, apiBase} {
		u = strings.ToLower(strings.TrimSpace(u))
		if u == "" {
			continue
		}
		// 判定顺序敏感：trae.com.cn 是 CN 域，但它是 trae.com 的超串，
		// 所以必须先判 .com.cn 再判国际版后缀，否则 CN 会被误判成 global。
		switch {
		case strings.Contains(u, "trae.com.cn"),
			strings.Contains(u, "trae.cn"),
			strings.Contains(u, "trae-api-cn"),
			strings.Contains(u, "mchost.guru"):
			return "cn"
		case strings.Contains(u, "trae.ai"), strings.Contains(u, "trae.com"):
			return "global"
		}
	}
	// 无法判定时按 CN 处理（与仓库当前的唯一实现一致）。
	return gateway.DefaultRealm
}

// Wrap 把配置里的账号包装成 gateway.Account。
//
// client/configFn 一并注入账号对象：Credits/Checkin/Keepalive 是账号自己的
// 能力，不该由调用方（池/调度器）代持上游依赖。
func Wrap(cfg *traepkg.Config, client *traepkg.Client) []gateway.Account {
	if cfg == nil {
		return nil
	}
	realm := realmOfBase(cfg.ChatBase, cfg.APITraeBase)
	out := make([]gateway.Account, 0, len(cfg.Accounts))
	for i := range cfg.Accounts {
		out = append(out, Account{
			A: &cfg.Accounts[i], realm: realm, dir: cfg.AuthDir,
			client: client,
		})
	}
	return out
}

// NicknameValue 实现 pool.Nicknamer（可选能力）。
func (x Account) NicknameValue() string { return x.A.Name }

// cfg 当前生效配置（热重载后即时生效）。
func (x Account) cfg() *traepkg.Config {
	if x.ConfigFn == nil {
		return nil
	}
	return x.ConfigFn()
}

// Credits 查询该账号余额（含按日到期批次）。
//
// 口径：Remain 取 General（product_id != 209）。对话只消耗 IDE 额度，
// 用 Total 会让快耗尽的号看起来健康。
func (x Account) Credits(ctx context.Context) (*gateway.Credits, error) {
	if x.client == nil || x.A == nil {
		return nil, gateway.ErrUnsupported
	}
	cfg := x.cfg()
	if cfg == nil {
		return nil, errors.New("trae: config not loaded")
	}
	sum, err := x.client.QueryCredits(ctx, cfg, x.A)
	if err != nil {
		return nil, err
	}
	out := &gateway.Credits{
		Available: sum.General,
		Limit:     sum.Limit,
	}
	now := time.Now()
	type dayAgg struct {
		at     time.Time
		remain int64
	}
	byDay := make(map[string]*dayAgg, len(sum.Packs))
	for _, pk := range sum.Packs {
		if pk.IsWork() {
			continue // Work 积分不参与对话抵扣
		}
		remain := int64(pk.Remaining())
		if remain <= 0 {
			continue
		}
		at, ok := traepkg.ParsePackTime(pk.EndTime)
		if !ok || !at.After(now) {
			continue
		}
		day := at.In(time.Local).Format("2006-01-02")
		agg, seen := byDay[day]
		if !seen {
			agg = &dayAgg{at: at}
			byDay[day] = agg
		} else if at.Before(agg.at) {
			agg.at = at
		}
		agg.remain += remain
	}
	if len(byDay) > 0 {
		dates := make([]string, 0, len(byDay))
		for d := range byDay {
			dates = append(dates, d)
		}
		sort.Strings(dates)
		out.Buckets = make([]gateway.ExpiryBucket, 0, len(dates))
		for _, d := range dates {
			out.Buckets = append(out.Buckets, gateway.ExpiryBucket{
				Date: d, ExpireAt: byDay[d].at, Remain: byDay[d].remain,
			})
		}
		out.Earliest = out.Buckets[0].ExpireAt
	}
	return out, nil
}

// Checkin 对该账号执行一次签到。
func (x Account) Checkin(ctx context.Context) (*gateway.CheckinResult, error) {
	if x.client == nil || x.A == nil {
		return nil, gateway.ErrUnsupported
	}
	cfg := x.cfg()
	if cfg == nil {
		return nil, errors.New("trae: config not loaded")
	}
	// 签到前按需续期：过期 token 会被上游分类成 session_dead 直接摘号，
	// 而实际上刷新一下就能签。
	if cfg.RefreshBeforeCheckinEnabled() {
		if _, err := x.client.EnsureTokenFresh(ctx, cfg, x.A, 2*time.Hour); err != nil {
			return nil, errors.New("签到前续期失败: " + err.Error())
		}
	}
	res, err := x.client.Checkin(ctx, cfg, x.A)
	if err != nil {
		// 9004/9095 是「设备/账号今日已签」的幂等拒绝，按成功处理。
		if apiErr, ok := err.(*traepkg.APIError); ok {
			if cls := apiErr.Classification(); cls.Already {
				// 9004 / 9095 都是**设备维度**的拒绝：本账号未必签过。
				// 池据此不标记「今日已签」（否则按钮谎报、次日不重试）。
				deviceAlready := cls.Code == "9095" || cls.Code == "9004"
				return &gateway.CheckinResult{
					Already:       true,
					DeviceAlready: deviceAlready,
					Message:       cls.Message,
				}, nil
			}
		}
		return nil, err
	}
	return &gateway.CheckinResult{
		Already:       res.Already,
		Reward:        res.Reward,
		Message:       res.Message,
		DeviceAlready: res.DeviceAlready,
		Unverified:    res.Unverified,
	}, nil
}

// Keepalive 刷新 access_token（Trae 的 refreshToken 会轮换，必须落盘）。
func (x Account) Keepalive(ctx context.Context) error {
	if x.client == nil || x.A == nil {
		return gateway.ErrUnsupported
	}
	cfg := x.cfg()
	if cfg == nil {
		return errors.New("trae: config not loaded")
	}
	_, err := x.client.EnsureTokenFresh(ctx, cfg, x.A, 0)
	return err
}

// SetNickname 更新昵称并落盘到 trae-<uid>.json。
//
// 为什么需要这条路径：Trae 的 JWT 里没有用户名字段（只有 data.id/source/
// tenant_id/type），登录时取不到昵称。让用户手工设置是唯一现实的来源，
// 而面板/日志都用昵称做展示（logfmt.Label 拼成 "昵称(uid8)"），
// 没有它就只能看到一串数字 uid。
func (x Account) SetNickname(name string) error {
	if x.A == nil {
		return errors.New("trae: nil account")
	}
	x.A.Name = name
	// SaveAccount 的目录由调用方（面板）提供，这里只改内存 + 写回单文件。
	if x.dir == "" {
		return nil // 无目录信息：仅内存生效（下次 Sync 会从文件重读覆盖）
	}
	return traepkg.SaveAccount(x.dir, x.A)
}
