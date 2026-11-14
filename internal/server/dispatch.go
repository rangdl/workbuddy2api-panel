package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"time"

	backendtrae "github.com/linguo2625469/workbuddy2api-panel/internal/backend/trae"
	"github.com/linguo2625469/workbuddy2api-panel/internal/gateway"
	"github.com/linguo2625469/workbuddy2api-panel/internal/logfmt"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/reqlog"
	"github.com/linguo2625469/workbuddy2api-panel/internal/trae"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
	"github.com/linguo2625469/workbuddy2api-panel/internal/usage"
)

// dispatch.go 把请求交给非 CodeBuddy 上游处理。
//
// 为什么单独一条路径而不是完全统一：CodeBuddy 链路有大量协议特有逻辑
// （prompt 改写、粘性跟随、11102 模型负缓存、内容拦截降级重试、企业版判定），
// 那些逻辑对 Trae 没有意义甚至有害。统一的是**选号与回退**，请求处理本身
// 仍由各 backend 的实现负责——这正是 gateway.Backend 接口的边界。
//
// 返回 true 表示已完整写出响应（调用方直接 return）；false 表示本轮失败且
// 不值得重试，调用方继续换号。

// backendOutcome 一次 backend 尝试的结果，决定主循环下一步。
type backendOutcome int

const (
	// backendHandled 已完整写出响应（成功或已向客户端报错），主循环直接返回。
	backendHandled backendOutcome = iota
	// backendRetry 换号重试（Decision.Rotate=true 或未分类的传输层错误）。
	backendRetry
	// backendStop 终止轮转（Decision.Rotate=false：换号也只会得到同样的结果，
	// 例如请求体畸形、模型不可用）。
	backendStop
)

// backendAttempt 一次 backend 尝试的上下文（参数多，用具名结构避免长参数表）。
type backendAttempt struct {
	Raw           []byte
	Stream        bool
	Ref           gateway.ModelRef
	SessionKey    string
	ChatMeta      upstream.ChatMeta
	DeclaredTools map[string]bool
	BareModel     string
	HasImage      bool
	// ModelRate 该 (域, 模型) 的积分倍率（用量账本用，空 = 未知）。
	ModelRate string
}

// dispatchViaBackend 用 acc 所属的 backend 处理本次请求。
//
// 返回值三态见 backendOutcome；第三项 err 是本次失败的原因（成功时为 nil），
// 供调用方在轮转耗尽时把上游原文透传给客户端。
func (h *Handler) dispatchViaBackend(w http.ResponseWriter, r *http.Request,
	acc gateway.Account, at backendAttempt, st *chatStat) (backendOutcome, error) {

	be := h.backendFor(acc)
	if be == nil {
		return backendRetry, nil // 未注册：按「无可用账号」处理，调用方继续换号
	}

	// 在途租约：所有上游共用同一名额池。
	if !h.cfg.Pool.Acquire(acc.UID()) {
		return backendRetry, nil
	}
	defer h.cfg.Pool.Release(acc.UID())

	// 凭证预刷新：能力可选（CodeBuddy 需要、Trae 不需要）。
	// 放在租约之后、发请求之前——刷新失败也要先占住名额，避免并发竞态下
	// 同一个死号被多个请求同时刷新。
	if f, ok := acc.(gateway.Freshener); ok && f.NeedsRefresh(h.cfg.RefreshSkew) {
		if err := f.Freshen(r.Context()); err != nil {
			log.Printf("WARN: [server] backend freshen acct=%s: %v", acc.UID(), err)
			var ue *upstream.Error
			if errors.As(err, &ue) && ue.Kind == upstream.ErrSessionDead {
				h.cfg.Pool.Disable(acc.UID(), "refresh session dead")
			} else {
				h.cfg.Pool.NoteError(acc.UID())
			}
			st.status = http.StatusServiceUnavailable
			return backendRetry, err
		}
	}

	chatID := "chatcmpl-" + shortID()
	in := &gateway.Inbound{
		Raw:                   at.Raw,
		Ref:                   at.Ref,
		Stream:                at.Stream,
		ClientIP:              upstream.ExtractClientIP(r),
		SessionKey:            at.SessionKey,
		ChatID:                chatID,
		ConversationID:        at.ChatMeta.ConversationID,
		ConversationRequestID: at.ChatMeta.ConversationRequestID,
		TraceID:               at.ChatMeta.TraceID,
		DeclaredTools:         at.DeclaredTools,
		// 文案策略属网关配置（hint 依赖请求形态），帧识别属 backend，
		// 所以由这里提供映射函数。
		FrameHint: func(kind, payload string) string {
			return h.hintOf(upstream.ErrKindOf(kind), payload, at.BareModel, at.HasImage, nil)
		},
	}

	started := time.Now()
	// Trae 运行期计数（在途/成功/失败/用量）：面板「Trae 签到」区块读这张表。
	// CodeBuddy 不走这里——它的统计由池的 tokenUsage 与 Status 提供。
	// defer 保证任何 return 都结算一次，否则在途数永远不归零。
	traeFinish := h.beginTraeStat(acc)
	statOK := false
	defer func() { traeFinish(statOK) }()

	out, err := be.Chat(r.Context(), acc, in)
	if err != nil {
		// 同账号内重试（RetryHook）：内容拦截这类「换个提示词就能过」的失败，
		// 不值得换号——换号只会把同一个拦截再撞一次，白烧一个健康号的风控额度。
		if hook := be.RetryHook(); hook != nil {
			var f *gateway.Failure
			if errors.As(err, &f) {
				if next, ok := hook.OnFailure(in, f); ok && next != nil {
					log.Printf("INFO: [server] backend retry acct=%s kind=%s (same-account retry)",
						acc.UID(), f.Decision.Kind)
					out, err = be.Chat(r.Context(), acc, next)
				}
			}
		}
		if err != nil {
			return h.handleBackendFailure(acc, at.Ref.Bare, err, st, r)
		}
	}
	if out == nil || out.Stream == nil {
		st.status = http.StatusBadGateway
		writeOpenAIError(w, http.StatusBadGateway, "upstream_error", "empty upstream response")
		return backendHandled, nil
	}
	defer out.Stream.Close()

	if at.Stream {
		statOK = h.forwardBackendStream(w, acc, out, chatID, at.Ref.Bare, at.ModelRate, st, started, at.SessionKey)
		return backendHandled, nil
	}

	// 非流式：把流聚合成一个 completion（backend 统一以流形式返回，
	// 聚合逻辑复用 upstream 的通用能力）。
	agg, stats, serr := aggregateBackend(out, chatID, at.Ref.Bare)
	if serr != nil {
		st.status = http.StatusBadGateway
		writeOpenAIError(w, http.StatusBadGateway, "upstream_error", serr.Error())
		return backendHandled, serr
	}
	// 标记修复（非流式）：把正文里的原生工具调用标记还原成 message.tool_calls。
	// 与流式路径同一套判定（名单为空时走弱判定——tools 声明在中转环节丢失是常态）。
	repair := upstream.RepairAggregatedResponse(agg, at.DeclaredTools, true)
	if n := repair.Converted(); n > 0 {
		log.Printf("INFO: [server] non-stream model=%s: repaired %d native tool call(s) from assistant text",
			at.Ref.Bare, n)
	} else if m := repair.Seen(); m > 0 {
		log.Printf("WARN: [server] non-stream model=%s: saw %d native tool call block(s) but repaired none",
			at.Ref.Bare, m)
	}
	st.status = http.StatusOK
	st.toks = completionTokens(agg)
	// 与流式路径**完全同一套**记账：清冷却、清负缓存、粘性跟随、token/缓存/积分。
	// 此前非流式只设了 toks，于是同步请求的 credit 恒为 "-"、缓存率不显示、
	// 面板上的「成功/失败」与「用量·次数」也从不增加。
	statOK = true
	h.cfg.Pool.NoteSuccess(acc.UID())
	h.cfg.Pool.BlockModelClear(acc.UID(), at.Ref.Bare)
	h.bindSessionAfterSuccess(at.SessionKey, acc)
	// recordBackendUsage 内部已包含 token/缓存/积分估算与账号级计数。
	h.recordBackendUsage(acc, at.Ref.Bare, at.ModelRate, stats, st)
	writeJSON(w, http.StatusOK, agg)
	return backendHandled, nil
}

// beginTraeStat 开启 Trae 账号的运行期计数；非 Trae 账号返回空操作。
//
// 为什么在 dispatch 而不是在 backend 内部：面板读的是 h.cfg.TraeStats 这个实例
// （main 构造、面板与 handler 共用），backend 包不认识它。
// 为什么必须用 defer：在途计数不归零会让面板永远显示「1 个在途」。
func (h *Handler) beginTraeStat(acc gateway.Account) func(ok bool) {
	stats := h.cfg.TraeStats
	if stats == nil || acc == nil || acc.Backend() != "trae" {
		return func(bool) {}
	}
	finish := stats.Begin(acc.UID())
	return func(ok bool) {
		// UsageDelta 的 token/延迟字段由 recordBackendUsage 另行落池，
		// 这里只结算在途与成功/失败（面板的「成功/失败/在途」三列）。
		finish(ok, trae.UsageDelta{})
	}
}

// handleBackendFailure 按 Decision 处置账号，返回三态结果与失败原因。
//
// model 是本次请求的裸模型名：必须传给 applyDecision，否则 ActionModelCooldown
// 会带着空模型名调 BlockModelBackoff，而该函数见空模型名直接返回——Trae 的
// 4001/4023「模型不可用」就永远写不进模型级负缓存，每次请求都要重新撞一遍上游。
func (h *Handler) handleBackendFailure(acc gateway.Account, model string, err error, st *chatStat, r *http.Request) (backendOutcome, error) {
	var f *gateway.Failure
	if !errors.As(err, &f) {
		// 非分类错误（传输层 / 转换失败）。
		//
		// 上游超时：**不换号、不罚号**。同一份请求换到别的号，撞上的是同一个慢
		// 上游，只会把客户端拖到 MaxRotate × header_timeout，期间还给一串健康号
		// 喂连败计数。与内联路径同判据（isUpstreamTimeout 三态）。
		if r != nil && isUpstreamTimeout(err, r.Context().Err() != nil) {
			st.status = http.StatusServiceUnavailable
			log.Printf("WARN: [server] backend upstream timeout acct=%s: %v (rotation stopped, account not penalized)",
				acc.UID(), err)
			// 失败尝试也计请求数：否则超时在「用量」视图里等于没发生。
			h.recordBackendAccounting(acc, model, "", gateway.StreamStats{}, st)
			return backendStop, fmt.Errorf("%w: %v", errUpstreamTimeout, err)
		}
		// 网络抖动：只换号，不喂熔断（传输层错误对连败熔断过于严苛）。
		log.Printf("WARN: [server] backend chat acct=%s: %v", acc.UID(), err)
		if h.cfg.Pool != nil {
			h.cfg.Pool.NoteFailures(acc.UID())
		}
		st.status = http.StatusServiceUnavailable
		h.recordBackendAccounting(acc, model, "", gateway.StreamStats{}, st)
		return backendRetry, err
	}
	h.applyFailure(acc, model, f, st)
	status := f.HTTPStatus
	if status < 400 {
		status = http.StatusBadGateway
	}
	st.status = status
	// 失败尝试也要计入账号级请求数与用量时序：否则面板「成功/失败」把失败
	// 全算成成功（请求数不涨即视为没发生），重试放大也看不见。
	// 与内联路径的 recordAttempt(空 delta) 同口径。
	h.recordBackendAccounting(acc, model, "", gateway.StreamStats{}, st)
	// Rotate 是 backend 对「换号是否有意义」的明确判断：请求体畸形这类错误
	// 换到哪个号都得到同样结果，继续轮转只会把同一个错误烧 MaxRotate 遍。
	// （模型不可用也标 Rotate=true——换有权限的账号仍可能成功，见
	// codebuddy.Decide 的 ErrModelBlocked 分支。）
	if !f.Decision.Rotate {
		return backendStop, f
	}
	return backendRetry, f
}

// applyFailure 按账号所属上游选择精细错误策略。
//
// 为什么不能统一走 applyDecision：CodeBuddy 的 applyErrorPolicy 有 12 类分支，
// 包含 6004 的墙钟重置对齐、11102 的指数负缓存、softStreak 封顶、WAF 的 IP 级
// fail-fast 等精细化处置；applyDecision 只有固定时长（1m/5m/12h）。把 CodeBuddy
// 的失败交给 applyDecision 就是**降级**。
//
// 判据用 Decision.Policy（由 backend 显式标注），**不用 Kind 字符串反查**：
// Trae 的 soft_rate/session_dead/server/client 与 CodeBuddy 的 ErrKind 同名，
// 按名字反查会把 Trae 失败送进 CodeBuddy 策略（冷却放大约 10 倍、
// session_dead 的 reason 写成 12153、5xx 不再冷却）。
func (h *Handler) applyFailure(acc gateway.Account, model string, f *gateway.Failure, st *chatStat) {
	if f.Decision.Policy == gateway.PolicyCodeBuddy {
		kind := upstream.ErrKindOf(f.Decision.Kind)
		body := string(f.Body)
		if body == "" {
			body = f.Message
		}
		// Retry-After 头在 backend 分类时已折算进 Decision.Cooldown，
		// 这里还原成 *upstream.Error 让 WAF/429 的 retry-after 分支照常生效。
		var uerr *upstream.Error
		if f.Decision.Cooldown > 0 {
			uerr = &upstream.Error{Kind: kind, Status: f.HTTPStatus, RetryAfter: f.Decision.Cooldown}
		}
		if kind == upstream.ErrNone {
			// Kind 不认识（防御）：回落到通用策略，别让一次分类失误吞掉处置。
			h.applyDecision(acc, model, f)
			return
		}
		h.applyErrorPolicy(acc.UID(), kind, body, model, uerr)
		return
	}
	h.applyDecision(acc, model, f)
}

// applyDecision 按统一处置动作更新账号状态。
func (h *Handler) applyDecision(acc gateway.Account, modelForDecision string, f *gateway.Failure) {
	uid := acc.UID()
	d := f.Decision
	switch d.Action {
	case gateway.ActionDisable:
		h.cfg.Pool.Disable(uid, d.Message)
	case gateway.ActionCoolShort, gateway.ActionCoolMedium, gateway.ActionCoolLong:
		cool := d.Cooldown
		if cool <= 0 {
			switch d.Action {
			case gateway.ActionCoolShort:
				cool = time.Minute
			case gateway.ActionCoolMedium:
				cool = 5 * time.Minute
			default:
				cool = 12 * time.Hour
			}
		}
		kind := pool.CoolSoft
		if d.Action == gateway.ActionCoolLong {
			kind = pool.CoolHard
		}
		h.cfg.Pool.Cooldown(uid, kind, cool, d.Message)
	case gateway.ActionModelCooldown:
		// 模型级避让：backend 把模型名放在 Decision.Code 之外，这里用请求模型。
		h.cfg.Pool.BlockModelBackoff(uid, modelForDecision, d.Message)
	case gateway.ActionDegrade:
		h.cfg.Pool.NoteFailures(uid)
	case gateway.ActionNone:
		// 不罚账号
	}
	log.Printf("[server] backend failure acct=%s kind=%s code=%s action=%s rotate=%v: %s",
		acc.UID(), d.Kind, d.Code, d.Action, d.Rotate, d.Message)
}

// forwardBackendStream 把 backend 的规范化帧转发给客户端。
func (h *Handler) forwardBackendStream(w http.ResponseWriter, acc gateway.Account,
	out *gateway.Outbound, chatID, model, modelRate string, st *chatStat, started time.Time, sessKey string) (ok bool) {

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	st.status = http.StatusOK

	ctrl := http.NewResponseController(w)
	// ttfb 在**首个真实帧**上打点，而不是 Convert 跑完之后取总耗时。
	// 后者会让流水行的 TTFB 列 ≈ 端到端耗时，且 tokensPerSecond 扣 TTFB 的
	// 那套口径（issue #34/#127）失效。
	var firstFrameAt time.Time
	res, err := out.Convert(func(frame []byte) error {
		if len(frame) == 0 {
			return nil
		}
		if firstFrameAt.IsZero() {
			firstFrameAt = time.Now()
		}
		if _, werr := w.Write(frame); werr != nil {
			return werr
		}
		return ctrl.Flush()
	})
	if !firstFrameAt.IsZero() {
		st.ttfb = firstFrameAt.Sub(started)
	}
	if err != nil {
		// 状态码已发出，只能记日志。
		if upstream.IsEmptyStreamError(err) {
			// 上游 200 但空流（0 有效帧）：Convert 已写 error 帧 + [DONE] 兜底
			// （HTTP 头已发出只能 200），但这是上游缺陷不是成功——状态收敛到 502
			// 观测，与非流式 Aggregate 空流 → 502 upstream_parse 同语义。
			// 只认 IsEmptyStreamError：客户端断连的写失败不误标（人已走，
			// 502 观测没有意义）。
			st.status = http.StatusBadGateway
			st.outcome = reqlog.OutcomeStreamError
			log.Printf("WARN: [server] backend stream acct=%s model=%s: empty upstream stream (200+0 frames)",
				acc.UID(), model)
			h.recordBackendUsage(acc, model, modelRate, res, st)
			return
		}
		// 客户端断连（写失败）：上游帧无恙，账号健康——照常记成功并让粘性跟上，
		// 请求日志归为 Interrupted（人已走，未完成）。
		// 不区分会把断连误记成 success，污染用量视图的成功率。
		st.outcome = reqlog.OutcomeInterrupted
		h.cfg.Pool.NoteSuccess(acc.UID())
		h.cfg.Pool.BlockModelClear(acc.UID(), model)
		h.bindSessionAfterSuccess(sessKey, acc)
		log.Printf("WARN: [server] backend stream acct=%s: %v (client disconnected)", acc.UID(), err)
		h.recordBackendUsage(acc, model, modelRate, res, st)
		return
	}
	// 流内 error 帧（Trae 的自定义事件流 / CodeBuddy 的 6004 限流帧）：
	// 已下发内容后无法改状态码，但仍要按错误分类处置账号——否则一个正在
	// 限流的号会被记成健康，下一轮继续打它。
	if res.ErrorCode != 0 {
		st.status = http.StatusBadGateway
		st.outcome = reqlog.OutcomeStreamError
		h.applyStreamError(acc, model, res, st)
		return
	}
	if res.Repaired > 0 {
		log.Printf("INFO: [server] stream acct=%s model=%s: repaired %d native tool call(s) from assistant text",
			acc.UID(), model, res.Repaired)
	} else if res.RepairSeen > 0 {
		log.Printf("WARN: [server] stream acct=%s model=%s: saw %d native tool call block(s) but repaired none",
			acc.UID(), model, res.RepairSeen)
	}
	// 成功产出内容：清冷却（账号已恢复）+ 清该模型的 11102 负缓存
	// （实测成功是「这个模型能用」的最强证据，不必等 TTL 到期）。
	h.cfg.Pool.NoteSuccess(acc.UID())
	h.cfg.Pool.BlockModelClear(acc.UID(), model)
	// 粘性跟随最终成功号：与内联路径同语义——绑定发生在**真正成功之后**，
	// 而不是选号时，避免把会话钉在一个正在限流的号上。
	h.bindSessionAfterSuccess(sessKey, acc)
	h.recordBackendUsage(acc, model, modelRate, res, st)
	return true
}

// bindSessionAfterSuccess 成功一跳后把会话绑定到该账号（未启用粘性时为空操作）。
func (h *Handler) bindSessionAfterSuccess(sessKey string, acc gateway.Account) {
	if sessKey == "" || h.cfg.Session == nil || acc == nil {
		return
	}
	h.cfg.Session.Bind(sessKey, pool.TriedKey(acc))
}

// applyStreamError 处置流内错误帧（无法改状态码，只能更新账号状态）。
//
// 与内联路径的流尾处置同语义：不记成功、不清负缓存、不绑粘性。
func (h *Handler) applyStreamError(acc gateway.Account, model string, res gateway.StreamStats, st *chatStat) {
	// 按上游分流分类器：两家的流内错误码表完全不同。
	//   - CodeBuddy：error 帧原文 → FrameKind（6004/11102/内容拦截等）
	//   - Trae：业务码在 StreamStats.ErrorCode（1005/4008/4017/20405 等），
	//     必须走 trae.Classify。把业务码当 HTTP status 传给 upstream.Classify
	//     会让「额度用尽」被当成未知错误——账号不冷却，继续被轮转选中。
	if acc.Backend() == "trae" {
		h.applyTraeStreamError(acc, model, res)
		return
	}
	kind := upstream.FrameKind(res.ErrorMsg)
	if kind == upstream.ErrNone {
		// 非业务码（backend 用 ErrorCode=-1 标记「原文在 ErrorMsg」）：按上游原文分类。
		// 传 0 而不是 int(ErrorCode)：-1 不是合法 HTTP status，会干扰分类。
		kind = upstream.Classify(0, res.ErrorMsg)
	}
	log.Printf("WARN: [server] stream acct=%s model=%s: upstream error frame kind=%s payload=%s",
		acc.UID(), model, kind, logfmt.Truncate(res.ErrorMsg, 200))
	h.applyErrorPolicy(acc.UID(), kind, res.ErrorMsg, model, nil)
}

// applyTraeStreamError 处置 Trae 流内错误（用自己的分类器与决策表）。
func (h *Handler) applyTraeStreamError(acc gateway.Account, model string, res gateway.StreamStats) {
	cls := trae.Classify(0, nil, res.ErrorCode, res.ErrorMsg)
	d := backendtrae.Decide(cls)
	log.Printf("WARN: [server] stream acct=%s model=%s: trae stream error kind=%s code=%s action=%s payload=%s",
		acc.UID(), model, cls.Kind, cls.Code, d.Action, logfmt.Truncate(res.ErrorMsg, 200))
	h.applyDecision(acc, model, &gateway.Failure{
		Decision:   d,
		HTTPStatus: cls.HTTPStatus(),
		Body:       []byte(res.ErrorMsg),
	})
}

// recordBackendUsage 记录一次 backend 调用的用量（流式与非流式共用）。
//
// 三块：token 计数、缓存拆分、积分（实测或估算）。
// 抽出来是因为非流式路径此前只设了 toks，credit 与缓存率都不显示。
func (h *Handler) recordBackendUsage(acc gateway.Account, model, modelRate string, res gateway.StreamStats, st *chatStat) {
	if acc == nil || st == nil {
		return
	}
	if res.Usage != nil {
		st.toks = int(res.Usage.CompletionTokens)
		if res.Usage.CacheRead > 0 || res.Usage.CacheCreation > 0 {
			st.cacheHit = res.Usage.CacheRead
			miss := res.Usage.PromptTokens - res.Usage.CacheRead
			if miss < 0 {
				miss = 0
			}
			st.cacheMiss = miss
			st.hasCache = true
		}
	}
	// 实测扣费优先：上游在 usage 里直接给了 credit（CodeBuddy）。
	if res.Usage != nil && res.Usage.HasCredit {
		st.credit = res.Usage.Credit
		st.hasCredit = true
		st.creditEstimated = false
	}
	// 积分估算：Trae 的 chat 流不含扣费字段（实测），只能按 token × 模型倍率推算。
	// 标成估算——它不参与选号成本分层，只作数量级参考。
	if res.Usage != nil && !st.hasCredit {
		if rate, ok := h.traeRateFor(acc, model); ok {
			if est, ok2 := trae.EstimateCreditsFromUsage(
				res.Usage.PromptTokens, res.Usage.CacheRead,
				res.Usage.CacheCreation, res.Usage.CompletionTokens, rate); ok2 {
				log.Printf("[server] 积分估算 acct=%s model=%s rate=%v prompt=%d cacheRead=%d cacheWrite=%d completion=%d → %.4f",
					acc.UID(), model, rate, res.Usage.PromptTokens, res.Usage.CacheRead,
					res.Usage.CacheCreation, res.Usage.CompletionTokens, est)
				st.credit = est
				st.hasCredit = true
				st.creditEstimated = true
			}
		}
	}
	// 余额：notify_usage 是 Trae 唯一能在流内拿到的额度口径。
	if res.Remain != nil {
		if res.Remain.HasConsumed {
			st.credit = res.Remain.Consumed
			st.hasCredit = true
			st.creditEstimated = false // 上游实测优先
		}
		h.noteBackendRemain(acc, res.Remain)
	}
	// 成本账本：末帧 usage 带 credit 与 token 总数时记录实测单价，
	// 供下次选号把免费/便宜的号排在前面（与内联路径同一逻辑，此前 dispatch 缺这一步）。
	if st.hasCredit && !st.creditEstimated && res.Usage != nil && res.Usage.TotalTokens > 0 {
		h.cfg.Pool.NoteModelCost(acc.UID(), model, st.credit, int(res.Usage.TotalTokens))
	}
	// 账号级计数 + 用量时序：这是面板「成功/失败」「用量·次数」「最近成功」
	// 三列的数据源。此前 dispatch 只调了 NoteSuccess（清冷却），没记账，
	// 于是 Trae 账号在账号池页面永远显示 0 次。
	h.recordBackendAccounting(acc, model, modelRate, res, st)
}

// recordBackendAccounting 把本次调用计入账号级计数与用量时序。
//
// 与 CodeBuddy 的 recordAttempt 同口径：成功/失败分开记、
// 一次尝试无论成功失败都计一次请求数（否则重试放大在用量视图里看不见）。
func (h *Handler) recordBackendAccounting(acc gateway.Account, model, modelRate string, res gateway.StreamStats, st *chatStat) {
	if acc == nil || st == nil {
		return
	}
	// 一次尝试计一次请求数（成功与失败都算）：否则重试放大在「用量」视图里
	// 看不见——一次用户请求打了 3 个号，面板只显示 1 次。
	st.attempts++
	// token 观测写进 chatStat：请求日志行与「用量」视图都读它，
	// 不写则面板永远显示 0 token（此前内联路径的 recordAttempt 干这件事）。
	if res.Usage != nil {
		st.promptTokens = res.Usage.PromptTokens
		st.completionTokens = res.Usage.CompletionTokens
		st.totalTokens = res.Usage.TotalTokens
		if st.totalTokens == 0 && (st.promptTokens > 0 || st.completionTokens > 0) {
			st.totalTokens = st.promptTokens + st.completionTokens
		}
	}
	if h.cfg.Pool == nil {
		return
	}
	uid := acc.UID()
	var delta pool.TokenUsageDelta
	delta.Model = model
	ok := res.ErrorCode == 0 && st.status == http.StatusOK
	if res.Usage != nil {
		delta.HasPromptTokens = true
		delta.PromptTokens = res.Usage.PromptTokens
		delta.HasCompletionTokens = true
		delta.CompletionTokens = res.Usage.CompletionTokens
		delta.HasTotalTokens = true
		delta.TotalTokens = res.Usage.TotalTokens
	}
	// 延迟与吞吐：TTFB 是流式的首字节；非流式没有 TTFB 时用总耗时。
	latency := time.Since(st.start)
	latencyMs := latency.Milliseconds()
	if latencyMs < 1 {
		latencyMs = 1
	}
	delta.HasLatencyMs = true
	delta.LatencyMs = latencyMs
	if delta.HasCompletionTokens {
		if tps, tok := tokensPerSecond(delta.CompletionTokens, latency, st.ttfb); tok {
			delta.HasTokensPerSecond = true
			delta.TokensPerSecond = tps
		}
	}
	h.cfg.Pool.RecordTokenUsage(uid, delta)

	// 用量时序（面板「用量」视图按 realm 分组）。Trae 用独立 realm 便于分账。
	if h.cfg.Usage != nil {
		realm := acc.Realm()
		if realm == "" {
			realm = "cn"
		}
		if acc.Backend() == "trae" {
			realm = "trae"
		}
		h.cfg.Usage.Add(time.Now(), realm, uid, model, usage.Delta{
			PromptTokens:     delta.PromptTokens,
			HasPromptTokens:  delta.HasPromptTokens,
			CompletionTokens: delta.CompletionTokens,
			HasCompletion:    delta.HasCompletionTokens,
			TotalTokens:      delta.TotalTokens,
			HasTotal:         delta.HasTotalTokens,
			Credit:           st.credit,
			HasCredit:        st.hasCredit,
			HasCacheTokens:   st.hasCache,
			CacheHitTokens:   st.cacheHit,
			CacheMissTokens:  st.cacheMiss,
			ModelRate:        modelRate,
			LatencyMs:        latencyMs,
		}, ok)
	}
}

// InitTraeRawDump 按环境变量 TRAE_RAW_DUMP 开启原始事件转储。
//
// 用法：TRAE_RAW_DUMP=1 启动 → 每次 Trae 对话的完整事件流打进日志。
// 默认关闭：常态开启会把对话内容全量落日志，量大且涉及隐私。
func InitTraeRawDump() {
	if os.Getenv("TRAE_RAW_DUMP") == "" {
		return
	}
	trae.SetRawDump(true)
	trae.DumpEvent = func(event, data string) {
		log.Printf("[trae-raw] event=%q data=%s", event, data)
	}
	log.Printf("[trae] 原始事件转储已开启（TRAE_RAW_DUMP），对话内容会写入日志")
}

// traeRateFor 取该模型在 Trae 目录里的积分倍率。
//
// 目录拉不到或该模型无倍率信息时返回 false——不估算未知倍率，
// 否则会把「未知」写成 0 被误读成免费。
func (h *Handler) traeRateFor(acc gateway.Account, model string) (float64, bool) {
	if acc == nil || acc.Backend() != "trae" || model == "" {
		return 0, false
	}
	if h.cfg.TraeClient == nil {
		return 0, false
	}
	cfg := h.traeConfig()
	if cfg == nil {
		return 0, false
	}
	a := backendtrae.Unwrap(acc)
	if a == nil {
		return 0, false
	}
	cat := h.cfg.TraeClient.FetchRemoteModels(context.Background(), cfg, a, false)
	if cat == nil {
		return 0, false
	}
	row, ok := cat[model]
	if !ok || row.CreditMultiplier == nil || *row.CreditMultiplier <= 0 {
		return 0, false
	}
	return *row.CreditMultiplier, true
}

// noteBackendRemain 记录上游流内上报的余额（并更新选号用的 credits）。
func (h *Handler) noteBackendRemain(acc gateway.Account, r *gateway.Remain) {
	if acc == nil || r == nil {
		return
	}
	if h.cfg.TraeStats != nil && acc.Backend() == "trae" {
		h.cfg.TraeStats.NoteRemain(acc.UID(), r.Available, r.Work)
	}
	// 池的 credits 用 IDE 额度（对话真正消耗的那份）。
	if r.Available > 0 {
		h.cfg.Pool.SetCredits(acc.UID(), int64(r.Available), 0)
	}
}

// aggregateBackend 把 backend 的流聚合成非流式 completion，并返回流统计。
func aggregateBackend(out *gateway.Outbound, chatID, model string) (map[string]any, gateway.StreamStats, error) {
	// 用一个内存管道把 Convert 的 emit 转成可读流，再交给通用聚合。
	pr, pw := io.Pipe()
	var stats gateway.StreamStats
	done := make(chan struct{})
	go func() {
		defer close(done)
		s, err := out.Convert(func(frame []byte) error {
			_, werr := pw.Write(frame)
			return werr
		})
		stats = s
		pw.CloseWithError(err)
	}()
	// 复用 trae 无关的通用聚合：两个 backend 的 Convert 都产出 OpenAI SSE。
	resp, err := upstream.Aggregate(pr)
	<-done // 等 Convert 收尾，拿到完整统计（含 usage）
	if err != nil {
		return nil, stats, err
	}
	if resp["id"] == nil || resp["id"] == "" {
		resp["id"] = chatID
	}
	resp["model"] = model
	return resp, stats, nil
}

// shortID 生成本请求的短随机 id（拼 chatcmpl 前缀用）。
func shortID() string {
	s, err := trae.RandomHex(4)
	if err != nil {
		return "x"
	}
	return s
}
