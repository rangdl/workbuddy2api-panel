package gateway

import (
	"errors"
	"time"
)

// decision.go 错误处置归一。
//
// 两套上游的错误码表完全不同（CodeBuddy 用 6004/11102/12153，Trae 用
// 1005/20405/4017），但**对账号要做什么**是同一套动作。所以这里归一的不是
// 错误名称，而是处置动作——各 backend 只需实现一张映射表。
//
// 为什么 Action 与 Rotate 分开：两者语义独立。「不罚账号」不等于「该换号」，
// 反过来也一样——请求本身有问题时不罚号，换号也只会把同一个错误烧两遍。

// Action 对账号的处置。
type Action int

const (
	// ActionNone 不罚账号（请求级错误 / 内容拦截）。
	ActionNone Action = iota
	// ActionCoolShort 短冷却（限流、偶发 404）。
	ActionCoolShort
	// ActionCoolMedium 中冷却（上游 5xx）。
	ActionCoolMedium
	// ActionCoolLong 长冷却（额度耗尽）。
	ActionCoolLong
	// ActionDisable 禁用（凭证/设备失效，冷却不会自愈，需人工介入）。
	ActionDisable
	// ActionModelCooldown 仅该 (账号, 模型) 避让，账号对其余模型仍健康。
	ActionModelCooldown
	// ActionDegrade 连败降权（临时出池，指数退避自愈）。
	ActionDegrade
)

func (a Action) String() string {
	switch a {
	case ActionNone:
		return "none"
	case ActionCoolShort:
		return "cool_short"
	case ActionCoolMedium:
		return "cool_medium"
	case ActionCoolLong:
		return "cool_long"
	case ActionDisable:
		return "disable"
	case ActionModelCooldown:
		return "model_cooldown"
	case ActionDegrade:
		return "degrade"
	default:
		return "unknown"
	}
}

// Decision 一次失败的完整处置决策。
type Decision struct {
	// Action 对账号做什么。
	Action Action
	// Rotate 是否值得换号重试（可能换到另一个 backend）。
	//
	// 刻意与 Action 分离：
	//   - 模型不可用：Action=ActionModelCooldown（只避让该模型）+ Rotate=false
	//     （换号还是同一个模型，纯烧额度）；
	//   - 请求体畸形：Action=ActionNone + Rotate=false（换谁都一样错）；
	//   - 额度耗尽：Action=ActionCoolLong + Rotate=true（换个号能过）。
	Rotate bool
	// Cooldown 覆盖默认冷却时长（0 = 用 Action 的缺省值）。
	// 上游明示 Retry-After 时由 backend 填这里。
	Cooldown time.Duration
	// Kind backend 的原始分类名（"plan_limit" / "session_dead"），
	// 仅用于日志与面板展示——调度逻辑不读它。
	Kind string
	// Code 上游原始业务码（"1005" / "11102"）。
	Code string
	// Message 可读原因（优先上游原文）。
	Message string
}

// Failure 已分类的上游失败（Backend.Chat 的错误返回值）。
type Failure struct {
	Decision
	// HTTPStatus 上游返回的 HTTP 状态（0 = 非 HTTP 层失败，如流内错误）。
	HTTPStatus int
	// Body 错误体原文。
	//
	// error-passthrough 语义：客户端必须看到真实错误才能排查，网关不改写成
	// 固定文案。为空时调用方用 Message 兜底。
	Body []byte
}

func (f *Failure) Error() string {
	if f.Message != "" {
		return f.Message
	}
	if f.Kind != "" {
		return "upstream failure (" + f.Kind + ")"
	}
	return "upstream failure"
}

// AsFailure 提取 *Failure（非该类型返回 nil）。
func AsFailure(err error) *Failure {
	var f *Failure
	if errors.As(err, &f) {
		return f
	}
	return nil
}

// 无可用账号的原因（Backend 之外的控制流错误）。
var (
	// ErrNoAccount 候选集为空（全部禁用/冷却/不支持该模型）。
	ErrNoAccount = errors.New("gateway: no usable account")
	// ErrAttemptsExhausted 换号次数用尽。
	ErrAttemptsExhausted = errors.New("gateway: retry attempts exhausted")
	// ErrCreditsUnsupported 该 backend 不支持余额查询。
	ErrCreditsUnsupported = errors.New("gateway: credits query unsupported")
)
