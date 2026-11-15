package gateway

import (
	"context"
	"io"
	"time"
)

// backend.go 上游能力抽象。

// Backend 一个上游的完整能力：账号装载 + 请求转换 + 发送 + 回程规范化。
//
// 实现方（backend/codebuddy、backend/trae）负责把各自私有的协议差异全部
// 收在包内：CodeBuddy 的 prepareBody/出站头/OpenAI SSE 透传，Trae 的
// llm_utils_chat 改写/自定义 SSE 重写。dispatch 层只面对本接口。
type Backend interface {
	// Name 稳定标识（"codebuddy" / "trae"），用于日志、持久化键、粘性绑定。
	Name() string

	// Aliases 模型名前缀可用的别名（不含 Name 本身），如 codebuddy 的 "wb"。
	Aliases() []string

	// Enabled 当前是否参与调度（运行期开关，可热更新）。
	//
	// 返回 false 的 backend 不参与选号，也不出现在模型列表里；已绑定的
	// 会话在下次 Resolve 时自然失效。
	Enabled() bool

	// Chat 发一次请求。
	//
	// 失败时返回 *Failure（已带 Decision），调用方不再二次分类。
	// 成功时返回的 Outbound.Stream 由调用方负责 Close。
	//
	// 实现约定：**不得改写 in.Raw**。同一个 Inbound 可能被多个 backend
	// 依次尝试（跨上游回退），任何原地修改都会污染后续尝试。
	Chat(ctx context.Context, acc Account, in *Inbound) (*Outbound, error)

	// RetryHook 链路特有的「同账号内重试」逻辑（可选，nil = 无）。
	//
	// CodeBuddy 用它实现内容拦截时的降级提示词重试；Trae 无此概念。
	RetryHook() RetryHook
}

// 本接口曾另有 Accounts / Supports / WireModel / Credits 四个方法，现已删除：
//   - 账号装载走 backend 各自的 Wrap（main 装配时调用），池只经 Sync/Add 接收；
//   - 模型可服务性靠上游报错后记负缓存（事后学习），不做事前查表；
//   - wire 名映射在 PrepareChatBody 内部完成；
//   - 余额查询统一走账号对象（gateway.Account.Credits）。
// 四个方法都零生产调用，留着只会让「新上游要实现什么」这件事看起来比实际复杂。

// RetryHook 同账号内的重试改写。
type RetryHook interface {
	// OnFailure 给定失败的 Inbound 与分类，返回改写后的 Inbound。
	// retry=false 表示不适用，调用方按 Decision 继续换号流程。
	OnFailure(in *Inbound, f *Failure) (next *Inbound, retry bool)
}

// Inbound 客户端原始请求。
//
// 关键设计：Raw 全程只读，一次构造、多次重放。这是「一条路径跨上游回退」
// 成立的技术前提——CodeBuddy 转换失败后，Trae 拿到的是同一份原始字节。
type Inbound struct {
	// Raw 原始请求体（JSON）。只读。
	Raw []byte
	// Ref 解析后的模型引用（含 backend/realm 锁定信息）。
	Ref ModelRef
	// Stream 客户端是否要求流式。
	Stream bool
	// ClientIP 调用方 IP（出站头透传用）。
	ClientIP string
	// SessionKey 粘性会话键（空 = 不做粘性绑定）。
	SessionKey string
	// ConversationID 会话标识（上游追踪头用）。
	ConversationID string
	// ChatID 本次响应的 id（"chatcmpl-..."），由调用方生成。
	//
	// 为什么放在 Inbound 而不是让 backend 自己生成：同一次客户端请求可能换号
	// 重试，各次尝试必须产出**同一个** id，否则客户端会把重试结果当成新消息。
	ChatID string

	// ── 以下字段供需要协议特化的 backend 使用（CodeBuddy 用，Trae 忽略）──

	// ConversationRequestID 轮级聚合键（出站 X-Conversation-Request-ID）。
	//
	// 上游按这个键做前缀缓存聚合：同轮的所有上游调用共享一个键，
	// 换 user 消息才换键。空 = 不带该头。
	ConversationRequestID string
	// TraceID 调用方透传的追踪 id（出站 X-Trace-ID）。
	TraceID string
	// DeclaredTools 本请求声明过的工具名白名单。
	//
	// 上游在没有 tools 时会把工具调用吐成原生标记文本，回程需要按名单判定
	// 是否还原成结构化 tool_calls。nil = 用弱判定。
	DeclaredTools map[string]bool
	// FrameHint 把流内 error 帧映射成 gateway_hint 文本（nil = 不加）。
	//
	// 文案策略属于网关配置（server 层知识），协议判定属于 backend——
	// 所以 backend 负责识别帧、由调用方提供文案。
	FrameHint func(kind, payload string) string
}

// Outbound 规范化的上游回程。
type Outbound struct {
	// Status HTTP 状态码。
	Status int
	// Stream 2xx 时的上游流（调用方 Close）。
	Stream io.ReadCloser
	// Body 非 2xx 时的错误体原文。
	Body []byte
	// Convert 把上游流规范化成 OpenAI SSE，逐帧交给 emit。
	//
	// 为什么挂在 Outbound 而不是让 dispatch 按 backend 分支：
	// 这是两条链路最大的形态差异——CodeBuddy 的流已是 OpenAI SSE（只需
	// 帧规范化/工具调用修复），Trae 是完全自定义的事件流（需整体重写）。
	// 统一成 emit 风格后，dispatch 只有一份转发循环。
	//
	// 返回的 StreamStats 携带错误与用量，供记账与「已下发内容则不可重试」判定。
	Convert func(emit func(frame []byte) error) (StreamStats, error)
}

// StreamStats 一次流式转换的统计。
type StreamStats struct {
	// SentAny 是否已向客户端下发过内容。
	//
	// 一旦为真就不能再改 HTTP 状态码，也不能换号重试——客户端已经看到
	// 部分内容，重试会导致重复输出。
	SentAny bool
	// ErrorCode / ErrorMsg 流内错误（0 = 无）。
	ErrorCode int64
	ErrorMsg  string
	// Decision 流内错误的处置决策（nil = 无错误）。
	//
	// **backend 必须填**：流内错误的分类只有上游自己知道（CodeBuddy 是
	// error 帧原文里的 6004/11102，Trae 是事件流里的业务码 1005/4017）。
	// 此前这个字段不存在，server 只能按 `acc.Backend() == "trae"` 分流到
	// 两套分类器——加第三个上游就要加第三个分支。
	//
	// 与 HTTP 层失败用同一个 Decision 意味着：处置语义（Action/Rotate/Policy）
	// 在两条路径上完全一致，server 侧只有一份 applyFailure。
	//
	// 为什么单独给字段而不是复用 ErrorCode：ErrorCode 是上游原文（Trae 的
	// 1005、CodeBuddy 的 -1 哨兵），不同上游的码表会撞名；Decision 已经过
	// backend 归一，是跨上游可直接消费的形态。
	Decision *Decision
	// FinishReason 结束原因。
	FinishReason string
	// Repaired 本次流中还原成功的工具调用数（原生标记文本 → tool_calls）。
	Repaired int
	// RepairSeen 识别到工具调用标记的块数（含还原失败的）。
	RepairSeen int
	// Usage token 用量（可能为 nil）。
	Usage *Usage
	// Remain 上游流内上报的剩余额度（Trae 的 notify_usage 独有）。
	// 不支持的 backend 返回 nil——dispatch 不需要判断 backend 类型。
	Remain *Remain
}

// Usage token 用量。
type Usage struct {
	PromptTokens     int64
	CompletionTokens int64
	TotalTokens      int64
	// CacheRead 命中缓存的输入 token（计费远低于未命中）。
	CacheRead int64
	// CacheCreation 写入缓存的输入 token。
	CacheCreation int64
	// Reasoning 思考 token（输出的一部分）。
	Reasoning int64
	// Credit 上游在 usage 里直接给出的本次扣费（CodeBuddy 的 usage.credit）。
	// 0 且 HasCredit=false 表示上游没给；有值时是**实测**，优先于任何估算。
	Credit float64
	// HasCredit Credit 是否有效。
	HasCredit bool
}

// CachedPercent 缓存命中率（0~100；无输入 token 时返回 0）。
//
// 口径：cached / (cached + 未命中部分)。上游的 prompt_tokens 已含缓存部分，
// 所以未命中 = prompt - cached。
func (u *Usage) CachedPercent() int {
	if u == nil || u.PromptTokens <= 0 || u.CacheRead <= 0 {
		return 0
	}
	pct := int(float64(u.CacheRead) / float64(u.PromptTokens) * 100)
	if pct > 100 {
		pct = 100
	}
	return pct
}

// Remain 上游上报的剩余额度。
type Remain struct {
	// Available 可用于对话的额度（Trae: IDE 积分，不含 Work）。
	Available float64
	// Work 仅供展示（Trae 的 Work 积分）；0 = 无此概念。
	Work float64
	// Consumed 本次计费消耗（0 = 上游未给出）。
	//
	// Trae 的 notify_usage 目前只确认了剩余额度字段；是否有消耗字段取决于
	// 上游。backend 若识别出消耗就填这里，dispatch 据此记 credit。
	Consumed float64
	// HasConsumed Consumed 是否有效。
	HasConsumed bool
	// Raw 原始 payload（可观测性：排查扣费口径时打日志）。
	Raw map[string]any
}

// UsageLabeller 可选能力：账号在用量账本里应该记到哪个「域」。
//
// 为什么需要：用量视图按域分组（cn / global），而某些上游的用量在业务上
// 需要独立分账——Trae 的积分消耗与 CodeBuddy 的套餐额度是两套体系，混在
// 一起会让「这个域花了多少」失去意义。默认记 Realm()，实现了本接口的账号
// 可以改成自己的标签。
//
// 未实现 = 用 Realm()。这与 Freshener 同一模式：能力可选，池与 server
// 都不判断上游名。
type UsageLabeller interface {
	// UsageLabel 用量账本里的分组标签（空 = 用 Realm()）。
	UsageLabel() string
}

// RemainObserver 可选能力：账号想接收上游流内上报的剩余额度。
//
// Trae 的 notify_usage 会带 IDE/Work 两份积分，面板的「Trae 签到」区块要
// 分别展示。让账号自己接收，server 就不需要 `acc.Backend() == "trae"` 分支，
// 也不必持有某个上游的统计实例。
//
// 未实现 = 忽略（池仍会用 Available 更新选号口径的 credits）。
type RemainObserver interface {
	ObserveRemain(r *Remain)
}

// StreamLifecycle 可选能力：账号想在每次请求前后记「在途 / 成功 / 失败」。
//
// Trae 面板要展示这三个数。实现方在 BeginStream 里把自己的计数 +1，
// 返回的 finish 由 server 在请求结束时调用（成功与否都要调，否则在途
// 计数永远不归零）。
//
// 未实现 = 不统计。
type StreamLifecycle interface {
	BeginStream() (finish func(ok bool))
}

// CreditEstimator 可选能力：上游没在 usage 里给实测扣费时，账号自己按
// token 与倍率估算本次消耗。
//
// 为什么是能力而不是 server 里的公式：估算口径是**上游特有的**。
// Trae 的 chat 流不含扣费字段（实测），只能按 token × 模型倍率推算，
// 且缓存读/写的权重各不相同（见 internal/trae/credit_estimate.go）；
// CodeBuddy 在 usage 里直接给 credit，根本不需要估算。
//
// 未实现 = 不计估算（面板显示 "-"，而不是编一个数）。
// 返回值 ok=false 表示无法估算（缺 token / 倍率未知）。
type CreditEstimator interface {
	EstimateCredits(u *Usage, rate float64) (credit float64, ok bool)
}

// Credits 归一后的余额视图。
//
// 字段形状对齐 pool.Status 现有的那几个——这个抽象是从通用需求长出来的，
// 不是为合池硬造的。
type Credits struct {
	// Available 可用于**对话**的积分。
	//
	// 这是各实现最容易出错的地方：
	//   CodeBuddy: 套餐剩余（企业版 Unlimited=true）
	//   Trae:      必须取 CreditsSummary.General（product_id != 209），
	//              **不能**用 Total——Total 含 Work 积分，而对话只消耗
	//              IDE 额度，用 Total 会让快耗尽的号看起来健康。
	Available float64
	// Limit 总额度（0 = 未知）。
	Limit float64
	// Unlimited 不限量（CodeBuddy 企业版）。
	Unlimited bool
	// Expiring 配置窗口内即将过期的额度。
	Expiring float64
	// Earliest 最早一批额度的到期时刻（零值 = 未知/无到期概念）。
	Earliest time.Time
	// Buckets 按自然日聚合的到期批次。
	Buckets []ExpiryBucket
}

// ExpiryBucket 单个自然日到期的额度合计。
type ExpiryBucket struct {
	Date     string
	ExpireAt time.Time
	Remain   int64
}

// CheckinResult 一次签到的结果（与具体上游无关的归一口径）。
type CheckinResult struct {
	// Already 今日已签到（含上游的幂等拒绝）。
	Already bool
	// Reward 本次奖励积分（上游未给时为 0）。
	Reward int64
	// Message 上游原文或可读说明。
	Message string
	// Skipped 本次未执行（例如企业版账号无签到体系）。
	//
	// 与 ErrUnsupported 的区别：Skipped 是「这个账号正常，只是这类账号不参与」，
	// 池按正常流程记账；ErrUnsupported 是「这个上游没实现」，池直接跳过。
	Skipped bool
	// DeviceAlready 该**设备**今日已签（Trae 的 9095/9004）。
	//
	// 限额按设备算：多账号共用同一设备号时只有第一个能签成。**本账号未必签过**，
	// 因此池不能把它标成「今日已签」，否则按钮会谎报、次日也不会重试。
	DeviceAlready bool
	// Unverified claim 返回成功但回查未确认（可能是「假成功」）。
	//
	// 与 DeviceAlready 同理：不标记已签，留待下一轮重试。
	Unverified bool
}
