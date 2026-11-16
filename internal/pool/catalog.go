// catalog.go 池的模型目录调度。
//
// 与 capabilities.go 的特色能力调度有一个**根本差异**，这决定了本文件的形态：
//
//	特色能力：逐号执行。每个号有自己的旅行状态、自己的签到记录，必须遍历。
//	模型目录：每上游一份。同一上游的任意健康号拉到的目录相同，遍历等于把
//	          同一个上游请求打好几遍。
//
// 所以这里的调度单位是**上游**而不是账号：每个上游挑一个此刻能用的号，
// 拉一次目录。哪个号被选中不影响结果，但**必须用池的健康判据挑**——
// 改造前 panel/server 各自遍历配置取「第一个非禁用账号」，会拿一个正在冷却
// 或已熔断的号去拉目录，拉失败后回落兜底表，用户看到的是过时列表。
package pool

import (
	"context"
	"errors"
	"log"
	"sort"
	"sync"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/gateway"
)

// catalogCacheEntry 单个 (上游, 域) 的缓存条目。
type catalogCacheEntry struct {
	models   []gateway.CatalogModel
	fetched  time.Time // 最近一次成功拉取
	lastFail time.Time // 最近一次失败（负缓存）
	lastErr  error
}

// CatalogEntry 一条模型条目，带所属上游标识。
type CatalogEntry struct {
	// Backend 所属上游名（gateway.Account.Backend()）。
	Backend string
	// Model 目录内容（裸 ID，不含前缀）。
	Model gateway.CatalogModel
}

// CatalogFallback 目录兜底提供者。
//
// 为什么需要：池里的账号**不一定**是 backend 包装后的完整实现。pool.WrapAuth
// 产生的最小适配器只承载身份与凭证（无 HTTP 客户端），它不实现 Cataloger；
// 生产路径上账号经 backend 包装，天然实现，但占位账号、测试池、以及将来某个
// 未实现该接口的上游都会走到兜底。
//
// 没有兜底时的行为是「整片模型列表消失」——这个后果比「多一条兜底路径」严重
// 得多。注入函数而不是让池 import upstream：依赖方向仍然是 pool → gateway，
// 由 main 装配时把具体上游的能力接进来（与 SetPlaceholderFactory 同一模式）。
type CatalogFallback func(ctx context.Context, backend string, acc gateway.Account) ([]gateway.CatalogModel, error)

// SetCatalogFallback 注入目录兜底提供者（nil = 不兜底）。
func (p *Pool) SetCatalogFallback(fn CatalogFallback) {
	p.mu.Lock()
	p.catalogFallback = fn
	p.mu.Unlock()
}

// CatalogResult 单个上游的目录拉取结果。
type CatalogResult struct {
	// Backend 上游名。
	Backend string
	// Realm 本批目录所属的域（cn / global / 空 = 不区分）。
	//
	// 为什么由池给出而不是让消费方猜：CodeBuddy 的域是**账号属性**，同一个上游
	// 的不同账号可能分属 cn 与 global，各自返回不同目录。池按账号分流拉取，
	// 自然知道这一批是哪个域的；消费方（面板/API）只负责加前缀，不该重新推导。
	Realm string
	// Models 该上游的模型列表（已排序）。失败或该上游无可用号时为空。
	Models []gateway.CatalogModel
	// Err 拉取失败原因（无可用账号 / 上游报错）。为空表示成功。
	//
	// 注意「上游没有这个能力」（ErrUnsupported）不算失败：那是该上游不提供
	// 目录，静默跳过即可，不该让整个列表报错。
	Err error
}

// CatalogAll 拉取所有上游的模型目录（并发执行，带进程内缓存）。
//
// 分组单位是 (上游, 域) 而不是单单上游：CodeBuddy 的 cn 与 global 是两个
// 独立目录，混成一批就没法给条目加正确的前缀。Trae 只有一个域，自然退化为
// 单个结果。
//
// 缓存语义（每个分组独立）：
//   - 成功结果缓存 catalogTTL（10 分钟）；
//   - 失败进负缓存 catalogFailCooldown（5 分钟），期间不重复打上游。
//
// 为什么缓存必须在池里而不是各消费方：改造前 server 与 panel 各有一份
// （CN 10 分钟 / global 1 小时 / Trae 30 分钟），三份 TTL 不同、互不可见，
// 于是「面板看到的模型列表」与「/v1/models 返回的」机制上就允许不一致。
// 收进来之后两者读的是同一个缓存，一致性是结构性保证。
//
// 失败处理：目录接口偶发 5xx 不代表账号的对话通道不可用，所以这里**不**
// NoteError、不进冷却、不罚号（models 拉取失败 ≠ chat 不可用）。错误只在
// 结果里回传，由调用方决定怎么展示。
//
// 返回顺序按 (上游, 域) 排序，保证面板与 API 的条目顺序稳定。
func (p *Pool) CatalogAll(ctx context.Context) []CatalogResult {
	groups := p.catalogGroups()
	out := make([]CatalogResult, len(groups))
	var wg sync.WaitGroup
	for i, g := range groups {
		wg.Add(1)
		go func(i int, g catalogGroup) {
			defer wg.Done()
			out[i] = p.catalogCached(ctx, g)
		}(i, g)
	}
	wg.Wait()
	return out
}

const (
	// catalogTTL 成功目录的缓存时长。
	//
	// 10 分钟是既有 server 侧参数（原 dynamicModelsTTL）的直接继承：目录新增
	// 模型时面板立即可见（面板可 force 刷新），公开端点最多滞后一个 TTL。
	catalogTTL = 10 * time.Minute
	// catalogFailCooldown 失败后的负缓存时长（原 modelsFetchFailCooldown）。
	//
	// 存在的理由与 TTL 不同：上游故障时若无冷却窗口，客户端轮询会持续放大
	// 请求量——负缓存正是为此设计，不是性能优化。
	catalogFailCooldown = 5 * time.Minute
)

// catalogCached 带缓存地拉一个分组。
func (p *Pool) catalogCached(ctx context.Context, g catalogGroup) CatalogResult {
	now := p.now()
	p.catalogMu.Lock()
	if e, ok := p.catalogCache[g]; ok {
		if len(e.models) > 0 && now.Sub(e.fetched) < catalogTTL {
			res := CatalogResult{Backend: g.backend, Realm: g.realm, Models: e.models}
			p.catalogMu.Unlock()
			return res
		}
		if !e.lastFail.IsZero() && now.Sub(e.lastFail) < catalogFailCooldown {
			res := CatalogResult{Backend: g.backend, Realm: g.realm, Models: e.models, Err: e.lastErr}
			p.catalogMu.Unlock()
			return res
		}
	}
	p.catalogMu.Unlock()

	res := p.catalogForGroup(ctx, g)
	if res.Err != nil && !errors.Is(res.Err, gateway.ErrUnsupported) {
		log.Printf("WARN: [pool] %s/%s 模型目录拉取失败（%s 内不再重试）: %v",
			g.backend, g.realm, catalogFailCooldown, res.Err)
	}

	p.catalogMu.Lock()
	if p.catalogCache == nil {
		p.catalogCache = make(map[catalogGroup]catalogCacheEntry, 4)
	}
	e := p.catalogCache[g]
	e.lastErr = res.Err
	if res.Err == nil && len(res.Models) > 0 {
		e.models = res.Models
		e.fetched = now
		e.lastFail = time.Time{} // 成功清空负缓存
	} else if res.Err != nil {
		e.lastFail = now
	}
	p.catalogCache[g] = e
	p.catalogMu.Unlock()
	return res
}

// CachedCatalog 只读某个上游的目录缓存（**不触发上游拉取**，过期也不刷新）。
//
// 用途是错误路径上的 gateway_hint 判定：那里加一次网络调用既拖慢错误响应，
// 又在错误风暴时放大上游请求量。缓存冷或过期 → nil（调用方退中性提示）。
func (p *Pool) CachedCatalog(backend string) []gateway.CatalogModel {
	now := p.now()
	p.catalogMu.Lock()
	defer p.catalogMu.Unlock()
	var out []gateway.CatalogModel
	for g, e := range p.catalogCache {
		if g.backend != backend || len(e.models) == 0 {
			continue
		}
		if now.Sub(e.fetched) >= catalogTTL {
			continue
		}
		out = append(out, e.models...)
	}
	return out
}

// InvalidateCatalog 清空目录缓存（配置变更 / 手动刷新时调用）。
func (p *Pool) InvalidateCatalog() {
	p.catalogMu.Lock()
	p.catalogCache = nil
	p.catalogMu.Unlock()
}

// CatalogForBackend 只拉指定上游的目录（找不到该上游返回空结果）。
//
// 上游可能有多个域（CodeBuddy 的 cn/global），此时返回第一个非空结果。
// 需要完整多域时用 CatalogAll 后按 Backend 过滤。
func (p *Pool) CatalogForBackend(ctx context.Context, backend string) CatalogResult {
	for _, g := range p.catalogGroups() {
		if g.backend != backend {
			continue
		}
		if res := p.catalogForGroup(ctx, g); res.Err == nil && len(res.Models) > 0 {
			return res
		}
	}
	return CatalogResult{Backend: backend, Err: gateway.ErrNoAccount}
}

// catalogGroup 一个 (上游, 域) 分组。
type catalogGroup struct {
	backend string
	realm   string
}

// catalogForGroup 拉单个分组的目录：挑一个可用号，失败就换下一个号。
//
// 为什么允许换号：同一个上游同域的号拉到的目录相同，但**可用性不同**——某个号
// 可能刚好凭证过期或撞上风控。换号重试的成本只是一次 HTTP，比整上游缺列表
// 便宜得多。上限取该分组的号数，不设固定次数（号少时不必硬凑）。
func (p *Pool) catalogForGroup(ctx context.Context, g catalogGroup) CatalogResult {
	res := CatalogResult{Backend: g.backend, Realm: g.realm}
	candidates := p.catalogCandidates(g)
	if len(candidates) == 0 {
		res.Err = gateway.ErrNoAccount
		return res
	}
	unsupported := 0
	fallbackTried := false
	var lastErr error
	for _, acc := range candidates {
		if err := ctx.Err(); err != nil {
			res.Err = err
			return res
		}
		cat, ok := acc.(gateway.Cataloger)
		if !ok {
			// 账号没实现目录能力。
			//
			// 兜底只试**一次**：它是上游级能力（同一个上游的所有号共享一份目录），
			// 逐号重试只会把同一次探测重复 N 遍——实测会让上游探测次数翻倍、
			// 负缓存也失效。
			if fallbackTried || p.catalogFallbackAbsent() {
				unsupported++
				continue
			}
			fallbackTried = true
			models, err := p.catalogViaFallback(ctx, g.backend, acc)
			switch {
			case errors.Is(err, gateway.ErrUnsupported):
				unsupported++
			case err != nil:
				lastErr = err
			case len(models) > 0:
				sort.Slice(models, func(i, j int) bool { return models[i].ID < models[j].ID })
				res.Models = models
				return res
			}
			continue
		}
		models, err := cat.Catalog(ctx)
		switch {
		case errors.Is(err, gateway.ErrUnsupported):
			// 该号不适用（无凭证 / 入口未启用）：换同组的下一个号。
			unsupported++
			continue
		case err != nil:
			// 失败不罚号，只记下来继续试下一个。
			lastErr = err
			log.Printf("catalog %s/%s: %v", g.backend, g.realm, err)
			continue
		case len(models) == 0:
			// 空目录不是错误：上游可能确实没有模型，或该号暂时看不见。
			// 继续试下一个号，全空则以「无模型」收尾（不是 Err）。
			continue
		default:
			sort.Slice(models, func(i, j int) bool { return models[i].ID < models[j].ID })
			res.Models = models
			return res
		}
	}
	// 全部号都没给出目录：区分「不支持」与「真失败」。
	if lastErr != nil {
		res.Err = lastErr
	} else if unsupported == len(candidates) {
		res.Err = gateway.ErrUnsupported
	}
	return res
}

// now 当前时刻（可注入，测试用）。
func (p *Pool) now() time.Time {
	if p.nowFn != nil {
		return p.nowFn()
	}
	return time.Now()
}

// SetCatalogClock 注入目录缓存的时钟（测试用；nil = time.Now）。
func (p *Pool) SetCatalogClock(fn func() time.Time) {
	p.mu.Lock()
	p.nowFn = fn
	p.mu.Unlock()
}

// catalogFallbackAbsent 是否未注入兜底（避免逐号重复取锁）。
func (p *Pool) catalogFallbackAbsent() bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.catalogFallback == nil
}

// catalogViaFallback 账号未实现 Cataloger 时走池级兜底。
//
// 未注入兜底时返回 ErrUnsupported（调用方按「该上游不提供目录」静默跳过）。
func (p *Pool) catalogViaFallback(ctx context.Context, backend string, acc gateway.Account) ([]gateway.CatalogModel, error) {
	p.mu.RLock()
	fn := p.catalogFallback
	p.mu.RUnlock()
	if fn == nil {
		return nil, gateway.ErrUnsupported
	}
	return fn(ctx, backend, acc)
}

// catalogGroups 返回池中所有 (上游, 域) 组合（去重排序）。
//
// 取成员快照而不是已注册 backend 列表：池本来就不认识 Backend 注册表，
// 它只知道手里有哪些账号、各自属于哪个上游与域。这也顺带保证「没有账号的
// 上游不出现」——没有号就拉不到目录，列出来只会多一行错误。
func (p *Pool) catalogGroups() []catalogGroup {
	seen := make(map[catalogGroup]bool, 4)
	for _, m := range p.Members(MemberFilter{IncludeDisabled: true, IncludePaused: true}) {
		if m.Acc == nil {
			continue
		}
		be := m.Acc.Backend()
		if be == "" {
			continue
		}
		// 域为空的上游（如 Trae 只有 CN 但 Realm() 返回 "cn"）照常参与；
		// 空域仍是合法分组，消费方按上游决定前缀。
		seen[catalogGroup{backend: be, realm: m.Acc.Realm()}] = true
	}
	out := make([]catalogGroup, 0, len(seen))
	for g := range seen {
		out = append(out, g)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].backend != out[j].backend {
			return out[i].backend < out[j].backend
		}
		return out[i].realm < out[j].realm
	})
	return out
}

// catalogCandidates 该分组此刻可用的账号，**健康号优先**。
//
// 顺序很关键：先返回没在冷却的号，再返回冷却中的号。这样正常情况永远用健康号；
// 只有当所有号都在冷却时才会退而求其次——宁可拿一个冷却号试一次，也好过整个
// 上游的模型列表凭空消失。
//
// 判据用 Status.Cooling 而不是 gateLocked：gateLocked 要 *entry，而这里拿到
// 的是快照里的 gateway.Account。冷却是最主要的排除原因（熔断/降权都通过
// 冷却时长体现），对「挑一个号拉目录」这个用途已经够用。
func (p *Pool) catalogCandidates(g catalogGroup) []gateway.Account {
	healthy := make([]gateway.Account, 0, 4)
	cooling := make([]gateway.Account, 0, 4)
	// IncludeDisabled=false：禁用号凭证已被上游判定失效，拉目录只会拿到同一批错误。
	for _, m := range p.Members(MemberFilter{Backend: g.backend, IncludePaused: true}) {
		if m.Acc == nil || m.Acc.Realm() != g.realm {
			continue
		}
		if m.Status.Cooling {
			cooling = append(cooling, m.Acc)
			continue
		}
		healthy = append(healthy, m.Acc)
	}
	return append(healthy, cooling...)
}

// ModelRateFor 查某账号对应模型的积分倍率（对话收尾估算扣费用）。
//
// 走账号自己的 gateway.ModelRate 实现：不同上游的倍率来源不同（CodeBuddy 读
// 目录快照、Trae 读 remote 目录缓存），池不关心。未实现该接口的账号返回
// false——调用方据此把扣费标成「未知」，不估算。
func (p *Pool) ModelRateFor(ctx context.Context, acc gateway.Account, model string) (float64, bool) {
	if acc == nil {
		return 0, false
	}
	r, ok := acc.(gateway.ModelRate)
	if !ok {
		return 0, false
	}
	return r.RateFor(ctx, model)
}
