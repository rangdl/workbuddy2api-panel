// Package codebuddy 把 internal/upstream（CodeBuddy 上游）适配成 gateway.Backend。
//
// 依赖方向：codebuddy → upstream/auth/gateway。gateway 不反向依赖任何 internal 包，
// pool 也不认识本包——三者通过接口解耦。
package codebuddy

import (
	"context"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/gateway"
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

// Credential 实现 gateway.CredentialUnwrapper。
func (x Account) Credential() any {
	if x.A == nil {
		return nil
	}
	return x.A
}

// Unwrap 取出原始凭证（非本 backend 的账号返回 nil）。
func Unwrap(acc gateway.Account) *auth.Auth {
	if a, ok := acc.(Account); ok {
		return a.A
	}
	if a, ok := acc.(*Account); ok {
		return a.A
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
