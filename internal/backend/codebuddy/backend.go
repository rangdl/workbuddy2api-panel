package codebuddy

import (
	"context"
	"errors"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/gateway"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// Backend CodeBuddy 上游实现。
type Backend struct {
	Client *upstream.Client
	// EnabledFn 运行期开关（热重载后即时生效）。
	EnabledFn func() bool
	// LoadFn 从 auths/ 目录装载账号（返回原始凭证，本包负责包装）。
	LoadFn func() ([]*auth.Auth, error)
}

// New 构造 backend。
func New(c *upstream.Client, enabled func() bool, load func() ([]*auth.Auth, error)) *Backend {
	return &Backend{Client: c, EnabledFn: enabled, LoadFn: load}
}

func (b *Backend) Name() string { return Name }

// Aliases "wb" 是 workbuddy 的缩写（面板里两个叫法混用）。
func (b *Backend) Aliases() []string { return []string{"wb"} }

func (b *Backend) Enabled() bool {
	if b == nil || b.Client == nil {
		return false
	}
	if b.EnabledFn != nil {
		return b.EnabledFn()
	}
	return true
}

func (b *Backend) Accounts() ([]gateway.Account, error) {
	if b.LoadFn == nil {
		return nil, errors.New("codebuddy: load function not configured")
	}
	raw, err := b.LoadFn()
	if err != nil {
		return nil, err
	}
	return Wrap(raw), nil
}

// Supports 该账号能否服务这个模型。
//
// 实现说明：CodeBuddy 的模型目录是按 realm 从上游动态拉的，本层不做网络查询；
// 只要上游目录里登记过该模型（或目录尚未拉到时保守放行）即视为支持。
// 是否真的能出票最终由上游决定（11102 会走 ActionModelCooldown 避让）。
func (b *Backend) Supports(acc gateway.Account, bareModel string) bool {
	if acc == nil || bareModel == "" {
		return true
	}
	a := Unwrap(acc)
	if a == nil {
		return false
	}
	if b.Client == nil {
		return true
	}
	// 倍率表就是上游目录的投影：有倍率条目即代表该 (realm, 模型) 组合在上游存在。
	// 目录未覆盖时保守放行——宁可让上游回 11102（走模型级避让），也不要因为
	// 目录未刷新就把可用账号整体排除。
	if rate := b.Client.ModelRate(a.Realm(), bareModel); rate != "" {
		return true
	}
	return true
}

// WireModel CodeBuddy 的 wire 名即模型名本身（不做映射）。
func (b *Backend) WireModel(acc gateway.Account, bareModel string) string { return bareModel }

// Chat 发一次请求并返回规范化回程。
func (b *Backend) Chat(ctx context.Context, acc gateway.Account, in *gateway.Inbound) (*gateway.Outbound, error) {
	a := Unwrap(acc)
	if a == nil {
		return nil, errors.New("codebuddy: account is not a CodeBuddy auth")
	}
	meta := upstream.ChatMeta{ConversationID: in.ConversationID}
	rc, status, body, err := b.Client.ChatStreamContext(ctx, a, in.Raw, in.ClientIP, meta)
	if err != nil {
		// 传输层错误：不是上游业务拒绝，交给调用方决定是否换号。
		var ue *upstream.Error
		if errors.As(err, &ue) {
			d := Decide(ue.Kind, ue.Status, ue.Msg, ue.RetryAfter)
			return nil, &gateway.Failure{Decision: d, HTTPStatus: ue.Status, Body: body}
		}
		return nil, err
	}
	if status >= 400 {
		kind := upstream.Classify(status, string(body))
		d := Decide(kind, status, string(body), 0)
		if rc != nil {
			rc.Close()
		}
		return nil, &gateway.Failure{Decision: d, HTTPStatus: status, Body: body}
	}
	return &gateway.Outbound{
		Status: status,
		Stream: rc,
		Convert: func(emit func(frame []byte) error) (gateway.StreamStats, error) {
			var stats gateway.StreamStats
			// errFrame 保存上游 error 帧原文（6004 限流 / 内容拦截 / 审核），
			// 流结束后据此判定账号处置。
			var errFrame string
			err := upstream.StreamTo(emit, rc,
				func(payload string) string { return "" }, // hint 由 handler 层决定，这里不加
				upstream.WithErrorFrameObserver(func(payload string) { errFrame = payload }),
				upstream.WithMarkupRepair(nil),
			)
			if errFrame != "" {
				stats.SentAny = true // 已下发内容后才发现错误帧，不能再改状态码
				stats.ErrorMsg = errFrame
				stats.ErrorCode = -1 // 非业务码：由 ErrorMsg 携带原文
			}
			if err != nil {
				// 空流（200 但无有效帧）不是写失败，交给调用方按上游故障处置。
				if upstream.IsEmptyStreamError(err) {
					return stats, err
				}
				return stats, err
			}
			stats.SentAny = true
			return stats, nil
		},
	}, nil
}

// Credits CodeBuddy 的余额由上游 usage 接口提供，此处不支持。
//
// 说明：余额刷新走 scheduler 的专用链路（它需要 realm、企业版判定等上下文），
// 不在本接口内重复实现。返回 ErrCreditsUnsupported 让调用方走原路径。
func (b *Backend) Credits(ctx context.Context, acc gateway.Account) (*gateway.Credits, error) {
	return nil, gateway.ErrCreditsUnsupported
}

// RetryHook CodeBuddy 的内容拦截降级重试由 handler 层实现（需要 prompt 包）。
func (b *Backend) RetryHook() gateway.RetryHook { return nil }

// 编译期断言。
var _ gateway.Backend = (*Backend)(nil)
