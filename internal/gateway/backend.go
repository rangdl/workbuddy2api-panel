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

	// Accounts 从各自来源装载账号。
	//
	// 每次调用都应反映磁盘最新状态（CodeBuddy 扫描 auths/ 目录，Trae 读
	// trae-*.json）。池的 Sync 会定期调用它。
	Accounts() ([]Account, error)

	// Supports 该账号能否服务这个模型（已剥离前缀的 Bare 名）。
	//
	// 契约：纯查表，**不得发网络请求**——它在选号热路径上被逐账号调用。
	// 返回 false 的账号不进入候选集。
	Supports(acc Account, bareModel string) bool

	// WireModel 客户端模型名 → 该上游的 wire 名（日志/账单对照用）。
	// 无映射时返回空串，调用方回落到 bareModel。
	WireModel(acc Account, bareModel string) string

	// Chat 发一次请求。
	//
	// 失败时返回 *Failure（已带 Decision），调用方不再二次分类。
	// 成功时返回的 Outbound.Stream 由调用方负责 Close。
	//
	// 实现约定：**不得改写 in.Raw**。同一个 Inbound 可能被多个 backend
	// 依次尝试（跨上游回退），任何原地修改都会污染后续尝试。
	Chat(ctx context.Context, acc Account, in *Inbound) (*Outbound, error)

	// Credits 查询余额（调度器余额刷新与面板共用）。
	// 不支持时返回 ErrCreditsUnsupported。
	Credits(ctx context.Context, acc Account) (*Credits, error)

	// RetryHook 链路特有的「同账号内重试」逻辑（可选，nil = 无）。
	//
	// CodeBuddy 用它实现内容拦截时的降级提示词重试；Trae 无此概念。
	RetryHook() RetryHook
}

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
	// FinishReason 结束原因。
	FinishReason string
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
