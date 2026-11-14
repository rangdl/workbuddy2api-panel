package server

import (
	backendcb "github.com/linguo2625469/workbuddy2api-panel/internal/backend/codebuddy"
	"github.com/linguo2625469/workbuddy2api-panel/internal/gateway"
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
	reg.Register(backendcb.New(h.cfg.Upstream, func() bool { return true }, nil))
	h.cfg.Backends = reg
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
