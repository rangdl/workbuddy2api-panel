package server

import (
	"github.com/linguo2625469/workbuddy2api-panel/internal/gateway"
)

// backends.go 多上游注册表接入。
//
// 注册表由 main 构造并注入（各 backend 需要各自的配置与客户端），handler 只读。
// 未注入时（老部署 / 测试裸用）退化为空注册表：Trae 前缀解析不到，
// 行为与引入多上游之前完全一致。

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
