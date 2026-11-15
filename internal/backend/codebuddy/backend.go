package codebuddy

import (
	"context"
	"encoding/json"
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
	// Retry 内容拦截降级重试的改写器（nil = 不启用）。
	//
	// 由 server 注入：降级状态机（degradeGate）与提示词文本都是网关配置，
	// 不属于上游协议；backend 只负责「把改写后的 Inbound 再发一次」。
	Retry gateway.RetryHook
}

// New 构造 backend。
//
// Retry 字段不在这里设置：降级重试依赖 server 侧的 degradeGate 与提示词配置，
// 由装配方（server 的兼容垫片 / cmd/server/main.go）负责注入。
// 漏注入的表现是「内容拦截误报后直接 400，不再降级重试」——静默失效，
// 所以两处装配都必须设（见 server.newCodeBuddyBackend 与 main.go）。
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

// Chat 发一次请求并返回规范化回程。
//
// 本方法承载 CodeBuddy 的全部协议特化：会话头族（ConversationID /
// ConversationRequestID / TraceID）、出站请求体预处理、回程 SSE 规范化与
// DSML 工具调用标记修复、流内 error 帧识别。调度（选号/租约/换号/记账）
// 仍由 server 层负责——那是请求级职责，不属于单个上游。
func (b *Backend) Chat(ctx context.Context, acc gateway.Account, in *gateway.Inbound) (*gateway.Outbound, error) {
	a := Unwrap(acc)
	if a == nil {
		return nil, errors.New("codebuddy: account is not a CodeBuddy auth")
	}
	meta := upstream.ChatMeta{
		ConversationID:        in.ConversationID,
		ConversationRequestID: in.ConversationRequestID,
		TraceID:               in.TraceID,
	}
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
	// 工具名名单：上游在没有 tools 的请求里会把工具调用吐成原生标记文本，
	// 修复层用它做严格判定。名单来自「本请求声明的 tools」与「会话历史里
	// 出现过的工具名」两个来源——实测 tools 声明会在中转环节丢失。
	declaredTools := in.DeclaredTools
	return &gateway.Outbound{
		Status: status,
		Stream: rc,
		Convert: func(emit func(frame []byte) error) (gateway.StreamStats, error) {
			var stats gateway.StreamStats
			// errFrame 保存上游 error 帧原文（6004 限流 / 内容拦截 / 审核），
			// 流结束后据此判定账号处置。
			var errFrame string
			// usage 从帧里旁路采集：StreamTo 只负责转发，不解析用量。
			// 这是流式记账的唯一数据源（token / 缓存 / 积分估算都依赖它）。
			var usage map[string]any
			// 标记修复：第二个参数 true = 名单为空时启用弱判定——tools 声明在
			// 链路上丢失是实测最常见的泄漏成因，只做严格判定等于对主场景不设防。
			repair := upstream.NewMarkupRepair(declaredTools, true)
			// hintFn 惰性求值：正常流零开销，只有真撞到 error 帧才组装文案。
			hintFn := func(payload string) string {
				if in.FrameHint == nil {
					return ""
				}
				return in.FrameHint(upstream.FrameKind(payload).String(), payload)
			}
			err := upstream.StreamTo(emit, rc, hintFn,
				upstream.WithErrorFrameObserver(func(payload string) { errFrame = payload }),
				upstream.WithMarkupRepair(repair),
				upstream.WithUsageObserver(func(u map[string]any) { usage = u }),
			)
			stats.Repaired = repair.Converted()
			stats.RepairSeen = repair.Seen()
			stats.Usage = usageFromMap(usage)
			if errFrame != "" {
				stats.SentAny = true // 已下发内容后才发现错误帧，不能再改状态码
				stats.ErrorMsg = errFrame
				stats.ErrorCode = -1 // 非业务码：由 ErrorMsg 携带原文
				// 流内错误的分类只有本包知道：6004/11102 之类是 error 帧
				// 原文里的业务码，要按 FrameKind 认。填 Decision 后 server
				// 不需要再按上游名分流到两套分类器。
				kind := upstream.FrameKind(errFrame)
				if kind == upstream.ErrNone {
					// 非业务码（FrameKind 认不出）：按上游原文分类。
					// 传 0 而不是 int(stats.ErrorCode)：-1 不是合法 HTTP
					// status，会干扰分类。
					kind = upstream.Classify(0, errFrame)
				}
				d := Decide(kind, 0, errFrame, 0)
				stats.Decision = &d
			}
			if err != nil {
				return stats, err
			}
			stats.SentAny = true
			return stats, nil
		},
	}, nil
}

// RetryHook 内容拦截降级重试（由 server 注入，见 Backend.Retry）。
func (b *Backend) RetryHook() gateway.RetryHook { return b.Retry }

// 编译期断言。
var _ gateway.Backend = (*Backend)(nil)

// usageFromMap 把上游 usage 原文转成跨上游的归一口径（nil = 无 usage）。
//
// 放在 backend 而不是 upstream 包：upstream 不认识 gateway（分层约束），
// 而「上游 usage 字段怎么映射到统一结构」正是 backend 的职责。
func usageFromMap(u map[string]any) *gateway.Usage {
	if u == nil {
		return nil
	}
	out := &gateway.Usage{}
	got := false
	if v, ok := upstream.UsagePromptTokens(u); ok {
		out.PromptTokens = int64(v)
		got = true
	}
	if v, ok := usageInt(u, "completion_tokens"); ok {
		out.CompletionTokens = v
		got = true
	}
	if v, ok := usageInt(u, "total_tokens"); ok {
		out.TotalTokens = v
		got = true
	}
	if v, ok := upstream.UsageCacheHitTokens(u); ok {
		out.CacheRead = int64(v)
	}
	if v, ok := usageInt(u, "cache_creation_input_tokens"); ok {
		out.CacheCreation = v
	}
	if v, ok := usageInt(u, "reasoning_tokens"); ok {
		out.Reasoning = v
	}
	// credit：CodeBuddy 在 usage 里直接给出本次扣费（实测值，优先于估算）。
	if v, ok := u["credit"].(float64); ok {
		out.Credit, out.HasCredit = v, true
	}
	if !got {
		return nil
	}
	return out
}

// usageInt 从 usage 顶层取整数字段。
func usageInt(u map[string]any, key string) (int64, bool) {
	switch n := u[key].(type) {
	case float64:
		return int64(n), true
	case int:
		return int64(n), true
	case int64:
		return n, true
	case json.Number:
		v, err := n.Int64()
		return v, err == nil
	}
	return 0, false
}
