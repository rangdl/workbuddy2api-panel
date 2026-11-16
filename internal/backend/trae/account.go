// Package trae 把 internal/trae（Trae 上游）适配成 gateway.Backend。
package trae

import (
	"context"
	"errors"
	"log"
	"sort"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/gateway"
	traepkg "github.com/linguo2625469/workbuddy2api-panel/internal/trae"
)

// Name 上游标识。
const Name = "trae"

// Account 把 *trae.Account 适配成 gateway.Account。
type Account struct {
	A *traepkg.Account
	// realm 该账号的网关域值（cn / global）。由凭证自身判定，见 Wrap。
	// 未导出：Realm() 方法同名会冲突（Go 不允许字段与方法同名）。
	realm string
	// region 该账号的服务区域（比 realm 宽，含 us）。
	region traepkg.ServiceRegion
	// dir 账号凭证目录（"auths"），SetNickname 落盘用。空 = 只改内存。
	dir string
	// client / configFn 是账号**自己的能力依赖**：Credits/Checkin/Keepalive
	// 由账号对象自己实现，而不是由外部的 backend 代劳。这样池遍历账号调
	// 统一方法即可，不需要认识 Trae 协议。
	client   *traepkg.Client
	ConfigFn func() *traepkg.Config
	// stats 运行期计数（面板的「成功/失败/在途」与积分展示用）。
	//
	// 由 cmd/server 在入池前注入（与 ConfigFn 同一手法）：面板与 handler 必须
	// 共用同一实例，而那个实例的构造属于装配层。
	stats *traepkg.Stats
}

// SetStats 注入运行期计数实例（装配期调用；nil = 不统计）。
func (x Account) SetStats(s *traepkg.Stats) Account {
	x.stats = s
	return x
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

// 【已移除】UsageLabel：它曾把 "trae" 当作用量账本的「域」，导致面板
// 「按域」出现 cn / global / trae 三行。上游身份由 Backend() 回答，
// 用量账本已按 (backend, realm) 两个维度分别记账，不再需要自报标签。

// ObserveRemain 实现 gateway.RemainObserver：接收 notify_usage 上报的两份积分。
//
// Trae 面板的签到区块要分别展示 IDE 与 Work 积分，所以这里必须把两份都存下来
// （池的 credits 只用 Available，那是对外选号口径）。
func (x Account) ObserveRemain(r *gateway.Remain) {
	if x.stats == nil || x.A == nil || r == nil {
		return
	}
	x.stats.NoteRemain(x.A.UID, r.Available, r.Work)
}

// BeginStream 实现 gateway.StreamLifecycle：面板的「成功/失败/在途」三列。
//
// 返回的 finish 必须被调用（成功与否都调），否则在途计数永不归零。
func (x Account) BeginStream() func(ok bool) {
	if x.stats == nil || x.A == nil {
		return func(bool) {}
	}
	finish := x.stats.Begin(x.A.UID)
	return func(ok bool) {
		// token/延迟字段由 server 的 recordBackendUsage 另行落池，
		// 这里只结算在途与成功/失败。
		finish(ok, traepkg.UsageDelta{})
	}
}

// 编译期断言。
//
// 【已移除】CreditEstimator：按「token × 倍率」估算 Trae 扣费曾是这里的一部分，
// 但 consumption_rate 是相对倍率（x0.06 那种），不是每 token 积分数，估出来
// 虚高约 1500 倍。真实消耗只认上游余额差（credithist 账本），不再估算。
var (
	_ gateway.RemainObserver  = Account{}
	_ gateway.StreamLifecycle = Account{}
)

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

// realmOfBase 按 base 域名判定域——**仅作老账号的兜底**。
//
// 正常情况下域由账号凭证自身的 userRegion / host 决定（见 traepkg.Account.Region）。
// 这条路径只服务升级前导入的账号文件（没有 host 与 user_region 字段），
// 那时域只能从配置 base 推断——这正是改造前的行为。
//
// 为什么不能只保留这一条：它让整份 trae.json 只能是单域。国际账号与国内账号
// 混在同一份配置里时，所有账号会拿到同一个域。
func realmOfBase(chatBase, apiBase string) string {
	for _, u := range []string{chatBase, apiBase} {
		if r, ok := traepkg.RegionOfHost(u); ok && r.IsServable() {
			return string(r)
		}
	}
	// 无法判定时按 CN 处理（与升级前导入的账号一致）。
	return gateway.DefaultRealm
}

// Wrap 把配置里的账号包装成 gateway.Account。
//
// client/configFn 一并注入账号对象：Credits/Checkin/Keepalive 是账号自己的
// 能力，不该由调用方（池/调度器）代持上游依赖。
//
// 域判定：优先账号自身凭证（userRegion / host），缺失时回落配置 base。
// **美国区（us）账号不进池**——它是真实存在但本项目未实现的区域，让它进池
// 会被当成普通账号选中，然后 401，再被记成一次上游故障污染熔断判据
// （参考项目 issue #66 的教训）。
func Wrap(cfg *traepkg.Config, client *traepkg.Client) []gateway.Account {
	if cfg == nil {
		return nil
	}
	fallbackRealm := realmOfBase(cfg.ChatBase, cfg.APITraeBase)
	out := make([]gateway.Account, 0, len(cfg.Accounts))
	for i := range cfg.Accounts {
		a := &cfg.Accounts[i]
		// 先判「凭证声明了什么区域」：US 的排除与逃生门无关，不能因为
		// global.enabled=false 就把 US 账号当成 CN 放进来。
		region := a.Region(cfg)
		if !region.IsServable() {
			log.Printf("WARN: [trae] 账号 %s 属于未支持的区域 %q（host=%q userRegion=%q），已排除出调度池",
				a.UID, region, a.Host, a.UserRegion)
			continue
		}
		// 再应用逃生门：global.enabled=false 时国际账号降级为 cn（纯 CN 锁定）。
		realm := a.RegionOf(cfg).Realm()
		if realm == "" {
			realm = fallbackRealm
		}
		out = append(out, Account{
			A: a, realm: realm, region: region, dir: cfg.AuthDir,
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
//
// **国际域（global）没有签到能力**：参考实现实测（2026-09-24）
// `/trae/api/v2/ug/*` 全族在国际网关上不存在——growsg-normal / api-sg-central /
// api.trae.ai 均 404，www.trae.ai 回落 HTML 首页。
//
// 所以国际账号返回 Skipped，**不是 ErrUnsupported**：
//   - Skipped = 「这个账号正常，只是这类账号不参与」→ 池正常记账，面板能读到
//     （panel 的 checkin_done 只看 Skipped），可以显示「该区域无签到」；
//   - ErrUnsupported = 「这个上游没实现」→ 池静默跳过，用户什么也看不到。
//
// 与 codebuddy 企业版的处理逐字对齐（那边也是 Skipped + 说明）。
// **不要**在上层（调度器/池）按域过滤——那会让账号在面板上神秘消失，
// 且 CodeBuddy 的 global 账号是照常签到的，两条上游的行为应当一致。
func (x Account) Checkin(ctx context.Context) (*gateway.CheckinResult, error) {
	// 域判定放在依赖检查**之前**：它不需要 client，且「这个域没有签到能力」
	// 是比「对象没装配好」更本质的答案。装配缺失时也不该把它误报成上游故障。
	if x.realm == string(traepkg.RegionGlobal) {
		return &gateway.CheckinResult{
			Skipped: true,
			Message: "国际版（global 域）无签到体系",
		}, nil
	}
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
