package server

import (
	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	backendcb "github.com/linguo2625469/workbuddy2api-panel/internal/backend/codebuddy"
	"github.com/linguo2625469/workbuddy2api-panel/internal/gateway"
	"github.com/linguo2625469/workbuddy2api-panel/internal/prompt"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// backends.go 多上游注册表接入。
//
// 注册表由 main 构造并注入（各 backend 需要各自的配置与客户端），handler 只读。
// 未注入时（老部署 / 测试裸用）退化为空注册表：Trae 前缀解析不到，
// 行为与引入多上游之前完全一致。
//
// 兼容垫片：只给了 Upstream（没有注册表）的调用方——大量既有测试与老部署就是
// 这种形态——会在 NewHandler 里自动注册一个 CodeBuddy backend。否则「CodeBuddy
// 走统一 Backend.Chat」这条路径对它们不可达，测试与生产会跑在不同的实现上。

// ensureCodeBuddyBackend 在未注入注册表但给了 Upstream 时补一个 CodeBuddy backend。
//
// 幂等：已注册同名 backend 时不重复注册。
func (h *Handler) ensureCodeBuddyBackend() {
	if h.cfg.Backends != nil {
		return // 调用方显式装配了注册表，尊重其内容
	}
	if h.cfg.Upstream == nil {
		return
	}
	reg := gateway.NewRegistry()
	reg.Register(h.newCodeBuddyBackend())
	h.cfg.Backends = reg
}

// newCodeBuddyBackend 构造带降级重试钩子的 CodeBuddy backend。
//
// 降级重试（内容拦截误报 → 换中性提示词重试同一账号）依赖 server 侧的
// degradeGate 与提示词配置，所以在这里注入而不是放进 backend 包。
func (h *Handler) newCodeBuddyBackend() *backendcb.Backend {
	return &backendcb.Backend{
		Client:    h.cfg.Upstream,
		EnabledFn: func() bool { return true },
		Retry:     degradeRetryHook{gate: h.degrade, mode: h.cfg.PromptMode},
	}
}

// NewCodeBuddyBackend 供 cmd/server 装配用：构造**带降级重试**的 CodeBuddy backend。
//
// 为什么必须导出一个构造器而不是让 main 直接 backendcb.New：
// 生产装配走的是「显式注入 Registry」这条路径，而 ensureCodeBuddyBackend 的
// 兼容垫片在「已有注册表」时直接 return。若 main 用 backendcb.New（不带 Retry），
// 生产环境的 RetryHook 恒为 nil —— 内容是别误报后直接 400，降级重试完全失效。
// 而测试走垫片路径（有 Retry），所以测试全绿、线上失效，是最难发现的那类不一致。
//
// gate 与 mode 由调用方传入：它们属于网关策略，backend 包不认识。
func NewCodeBuddyBackend(c *upstream.Client, enabled func() bool,
	load func() ([]*auth.Auth, error), gate *DegradeGate, promptMode string) *backendcb.Backend {
	return &backendcb.Backend{
		Client:    c,
		EnabledFn: enabled,
		LoadFn:    load,
		Retry:     degradeRetryHook{gate: gate, mode: promptMode},
	}
}

// DegradeGate 降级状态机的对外句柄（main 需要跨 Handler 共享同一实例）。
type DegradeGate = degradeGate

// NewDegradeGate 构造一个空的降级状态机。
func NewDegradeGate() *DegradeGate { return &degradeGate{} }

// degradeRetryHook 内容拦截时的同账号重试改写。
//
// 只在 passthrough / append 模式生效：这两个模式会透传客户端原始 system，
// 而拦截多半是 system 里的固定模板句被逐字误杀。custom 模式用的是网关自己的
// 提示词，被拦就是用户内容本身有问题，重试没有意义。
type degradeRetryHook struct {
	gate *degradeGate
	mode string
}

// OnFailure 命中内容拦截时把 system 换成中性提示词并重试一次。
func (d degradeRetryHook) OnFailure(in *gateway.Inbound, f *gateway.Failure) (*gateway.Inbound, bool) {
	if f == nil || f.Decision.Kind != upstream.ErrContentBlocked.String() {
		return nil, false
	}
	if d.mode != "passthrough" && d.mode != "append" {
		return nil, false
	}
	if d.gate != nil {
		d.gate.Trigger()
	}
	// Inbound.Raw 只读约定：必须产出新副本，不能原地改（同一份 Inbound 可能
	// 被多个上游依次尝试）。
	next := *in
	next.Raw = prompt.Rewrite(in.Raw, prompt.Degraded)
	// 同一请求的 ChatID 保持不变：客户端不该把重试结果当成新消息。
	return &next, true
}

// backendNames 返回可用于模型名前缀的上游标识（主名 + 别名，仅已启用）。
func (h *Handler) backendNames() []string {
	if h.cfg.Backends == nil {
		return nil
	}
	return h.cfg.Backends.Names()
}

// backendFor 返回账号所属的上游实现。
func (h *Handler) backendFor(acc gateway.Account) gateway.Backend {
	if h.cfg.Backends == nil || acc == nil {
		return nil
	}
	return h.cfg.Backends.For(acc)
}

// enabledBackends 返回当前参与调度的上游（按名排序）。
func (h *Handler) enabledBackends() []gateway.Backend {
	if h.cfg.Backends == nil {
		return nil
	}
	return h.cfg.Backends.Enabled()
}
