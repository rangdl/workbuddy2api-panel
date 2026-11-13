package trae

import (
	"sort"
	"sync"
	"time"
)

// stats.go 账号运行期计数（进程内，重启清零）。
//
// 为什么需要：面板的 Trae 账号区此前只有「凭证有没有配」这类静态信息，而
// CodeBuddy 账号池那一栏有成功/失败、在途、用量、最近成功——运维看两个上游时
// 心智模型不一致。Trae 账号不进 pool（两套账号体系互不相干），所以这里给一份
// 同口径的轻量计数。
//
// 与 Cooldowns 同样的取舍：内存态、不落盘。Trae 账号数量级小，重启丢失的代价
// 只是"面板上计数归零"，不值得为此引入状态文件与它带来的并发/迁移负担。

// Stats 账号运行期计数表（并发安全）。
type Stats struct {
	mu sync.Mutex
	m  map[string]*statEntry
}

type statEntry struct {
	inFlight int64

	success     int64
	errs        int64
	lastSuccess time.Time
	lastErr     time.Time

	// 最近一次尝试的耗时（流式为 TTFB，非流式为总耗时）。
	lastLatency time.Duration

	// token 用量累计（与 pool.TokenUsage 同口径）。
	reqCount         int64
	usageCount       int64
	promptTokens     int64
	completionTokens int64
	totalTokens      int64
	lastModel        string
	lastTPS          float64
	hasTPS           bool

	// 最近一次已知的剩余额度。两个来源：
	//   - notify_usage（对话流内上报，免费、实时，但只有 IDE/Work 分解）；
	//   - 面板手动「积分」查询（权威，含额度上限，但要打一次上游）。
	// 后写入者覆盖前者，creditsAt 记录时刻供前端标注新鲜度。
	creditsIDE   float64
	creditsWork  float64
	creditsTotal float64
	creditsLimit float64
	creditsAt    time.Time
	hasCredits   bool
}

// StatView 单账号的只读快照（面板展示用；不含任何凭证）。
type StatView struct {
	UID string `json:"uid"`
	// InFlight 当前在途请求数。
	InFlight int64 `json:"in_flight"`
	// SuccessCount / ErrTotal 累计成功 / 失败（终身累计，不清零）。
	SuccessCount int64 `json:"success_count"`
	ErrTotal     int64 `json:"err_total"`
	// LastSuccess / LastErr 最近成功 / 失败时刻（零值 JSON 序列化为 null）。
	LastSuccess *time.Time `json:"last_success,omitempty"`
	LastErr     *time.Time `json:"last_err,omitempty"`
	// LastLatencyMs 最近一次耗时（流式为 TTFB）。
	LastLatencyMs int64 `json:"last_latency_ms,omitempty"`
	// TokenUsage 与 pool.Status.TokenUsage 同构，前端可复用同一套渲染。
	TokenUsage TokenUsageView `json:"token_usage"`
	// 积分（Credits 剩余 / CreditsLimit 额度上限；0 = 未知）。
	Credits      float64 `json:"credits,omitempty"`
	CreditsIDE   float64 `json:"credits_ide,omitempty"`
	CreditsWork  float64 `json:"credits_work,omitempty"`
	CreditsLimit float64 `json:"credits_limit,omitempty"`
	// CreditsAt 积分数据的观测时刻（前端标注「x 分钟前」）。
	CreditsAt *time.Time `json:"credits_at,omitempty"`
	// HasCredits 是否有过至少一次积分观测（false = Credits 字段无意义）。
	HasCredits bool `json:"has_credits,omitempty"`
}

// TokenUsageView 与 pool.TokenUsage 字段同名，前端 formatTokenCount 等可直接复用。
type TokenUsageView struct {
	RequestCount        int64      `json:"request_count,omitempty"`
	UsageCount          int64      `json:"usage_count,omitempty"`
	PromptTokens        int64      `json:"prompt_tokens,omitempty"`
	CompletionTokens    int64      `json:"completion_tokens,omitempty"`
	TotalTokens         int64      `json:"total_tokens,omitempty"`
	LastLatencyMs       int64      `json:"last_latency_ms,omitempty"`
	LastTokensPerSecond *float64   `json:"last_tokens_per_second,omitempty"`
	LastUsedAt          *time.Time `json:"last_used_at,omitempty"`
	LastModel           string     `json:"last_model,omitempty"`
}

// UsageDelta 一次请求的用量增量。Has* 区分「上游没给」与「值就是 0」。
type UsageDelta struct {
	Model               string
	HasPromptTokens     bool
	PromptTokens        int64
	HasCompletionTokens bool
	CompletionTokens    int64
	HasTotalTokens      bool
	TotalTokens         int64
	HasLatencyMs        bool
	LatencyMs           int64
	HasTokensPerSecond  bool
	TokensPerSecond     float64
}

// NewStats 构造空计数表。
func NewStats() *Stats {
	return &Stats{m: make(map[string]*statEntry)}
}

// entry 取（或惰性创建）账号条目。调用方必须已持有 mu。
func (s *Stats) entry(uid string) *statEntry {
	e, ok := s.m[uid]
	if !ok {
		e = &statEntry{}
		s.m[uid] = e
	}
	return e
}

// Begin 标记一次请求开始（在途 +1）。
//
// 返回的 finish 必须在请求结束时调用一次；ok=false 计入失败。
// 这样即使中途 return / panic 恢复，在途计数也能归零。
func (s *Stats) Begin(uid string) (finish func(ok bool, d UsageDelta)) {
	if s == nil || uid == "" {
		return func(bool, UsageDelta) {}
	}
	s.mu.Lock()
	e := s.entry(uid)
	e.inFlight++
	s.mu.Unlock()

	var once sync.Once
	return func(ok bool, d UsageDelta) {
		once.Do(func() {
			s.mu.Lock()
			defer s.mu.Unlock()
			e := s.entry(uid)
			if e.inFlight > 0 {
				e.inFlight--
			}
			now := time.Now()
			if ok {
				e.success++
				e.lastSuccess = now
			} else {
				e.errs++
				e.lastErr = now
			}
			e.reqCount++
			if d.Model != "" {
				e.lastModel = d.Model
			}
			known := false
			if d.HasPromptTokens && d.PromptTokens >= 0 {
				e.promptTokens += d.PromptTokens
				known = true
			}
			if d.HasCompletionTokens && d.CompletionTokens >= 0 {
				e.completionTokens += d.CompletionTokens
				known = true
			}
			if d.HasTotalTokens && d.TotalTokens >= 0 {
				e.totalTokens += d.TotalTokens
				known = true
			}
			if known {
				e.usageCount++
			}
			if d.HasLatencyMs && d.LatencyMs > 0 {
				e.lastLatency = time.Duration(d.LatencyMs) * time.Millisecond
			}
			// 失败或没有 completion_tokens 时不保留上一次的吞吐，避免误读。
			if ok && d.HasTokensPerSecond && d.TokensPerSecond > 0 {
				e.lastTPS, e.hasTPS = d.TokensPerSecond, true
			} else {
				e.hasTPS = false
			}
		})
	}
}

// NoteRemain 记录 notify_usage 上报的剩余额度（对话流内免费获得）。
//
// 只更新 IDE/Work 分解，不动额度上限——上限只有积分查询接口才知道，
// 用 0 覆盖会让面板把"未知"画成"0 额度"。
func (s *Stats) NoteRemain(uid string, ide, work float64) {
	if s == nil || uid == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	e := s.entry(uid)
	e.creditsIDE = ide
	e.creditsWork = work
	if e.creditsLimit == 0 {
		e.creditsTotal = ide + work
	}
	e.creditsAt = time.Now()
	e.hasCredits = true
}

// NoteCredits 记录一次权威积分查询的结果（面板「积分」按钮）。
func (s *Stats) NoteCredits(uid string, general, work, total, limit float64) {
	if s == nil || uid == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	e := s.entry(uid)
	e.creditsIDE = general
	e.creditsWork = work
	e.creditsTotal = total
	e.creditsLimit = limit
	e.creditsAt = time.Now()
	e.hasCredits = true
}

// Get 返回单账号快照。
func (s *Stats) Get(uid string) StatView {
	if s == nil {
		return StatView{UID: uid}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.m[uid]
	if !ok {
		return StatView{UID: uid}
	}
	return viewOf(uid, e)
}

// Snapshot 返回全部账号快照（按 uid 升序，输出稳定）。
func (s *Stats) Snapshot() []StatView {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]StatView, 0, len(s.m))
	for uid, e := range s.m {
		out = append(out, viewOf(uid, e))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].UID < out[j].UID })
	return out
}

// Reset 清空某账号的计数（删除账号时调用，避免残留占用内存）。
func (s *Stats) Reset(uid string) {
	if s == nil || uid == "" {
		return
	}
	s.mu.Lock()
	delete(s.m, uid)
	s.mu.Unlock()
}

// viewOf 把内部条目转成只读快照。调用方必须已持有 mu。
func viewOf(uid string, e *statEntry) StatView {
	v := StatView{
		UID:           uid,
		InFlight:      e.inFlight,
		SuccessCount:  e.success,
		ErrTotal:      e.errs,
		LastLatencyMs: int64(e.lastLatency / time.Millisecond),
		TokenUsage: TokenUsageView{
			RequestCount:     e.reqCount,
			UsageCount:       e.usageCount,
			PromptTokens:     e.promptTokens,
			CompletionTokens: e.completionTokens,
			TotalTokens:      e.totalTokens,
			LastModel:        e.lastModel,
		},
	}
	if !e.lastSuccess.IsZero() {
		t := e.lastSuccess
		v.LastSuccess = &t
		v.TokenUsage.LastUsedAt = &t
	}
	if !e.lastErr.IsZero() {
		t := e.lastErr
		v.LastErr = &t
	}
	if e.hasTPS {
		tps := e.lastTPS
		v.TokenUsage.LastTokensPerSecond = &tps
	}
	if e.hasCredits {
		v.HasCredits = true
		v.Credits = e.creditsIDE + e.creditsWork
		v.CreditsIDE = e.creditsIDE
		v.CreditsWork = e.creditsWork
		v.CreditsLimit = e.creditsLimit
		// 手动查询给了上限时以它为准（notify_usage 只有分解，没有上限）。
		if e.creditsLimit > 0 && e.creditsTotal > 0 {
			v.Credits = e.creditsTotal
		}
		t := e.creditsAt
		v.CreditsAt = &t
	}
	return v
}
