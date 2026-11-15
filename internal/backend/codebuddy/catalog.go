// catalog.go CodeBuddy 账号的模型目录能力（gateway.Cataloger / gateway.ModelRate）。
//
// 改造前这段逻辑在 server 与 panel 各有一份：都自己 Pool.AvailableUIDsForRealm
// 挑一个号、自己调 upstream.FetchModels / FetchGlobalModelInfos、自己决定缓存
// 与失败处理。现在收进账号对象——目录来自哪个端点由**账号自己的 realm** 决定，
// 这本来就是账号属性，不该让调用方判断。
package codebuddy

import (
	"context"
	"net/http"
	"strconv"

	"github.com/linguo2625469/workbuddy2api-panel/internal/gateway"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// Catalog 拉取本账号所属域的模型目录。
//
// 两个域的目录来源完全不同（CN 走 /v3/config + 企业端点并集，global 走独立
// 目录端点），但**对调用方是同一个问题**：「这个账号能看见哪些模型」。
// 按 Realm() 分流在这里完成，调用方不需要知道有几个域。
//
// global 域的一个额外事实：FetchGlobalModelInfos 在上游未启用 global 路由或
// 探测失败时返回 nil（而不是 error）。nil 表示「这个号此刻给不出目录」，
// 池会去试别的号；如果全部都是 nil，池判定该上游本轮不可用。
func (x Account) Catalog(ctx context.Context) ([]gateway.CatalogModel, error) {
	if x.client == nil || x.A == nil {
		return nil, gateway.ErrUnsupported
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	// 目录接口自己的超时与重试在 upstream 内部，这里不重复加 deadline：
	// 调用方传进来的 ctx 只用来传递取消（面板关页面、请求断开）。
	if x.A.Realm() == "global" {
		infos := x.client.FetchGlobalModelInfos(x.A)
		if len(infos) == 0 {
			return nil, nil
		}
		efforts, defaults := x.client.GlobalEffortSnapshot()
		return catalogFromModelInfos("global", infos, efforts, defaults, x.client.HTTP), nil
	}
	infos, err := x.client.FetchModels(x.A)
	if err != nil {
		return nil, err
	}
	out := make([]gateway.CatalogModel, 0, len(infos))
	for _, mi := range infos {
		out = append(out, catalogModelOf("cn", mi, mi.Efforts, mi.DefaultEffort, x.client.HTTP))
	}
	return out, nil
}

// RateFor 取某个模型的生效积分倍率（对话收尾时估算扣费用）。
//
// 读的是 upstream 在目录刷新时写下的倍率快照（ModelRate），**不会**为了一个
// 倍率再发一次上游请求。快照里没有该模型就返回 false——调用方据此标成
// 「未知」，绝不写成 0（那会被后续逻辑当成免费）。
func (x Account) RateFor(ctx context.Context, model string) (float64, bool) {
	if x.client == nil || x.A == nil || model == "" {
		return 0, false
	}
	raw := x.client.ModelRate(x.A.Realm(), model)
	if raw == "" {
		return 0, false
	}
	rate, err := strconv.ParseFloat(raw, 64)
	if err != nil || rate <= 0 {
		// 0 与不可解析都不是「可用的倍率」：0 在这里等于「免费」，而目录里
		// 没有倍率字段的模型也会走到这里，混淆会让估算凭空变成 0。
		return 0, false
	}
	return rate, true
}

// catalogFromModelInfos 把 global 域的一批 ModelInfo 转成统一条目。
func catalogFromModelInfos(realm string, infos []upstream.ModelInfo, efforts map[string][]string,
	defaults map[string]string, httpc *http.Client) []gateway.CatalogModel {
	out := make([]gateway.CatalogModel, 0, len(infos))
	for _, mi := range infos {
		out = append(out, catalogModelOf(realm, mi, efforts[mi.ID], defaults[mi.ID], httpc))
	}
	return out
}

// catalogModelOf 单个 ModelInfo → gateway.CatalogModel。
//
// 上下文与输出上限走 upstream 的四级查找链（上游动态值 → 静态种子 → model.json
// → models.dev 按需拉取）。这条链**必须**留在 upstream 包内：它依赖 model.json
// 缓存与静态种子表，gateway 不能 import 它们（分层约束）。
func catalogModelOf(realm string, mi upstream.ModelInfo, remoteEfforts []string,
	remoteDefault string, httpc *http.Client) gateway.CatalogModel {
	efforts, def := upstream.EffortListing(realm, mi.ID, remoteEfforts, remoteDefault)
	m := gateway.CatalogModel{
		ID:                 mi.ID,
		Name:               mi.Name,
		ContextLength:      upstream.ContextWindowListingV4(mi.ID, mi.ContextWindow, httpc),
		Efforts:            efforts,
		DefaultEffort:      def,
		SupportsReasoning:  mi.SupportsReasoning,
		CanDisableThinking: mi.CanDisableThinking,
		SupportsImages:     mi.SupportsImages,
		SupportsToolCall:   mi.SupportsToolCall,
		IsDefault:          mi.IsDefault,
		Credits:            mi.Credits,
		Description:        mi.Description,
		Vendor:             mi.Vendor,
		Tags:               mi.Tags,
		OnlyReasoning:      mi.OnlyReasoning,
		ReasoningEffort:    mi.ReasoningEffort,
		ReasoningSummary:   mi.ReasoningSummary,
		MaxAllowedSize:     mi.MaxAllowedSize,
		PromoFactor:        mi.PromoFactor,
		PromoCredits:       mi.PromoCredits,
		PromoLabel:         mi.PromoLabel,
		PromoNote:          mi.PromoNote,
	}
	if mo, ok := upstream.MaxOutputTokensListingV4(mi.ID, mi.MaxTokens, httpc); ok {
		m.MaxOutputTokens = mo
	}
	return m
}
