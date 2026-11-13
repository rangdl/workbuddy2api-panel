package server

import (
	"errors"
	"io"
	"log"
	"net/http"
	"time"

	"context"
	backendtrae "github.com/linguo2625469/workbuddy2api-panel/internal/backend/trae"
	"github.com/linguo2625469/workbuddy2api-panel/internal/gateway"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/trae"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
	"github.com/linguo2625469/workbuddy2api-panel/internal/usage"
	"os"
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

// dispatchViaBackend 用 acc 所属的 backend 处理本次请求。
func (h *Handler) dispatchViaBackend(w http.ResponseWriter, r *http.Request,
	acc gateway.Account, raw []byte, stream bool, ref gateway.ModelRef,
	st *chatStat, sessKey string) bool {

	be := h.backendFor(acc)
	if be == nil {
		return false // 未注册：按「无可用账号」处理，调用方继续换号
	}

	// 在途租约：与 CodeBuddy 共用同一名额池（搜索/多上游都算真实上游调用）。
	if !h.cfg.Pool.Acquire(acc.UID()) {
		return false
	}
	defer h.cfg.Pool.Release(acc.UID())

	chatID := "chatcmpl-" + shortID()
	in := &gateway.Inbound{
		Raw:        raw,
		Ref:        ref,
		Stream:     stream,
		ClientIP:   upstream.ExtractClientIP(r),
		SessionKey: sessKey,
		ChatID:     chatID,
	}

	started := time.Now()
	out, err := be.Chat(r.Context(), acc, in)
	if err != nil {
		return h.handleBackendFailure(acc, err, st)
	}
	if out == nil || out.Stream == nil {
		st.status = http.StatusBadGateway
		writeOpenAIError(w, http.StatusBadGateway, "upstream_error", "empty upstream response")
		return true
	}
	defer out.Stream.Close()

	if stream {
		h.forwardBackendStream(w, acc, out, chatID, ref.Bare, st, started)
		return true
	}

	// 非流式：把流聚合成一个 completion（backend 统一以流形式返回，
	// 聚合逻辑复用 responses/sse 的通用能力）。
	agg, stats, serr := aggregateBackend(out, chatID, ref.Bare)
	if serr != nil {
		st.status = http.StatusBadGateway
		writeOpenAIError(w, http.StatusBadGateway, "upstream_error", serr.Error())
		return true
	}
	st.status = http.StatusOK
	st.toks = completionTokens(agg)
	// 与流式路径**完全同一套**记账：清冷却、token/缓存/积分、账号级计数。
	// 此前非流式只设了 toks，于是同步请求的 credit 恒为 "-"、缓存率不显示、
	// 面板上的「成功/失败」与「用量·次数」也从不增加。
	h.cfg.Pool.NoteSuccess(acc.UID())
	// recordBackendUsage 内部已包含 token/缓存/积分估算与账号级计数。
	h.recordBackendUsage(acc, ref.Bare, stats, st)
	writeJSON(w, http.StatusOK, agg)
	return true
}

// handleBackendFailure 按 Decision 处置账号，返回是否已写出响应。
func (h *Handler) handleBackendFailure(acc gateway.Account, err error, st *chatStat) bool {
	var f *gateway.Failure
	if !errors.As(err, &f) {
		// 非分类错误（传输层/转换失败）：交给调用方换号，不处置账号。
		log.Printf("WARN: [server] backend chat acct=%s: %v", acc.UID(), err)
		return false
	}
	h.applyDecision(acc, "", f)
	status := f.HTTPStatus
	if status < 400 {
		status = http.StatusBadGateway
	}
	st.status = status
	msg := string(f.Body)
	if msg == "" {
		msg = f.Message
	}
	return false // 仍交给调用方换号（Rotate 由 applyDecision 反映到冷却表）
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
	out *gateway.Outbound, chatID, model string, st *chatStat, started time.Time) {

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	st.status = http.StatusOK

	ctrl := http.NewResponseController(w)
	res, err := out.Convert(func(frame []byte) error {
		if len(frame) == 0 {
			return nil
		}
		if _, werr := w.Write(frame); werr != nil {
			return werr
		}
		return ctrl.Flush()
	})
	if err != nil {
		// 状态码已发出，只能记日志。
		log.Printf("WARN: [server] backend stream acct=%s: %v", acc.UID(), err)
		return
	}
	if res.SentAny {
		st.ttfb = time.Since(started)
	}
	if res.ErrorCode != 0 {
		st.status = http.StatusBadGateway
		return
	}
	// 成功产出内容：清冷却（账号已恢复）。
	h.cfg.Pool.NoteSuccess(acc.UID())
	if res.SentAny {
		st.ttfb = time.Since(started)
	}
	h.recordBackendUsage(acc, model, res, st)
}

// recordBackendUsage 记录一次 backend 调用的用量（流式与非流式共用）。
//
// 三块：token 计数、缓存拆分、积分（实测或估算）。
// 抽出来是因为非流式路径此前只设了 toks，credit 与缓存率都不显示。
func (h *Handler) recordBackendUsage(acc gateway.Account, model string, res gateway.StreamStats, st *chatStat) {
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
	// 账号级计数 + 用量时序：这是面板「成功/失败」「用量·次数」「最近成功」
	// 三列的数据源。此前 dispatch 只调了 NoteSuccess（清冷却），没记账，
	// 于是 Trae 账号在账号池页面永远显示 0 次。
	h.recordBackendAccounting(acc, model, res, st)
}

// recordBackendAccounting 把本次调用计入账号级计数与用量时序。
//
// 与 CodeBuddy 的 recordAttempt 同口径：成功/失败分开记、
// 一次尝试无论成功失败都计一次请求数（否则重试放大在用量视图里看不见）。
func (h *Handler) recordBackendAccounting(acc gateway.Account, model string, res gateway.StreamStats, st *chatStat) {
	if acc == nil || h.cfg.Pool == nil {
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
