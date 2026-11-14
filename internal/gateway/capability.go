// capability.go 账号的**可选特色能力**抽象。
//
// 与 Account 的三项必备能力（Credits / Checkin / Keepalive）分两层：
//
//   - 必备能力：所有上游都该有，不支持时返回 ErrUnsupported（企业版等情形）；
//     池的 CheckinAll / RefreshCredits / KeepaliveAll 对全部账号逐个调用。
//   - 特色能力（本文件）：「旅行」「盲盒」「连登」这类**某个上游独有的玩法**。
//     账号实现了对应接口就自动参与池的批量调度，没实现就静默跳过——池不认识
//     「Trae」「CodeBuddy」这些具体上游，也不判断后端类型。
//
// 新增一个上游时只要它的账号对象实现了同名接口，池与调度器一行都不用改；
// 反过来，某个上游没有旅行，就永远不用担心它被卷进旅行流程。
//
// 返回 ErrUnsupported 表示「这个账号不适用」（企业版 / global / 无凭证），
// 池按跳过处理：不是失败、不记错误、不进冷却。
package gateway

import (
	"context"
	"time"
)

// TravelOutcome 一次旅行巡检的结果。
type TravelOutcome struct {
	// Action 本次实际做了什么："adopted"（领养）/ "departed"（派出）/
	// "claimed"（领奖）/ "skipped"（无需动作）/ "none"（未执行）。
	Action string
	// State 巡检后的旅行状态（idle / traveling / arrived / unknown）。
	State string
	// Reward 本次领到的积分（未领奖为 0）。
	Reward int64
	// Message 可读说明（面板/日志直接展示）。
	Message string
	// RetryTomorrow 本次失败属于「今日不必再试」（例如领养门槛未达）。
	//
	// 池据此在当日剩余时间里跳过该账号：同类重试只会得到同一个业务错误，
	// 而每次重试都要打一次上游。跨自然日自动失效。
	RetryTomorrow bool
}

// TravelSnapshot 只读的行程快照（到站定时器用，不产生任何写操作）。
type TravelSnapshot struct {
	// State idle / traveling / arrived。
	State string
	// RecordID 在途/到站记录 id（领奖必带）。
	RecordID int64
	// ArriveAt 本次行程到站时刻（Unix 秒）。
	ArriveAt int64
	// ServerNow 上游当前时刻（Unix 秒），用于换算剩余时长、免疫本机时钟偏差。
	ServerNow int64
	// DailyLimitReached 今日已派出过。
	DailyLimitReached bool
}

// TravelOptions 一次旅行巡检的调用参数。
type TravelOptions struct {
	// SkipAdopt 本次跳过领养尝试（池的当日台账判定「今天已试过、门槛未达」）。
	//
	// 只跳过领养，**仍然查询有无猫与行程状态**——猫可能已经被别处领养，
	// 或者行程到站需要领奖，这些动作与领养门槛无关。
	SkipAdopt bool
	// BlindboxEnabled 空闲阶段是否顺带开能量盲盒。
	//
	// 开关与次数上限都是**调度策略**（用户在面板配置），不属于账号能力，
	// 因此由调用方经这里传入，而不是让账号自己去读配置。
	BlindboxEnabled bool
	// BlindboxMaxPerRun 单趟开盒次数上限（<=0 用实现方默认值）。
	BlindboxMaxPerRun int
}

// Traveler 账号支持「宠物旅行」玩法（领养 / 派出 / 到站领奖）。
//
// 实现方负责全部门控（企业版无成长体系、global 无 CN 任务体系、无 refreshToken），
// 不适用时返回 ErrUnsupported。池只负责遍历与限速。
type Traveler interface {
	// Travel 对账号推进一趟巡检（领养 / 领奖 / 盲盒 / 派出至多各一次）。
	Travel(ctx context.Context, opts TravelOptions) (TravelOutcome, error)
	// TravelState 只读查询当前行程，不产生任何写操作。
	//
	// 到站定时器靠它算下一次唤醒时刻：上游的 arrive_at 是精确到秒的，
	// 按它定闹钟比「每 N 分钟轮询一遍」省下 99% 的空转请求。
	TravelState(ctx context.Context) (TravelSnapshot, error)
}

// BlindboxOutcome 一次盲盒处理的结果。
//
// JSON tag 是**前端契约**（`panel/js/accounts.js` 直接读 `travel_state` /
// `will_open` / `energy` 等）。这些字段经 `pool.BlindboxResult` 嵌入后摊平
// 序列化，缺 tag 会让前端读到 undefined、整列显示空白。
type BlindboxOutcome struct {
	// TravelState 当前旅行状态（盲盒只能在 idle 时开）。
	TravelState string `json:"travel_state"`
	// Energy 能量余额。
	Energy int64 `json:"energy"`
	// Affordable 上游算出的可开次数。
	Affordable int `json:"affordable"`
	// CostPerOpen 单次消耗能量。
	CostPerOpen int64 `json:"cost_per_open"`
	// MaxOpen 上游允许的单趟上限。
	MaxOpen int `json:"max_open_count"`
	// WillOpen 本次计划开几次（0 = 不开）。
	WillOpen int `json:"will_open"`
	// Decision 给用户看的一句话理由（面板直接渲染）。
	Decision string `json:"decision"`
	// Opened 是否真的开了（dry-run 时为 false）。
	Opened bool `json:"opened"`
	// Prize 奖品名（可能为空）。
	Prize string `json:"prize,omitempty"`
}

// Blindboxer 账号支持「能量盲盒」玩法。
//
// maxPerRun 是本次单账号的开盒次数上限（<=0 表示用实现方默认值）。
// apply=false 为 dry-run：只查状态与配额并给出决策，不消耗能量。
type Blindboxer interface {
	Blindbox(ctx context.Context, apply bool, maxPerRun int) (BlindboxOutcome, error)
}

// StreakOutcome 一次连登管家执行的结果。
type StreakOutcome struct {
	// Days 当前连续登录天数。
	Days int
	// RedeemedTiers 本次兑换成功的档位（"7d"/"14d"/"28d"）。
	RedeemedTiers []string
	// LotteryDraws 本次抽奖次数。
	LotteryDraws int
	// Reward 本次累计获得的积分。
	Reward int64
	// Message 可读说明。
	Message string
}

// StreakRunner 账号支持「连登管家」玩法（补签 → 礼包/补偿 → 兑换档位 → 抽奖）。
//
// 幂等：已兑换档位、无抽奖次数时自动跳过。
type StreakRunner interface {
	RunStreak(ctx context.Context) (StreakOutcome, error)
}

// BlackcatOutcome 一次夜猫子任务执行的结果。
type BlackcatOutcome struct {
	// Need 还需补足的对话次数（0 = 无需执行）。
	Need int64
	// Completed 实际完成的对话次数。
	Completed int64
	// Message 可读说明。
	Message string
}

// BlackcatRunner 账号支持「夜猫子」玩法（夜间补足对话并上报事件）。
//
// 这是特色能力里唯一**会发真实模型对话**的（不是纯 RPC），实现方需自行判断
// 是否处于夜间计数窗口；窗口外返回 Need=0 的结果即可。
type BlackcatRunner interface {
	RunBlackcat(ctx context.Context) (BlackcatOutcome, error)
}

// CompensationOutcome 补偿活动的状态 / 领取结果。
type CompensationOutcome struct {
	// Active 当前是否有生效中的补偿活动。
	Active bool
	// Claimed 本次活动是否已领取。
	Claimed bool
	// CreditNum 待领额度。
	CreditNum int64
	// EndTime 领取截止时间（上游原文，无时区）。
	EndTime string
	// ClaimedNow 本次调用是否真的领到了（仅 Claim 有意义）。
	ClaimedNow bool
	// Credit 本次领到的额度（仅 Claim 有意义）。
	Credit int64
	// Skip 未执行的原因标识（"inactive" / "claimed" / "zero_credit"，空 = 已执行）。
	Skip string
	// Message 可读说明。
	Message string
}

// Compensator 账号支持「补偿活动」玩法（查状态 / 待领才领）。
type Compensator interface {
	// CompensationStatus 只读查询补偿状态，不领取。
	CompensationStatus(ctx context.Context) (CompensationOutcome, error)
	// CompensationClaim 查状态后「待领才领」，幂等。
	CompensationClaim(ctx context.Context) (CompensationOutcome, error)
}

// ActivityOutcome 一次活跃上报的结果。
type ActivityOutcome struct {
	// StreakDays 上报后回读的连登天数（用于发现「上报 200 但静默丢弃」）。
	StreakDays int
	// Message 可读说明。
	Message string
}

// ActivityReporter 账号支持「活跃上报」玩法（点亮连登天数 / 解锁领养）。
type ActivityReporter interface {
	ReportActivity(ctx context.Context) (ActivityOutcome, error)
}

// Freshener 账号能在发起请求前自检并续期凭证。
//
// 为什么是可选能力而不是渠道循环里的固定步骤：不同上游的续期时机不同。
// CodeBuddy 的 accessToken 会临近过期，需要提前刷新并落盘；Trae 的 token
// 由 ChatStream 自己保证新鲜，不需要预刷新。
//
// 未实现 = 不需要预刷新。
type Freshener interface {
	// NeedsRefresh 报告是否已进入需要刷新凭证的时间窗。
	NeedsRefresh(within time.Duration) bool
	// Freshen 刷新凭证并落盘。失败时返回分类后的错误，由调用方决定换号与否。
	Freshen(ctx context.Context) error
}
