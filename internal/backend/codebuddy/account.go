// Package codebuddy 把 internal/upstream（CodeBuddy 上游）适配成 gateway.Backend。
//
// 依赖方向：codebuddy → upstream/auth/gateway。gateway 不反向依赖任何 internal 包，
// pool 也不认识本包——三者通过接口解耦。
package codebuddy

import (
	"context"
	"errors"
	"log"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/gateway"
	"github.com/linguo2625469/workbuddy2api-panel/internal/logfmt"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// Name 上游标识。
const Name = "codebuddy"

// Account 把 *auth.Auth 适配成 gateway.Account。
//
// 为什么包装而不是直接给 auth.Auth 加方法：那会让 internal/auth 反向依赖
// gateway，破坏「gateway 不依赖任何 internal 包」的设计前提。
type Account struct {
	A *auth.Auth
	// client 是账号**自己的能力依赖**：Credits/Checkin/Keepalive 由账号对象
	// 自己实现，池与调度器都不需要认识 upstream 包。
	client *upstream.Client
	// expiringSoon 快过期窗口（余额查询时把窗口内到期的套餐标记出来）。
	expiringSoon func() time.Duration
}

func (x Account) UID() string      { return x.A.UID }
func (x Account) Nickname() string { return x.A.NicknameValue() }
func (x Account) Realm() string    { return x.A.Realm() }
func (x Account) Backend() string  { return Name }
func (x Account) Enterprise() bool { return x.A.IsEnterprise() }

// Credits 查询该账号余额（含按日到期批次）。
//
// 口径与企业版分流都在 upstream.UserResourceSnapshot 内部，这里只做形态转换：
// 把一长串返回值收成 gateway.Credits，池拿到的是与上游无关的结构。
//
// 快过期窗口（Expiring）不由账号对象决定：那是**调度策略**，同类信息在池侧
// 统一按当前配置计算（见 pool.RefreshCredits），换配置即时生效。这里固定传 0。
func (x Account) Credits(ctx context.Context) (*gateway.Credits, error) {
	if x.client == nil || x.A == nil {
		return nil, gateway.ErrUnsupported
	}
	remain, total, _, earliestAt, _, buckets, err :=
		x.client.UserResourceSnapshot(x.A, 0)
	if err != nil {
		return nil, err
	}
	out := &gateway.Credits{
		Available: float64(remain),
		Limit:     float64(total),
		Earliest:  earliestAt,
		Unlimited: x.A.IsEnterprise(),
	}
	if len(buckets) > 0 {
		out.Buckets = make([]gateway.ExpiryBucket, len(buckets))
		for i, b := range buckets {
			out.Buckets[i] = gateway.ExpiryBucket{Date: b.Date, ExpireAt: b.ExpireAt, Remain: b.Remain}
		}
	}
	return out, nil
}

// Checkin 对该账号执行一次签到。
//
// 企业版无签到体系（上游 400 code 10001）：返回 Skipped 而不是错误——这不
// 是失败，只是这类账号不参与，池按正常流程记账即可。
func (x Account) Checkin(ctx context.Context) (*gateway.CheckinResult, error) {
	if x.client == nil || x.A == nil {
		return nil, gateway.ErrUnsupported
	}
	if x.A.IsEnterprise() {
		return &gateway.CheckinResult{
			Skipped: true,
			Message: "企业账号无签到体系",
		}, nil
	}
	if err := x.client.DailyCheckin(x.A); err != nil {
		// 「今天已签到」是幂等成功，不该当失败。
		if upstream.IsAlreadyCheckin(err) {
			return &gateway.CheckinResult{Already: true, Message: err.Error()}, nil
		}
		return nil, err
	}
	return &gateway.CheckinResult{Message: "签到成功"}, nil
}

// Keepalive 刷新该账号的 token。
func (x Account) Keepalive(ctx context.Context) error {
	if x.client == nil || x.A == nil {
		return gateway.ErrUnsupported
	}
	return x.client.RefreshToken(x.A)
}

// NeedsRefresh 实现 gateway.Freshener：token 是否已进入需要提前刷新的窗口。
func (x Account) NeedsRefresh(within time.Duration) bool {
	if x.A == nil {
		return false
	}
	return x.A.NeedsRefresh(within)
}

// Freshen 刷新 token 并落盘。
//
// 落盘失败只记日志不报错：刷新本身已成功，本次请求可以继续；下次启动会用旧
// token，但那时会再刷一次。把落盘失败升级成请求失败会让一次磁盘故障演变成
// 全量请求 5xx。
func (x Account) Freshen(ctx context.Context) error {
	if x.client == nil || x.A == nil {
		return errors.New("codebuddy: account has no client")
	}
	if err := x.client.RefreshToken(x.A); err != nil {
		return err
	}
	if err := x.A.SaveAtomic(); err != nil {
		log.Printf("ERR: [codebuddy] freshen acct=%s: save auth failed: %v",
			logfmt.Label(x.A.UID, x.A.NicknameValue()), err)
	}
	return nil
}

// Credential 实现 gateway.CredentialUnwrapper。
func (x Account) Credential() any {
	if x.A == nil {
		return nil
	}
	return x.A
}

// Unwrap 取出原始凭证（非本 backend 的账号返回 nil）。
//
// 两条路径都要认：
//   - 生产路径的账号是本包的 Account（backend 换装后入池）；
//   - 测试/占位路径的账号是 pool.authAdapter，它通过 gateway.CredentialUnwrapper
//     暴露凭证。只认第一种的话，把 CodeBuddy 切到 Backend.Chat 后所有测试账号
//     会集体解不出凭证（nil）——表现为「测试全挂但生产正常」这种最难查的形态。
func Unwrap(acc gateway.Account) *auth.Auth {
	switch a := acc.(type) {
	case Account:
		return a.A
	case *Account:
		return a.A
	}
	if u, ok := acc.(gateway.CredentialUnwrapper); ok {
		if a, ok := u.Credential().(*auth.Auth); ok {
			return a
		}
	}
	return nil
}

// Wrap 批量包装（不带能力依赖，仅供不需要 Credits/Checkin 的调用方使用）。
func Wrap(list []*auth.Auth) []gateway.Account {
	out := make([]gateway.Account, 0, len(list))
	for _, a := range list {
		if a != nil {
			out = append(out, Account{A: a})
		}
	}
	return out
}

// WrapWithClient 包装并注入能力依赖（池的账号走这条路径）。
func WrapWithClient(list []*auth.Auth, client *upstream.Client, expiringSoon func() time.Duration) []gateway.Account {
	out := make([]gateway.Account, 0, len(list))
	for _, a := range list {
		if a != nil {
			out = append(out, Account{A: a, client: client, expiringSoon: expiringSoon})
		}
	}
	return out
}
