// catalog.go 账号的**模型目录**能力抽象。
//
// 为什么目录要收进这里：模型列表天然是「上游说了算」的数据——哪个模型存在、
// 多长上下文、几档思考、什么价，只有上游知道。改造前这些查询散在 server 与
// panel 两处，各自 trae.Load()、各自挑账号、各自缓存，于是有了三份互不相干的
// 目录（CN 动态缓存 10 分钟 / global 桶 1 小时 / Trae remote 30 分钟），
// 面板显示的列表与 /v1/models 返回的列表机制上就允许不一致。
//
// 收进可选能力后，形态与 Traveler / Blindboxer 完全一致：账号实现了 Cataloger
// 就自动参与池的批量查询，没实现就静默跳过。池不认识「CodeBuddy」「Trae」，
// 只认接口；server 与 panel 只做「调池 → 加前缀 → 输出」，不再直连任何上游。
//
// 两条与特色能力的关键差异：
//
//  1. **目录不是每个账号等价的**。旅行/签到必须逐号执行（每个号有自己的状态），
//     而模型目录对一个上游只有一份——同一个上游的任意一个健康号都能拉。
//     所以池的目录调度是「每个上游挑一个号」而不是「遍历所有号」。
//
//  2. **失败不罚号**。目录接口偶发 5xx 不代表该号的对话通道不可用，这是
//     handler 里 fetchDynamicModels 早就写明的判断（models 拉取失败 ≠ chat
//     不可用）。所以目录失败只进池的负缓存，不 NoteError、不进冷却。
package gateway

import "context"

// CatalogModel 目录里的一条模型（跨上游统一形态）。
//
// 刻意**不**包含上游原始的整包字段：CodeBuddy 的 promo_*、Trae 的
// config_source 之类各上游独有，那些由各 backend 在自己的展示层处理。
// 这里只放「所有上游都该回答」的部分，池与消费方据此就能渲染。
type CatalogModel struct {
	// ID 上游 wire 名（裸名，不含任何前缀）。
	ID string
	// Name 展示名（缺失 = 空，消费方回落 ID）。
	Name string
	// ContextLength 生效的上下文窗口（0 = 未知）。
	//
	// 这是**最终可用的上限**：CodeBuddy 走四级查找链、Trae 在有 max_mode 时
	// 取扩展档。消费方不需要知道这个数是怎么算出来的。
	ContextLength int64
	// ContextLengthBase 基础窗口（0 = 无扩展档）。
	//
	// 只有 Trae 这类「基础档 + 扩展档」的上游才非 0，用于面板在悬停里对照。
	ContextLengthBase int64
	// MaxOutputTokens 单次输出上限（0 = 未知）。
	MaxOutputTokens int64
	// Efforts 支持的思考档位（网关口径 low/medium/high/xhigh/max）。
	// 空 = 未知或固定档，消费方不该据此断言「不支持思考」。
	Efforts []string
	// DefaultEffort 默认思考档位（空 = 未知）。
	DefaultEffort string
	// SupportsReasoning 是否支持思考（false = 上游没说或不支持）。
	SupportsReasoning bool
	// CanDisableThinking 思考是否可关（true = off 档可用）。
	CanDisableThinking bool
	// SupportsImages 是否支持图片输入。
	SupportsImages bool
	// SupportsToolCall 是否支持工具调用。
	SupportsToolCall bool
	// IsDefault 是否该上游的默认模型。
	IsDefault bool
	// Credits 积分倍率原文（"x0.15"），仅展示。
	//
	// 空 = **未知**，不是免费。上游缺失这个字段时不要当成 0（dsh 的教训：
	// 把未知当免费会让选号把收费模型当免费层）。
	Credits string
	// Description 描述（可空）。
	Description string
	// Vendor 厂商（可空）。
	Vendor string
	// Tags 标签（可空）。
	Tags []string
	// OnlyReasoning 是否纯推理模型（true = 不可关闭思考、只能思考着答）。
	OnlyReasoning bool
	// ReasoningEffort 上游的推理模式字段原文（与 Efforts 数组不同源，仅展示）。
	ReasoningEffort string
	// ReasoningSummary 推理摘要模式（如 "auto"，仅展示）。
	ReasoningSummary string
	// MaxAllowedSize 上游下发的最大允许上下文（与 ContextLength 口径并列）。
	// 0 = 未下发。
	MaxAllowedSize int64

	// 限时优惠：Credits 是**牌价**，Promo* 是当前生效折扣。
	//
	// 面板据此显示「生效价 + 标签 + 划线牌价」。PromoFactor 为 nil 表示无
	// machine-readable 折扣（如「错峰使用」只有时段文案），此时只挂标签。
	PromoFactor  *float64
	PromoCredits string
	PromoLabel   string
	PromoNote    string
}

// Cataloger 账号能列出所属上游的模型目录。
//
// 约定：**同一上游内任意健康账号返回的目录应当相同**。实现方可以放心依赖
// 上游的进程内缓存，不必每次真发请求。
//
// 返回 ErrUnsupported 表示该账号无法提供目录（无凭证 / 入口未启用），池会
// 跳过它去试同一个上游的另一个号；全部不支持才判定该上游本轮不可用。
type Cataloger interface {
	// Catalog 拉取模型目录。
	//
	// 失败返回 error：池据此进负缓存，但**不惩罚账号**（见文件头说明 2）。
	Catalog(ctx context.Context) ([]CatalogModel, error)
}

// ModelRate 目录里某个模型的积分倍率（用于对话后的扣费估算）。
//
// 与 Catalog 分开是因为调用时机不同：Catalog 是页面级的批量查询，ModelRate
// 在每次对话收尾时按模型名单独问一次，靠上游缓存命中，不该为了一个倍率把
// 整个目录复制一遍。
type ModelRate interface {
	// RateFor 返回该模型的倍率与是否有值。
	//
	// 返回 false 表示未知（目录拉不到 / 该模型无倍率字段）——调用方不能把
	// 「未知」写成 0，那会被后续逻辑当成免费。
	RateFor(ctx context.Context, model string) (rate float64, ok bool)
}
