// catalog.go Trae 账号的模型目录能力（gateway.Cataloger / gateway.ModelRate）。
//
// 改造前这段逻辑在 server 与 panel 各有一份完整副本（连挑账号的函数都各写了
// 一个 firstTraeAccount / firstEnabledTraeAccount），两份的差异只有「要不要裸名
// ID」和「要不要 upstream 字段」——那本来就该是消费方的参数，不是两份实现。
package trae

import (
	"context"
	"sort"

	"github.com/linguo2625469/workbuddy2api-panel/internal/gateway"
	traepkg "github.com/linguo2625469/workbuddy2api-panel/internal/trae"
)

// Catalog 拉取 Trae 的模型目录。
//
// 只列**真正能当模型调用**的行（trae.IsCallableModel）。Remote 目录把模型和
// IDE 自带工具（summary / title_generation / fast_apply）混在一起，不过滤会让
// 客户端看到一堆积木名。判据来自 dsh 实测，见 trae/remote_models.go。
//
// 上游目录拉不到时回落内置模型表（trae.KnownModels）：至少让用户知道存在哪些
// 模型，只是没有上下文与定价。这与「拉取失败」要区分——回落成功不是错误。
func (x Account) Catalog(ctx context.Context) ([]gateway.CatalogModel, error) {
	if x.client == nil || x.A == nil || x.ConfigFn == nil {
		return nil, gateway.ErrUnsupported
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	cfg := x.ConfigFn()
	if cfg == nil {
		return nil, gateway.ErrUnsupported
	}
	remote := x.client.FetchRemoteModels(ctx, cfg, x.A, false)

	ids := make([]string, 0, len(remote))
	if len(remote) > 0 {
		for id, row := range remote {
			if !traepkg.IsCallableModel(row) {
				continue
			}
			ids = append(ids, id)
		}
	} else {
		ids = traepkg.KnownModels()
	}
	if len(ids) == 0 {
		return nil, nil
	}
	sort.Strings(ids)

	out := make([]gateway.CatalogModel, 0, len(ids))
	for _, id := range ids {
		out = append(out, traeCatalogModel(id, remote[id]))
	}
	return out, nil
}

// traeCatalogModel 把一行 RemoteModel 转成统一条目。
//
// 上下文取扩展档优先：max_mode 是上游的「扩展上下文」开关，开了才真正按 max
// 服务。上游对多数模型都下发 max 数值，但不看 max_mode 就照抄会虚报窗口。
func traeCatalogModel(id string, row traepkg.RemoteModel) gateway.CatalogModel {
	name := row.DisplayName
	if name == "" {
		name = id
	}
	m := gateway.CatalogModel{
		ID:                id,
		Name:              name,
		ContextLength:     row.ContextWindow,
		SupportsImages:    row.Multimodal,
		SupportsReasoning: row.ReasoningSupported,
		Credits:           row.RateText,
	}
	if row.MaxContextWindow > 0 {
		m.ContextLength = row.MaxContextWindow
		m.ContextLengthBase = row.ContextWindow
	}
	// 思考档位：上游公布了用上游的，未公布时**不**回落到静态表——Trae 没有
	// 像 CodeBuddy 那样的产品静态档位表，凭空给一档等于编造。
	if len(row.ReasoningEfforts) > 0 {
		m.Efforts = row.ReasoningEfforts
		m.DefaultEffort = row.ReasoningDefault
	}
	return m
}

// RateFor 取某个模型的积分倍率（对话收尾时估算扣费用）。
//
// Trae 的 chat 流不含扣费字段（实测），只能按 token × 倍率推算，所以这个查询
// 在**每次对话收尾**都会发生。靠 FetchRemoteModels 的 30 分钟进程内缓存命中，
// 不会真的每次发请求。
func (x Account) RateFor(ctx context.Context, model string) (float64, bool) {
	if x.client == nil || x.A == nil || model == "" || x.ConfigFn == nil {
		return 0, false
	}
	cfg := x.ConfigFn()
	if cfg == nil {
		return 0, false
	}
	cat := x.client.FetchRemoteModels(ctx, cfg, x.A, false)
	row, ok := cat[model]
	if !ok || row.CreditMultiplier == nil || *row.CreditMultiplier <= 0 {
		// 未知或 0 都返回 false：0 在这个上下文等于「免费」，而目录里没有倍率
		// 字段的模型也会走到这里，混淆会让估算凭空变成 0。
		return 0, false
	}
	return *row.CreditMultiplier, true
}
