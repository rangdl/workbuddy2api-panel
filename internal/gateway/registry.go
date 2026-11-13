package gateway

import (
	"sort"
	"strings"
	"sync"
)

// registry.go Backend 注册表。
//
// 职责：按名字查找 backend、按账号反查其所属 backend、聚合全部别名供模型名
// 前缀解析。注册表是**只读**的（启动期注册完，运行期只查），所以用 RWMutex
// 而非更复杂的并发结构。

// Registry Backend 注册表（并发安全）。
type Registry struct {
	mu       sync.RWMutex
	backends map[string]Backend // 主名 → backend（不含别名）
	aliases  map[string]string  // 别名（小写）→ 主名
}

// NewRegistry 构造空注册表。
func NewRegistry() *Registry {
	return &Registry{
		backends: make(map[string]Backend),
		aliases:  make(map[string]string),
	}
}

// Register 注册一个 backend。
//
// 主名与别名都归一为小写。重复注册同名会覆盖（便于运行期热替换配置）。
func (r *Registry) Register(b Backend) {
	if b == nil || b.Name() == "" {
		return
	}
	name := strings.ToLower(b.Name())
	r.mu.Lock()
	defer r.mu.Unlock()
	r.backends[name] = b
	r.aliases[name] = name
	for _, a := range b.Aliases() {
		if a == "" {
			continue
		}
		r.aliases[strings.ToLower(a)] = name
	}
}

// Lookup 按名字或别名查 backend（未注册或未启用返回 nil）。
//
// Enabled() 的判断放在这里而不是调用方：模型名前缀解析、选号、模型列表
// 三处都需要「只认启用的」，集中一处避免各写一遍。
func (r *Registry) Lookup(name string) Backend {
	if name == "" {
		return nil
	}
	r.mu.RLock()
	canonical, ok := r.aliases[strings.ToLower(name)]
	if !ok {
		r.mu.RUnlock()
		return nil
	}
	b := r.backends[canonical]
	r.mu.RUnlock()
	if b == nil || !b.Enabled() {
		return nil
	}
	return b
}

// For 返回账号所属的 backend。
func (r *Registry) For(acc Account) Backend {
	if acc == nil {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.backends[strings.ToLower(acc.Backend())]
}

// All 返回全部已注册 backend（按主名排序，输出稳定）。
//
// 包含未启用的——调用方按需自行判断 Enabled()。
func (r *Registry) All() []Backend {
	r.mu.RLock()
	defer r.mu.RUnlock()
	names := make([]string, 0, len(r.backends))
	for n := range r.backends {
		names = append(names, n)
	}
	sort.Strings(names)
	out := make([]Backend, 0, len(names))
	for _, n := range names {
		out = append(out, r.backends[n])
	}
	return out
}

// Enabled 返回当前参与调度的 backend（按主名排序）。
func (r *Registry) Enabled() []Backend {
	all := r.All()
	out := make([]Backend, 0, len(all))
	for _, b := range all {
		if b.Enabled() {
			out = append(out, b)
		}
	}
	return out
}

// Names 返回全部可用标识（主名 + 别名 + 已启用 backend 的主名），
// 供 ParseModelRef 判断前缀。
//
// 只收录**已启用**的 backend 及其别名：未启用的上游不应把
// "trae:glm-5.3" 解析成「指定 trae」，否则用户会拿到「无可用账号」
// 而不是「未知模型」这种更准确的错误。
func (r *Registry) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.aliases))
	for alias, canonical := range r.aliases {
		b := r.backends[canonical]
		if b == nil || !b.Enabled() {
			continue
		}
		out = append(out, alias)
	}
	sort.Strings(out)
	return out
}

// SplitQualifiedUID 拆分 "backend:uid"。
//
// 找不到冒号时 backend 为空、uid 原样返回——这是单 backend 部署下的
// 向后兼容路径（历史粘性绑定值没有前缀）。
func SplitQualifiedUID(s string) (backend, uid string) {
	if i := strings.IndexByte(s, ':'); i >= 0 {
		return s[:i], s[i+1:]
	}
	return "", s
}
