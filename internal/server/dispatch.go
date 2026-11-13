package server

import (
	"errors"
	"io"
	"log"
	"net/http"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/gateway"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/trae"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
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
	agg, serr := aggregateBackend(out, chatID, ref.Bare)
	if serr != nil {
		st.status = http.StatusBadGateway
		writeOpenAIError(w, http.StatusBadGateway, "upstream_error", serr.Error())
		return true
	}
	st.status = http.StatusOK
	st.toks = completionTokens(agg)
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
	if res.Usage != nil {
		st.toks = int(res.Usage.CompletionTokens)
	}
}

// aggregateBackend 把 backend 的流聚合成非流式 completion。
func aggregateBackend(out *gateway.Outbound, chatID, model string) (map[string]any, error) {
	// 用一个内存管道把 Convert 的 emit 转成可读流，再交给通用聚合。
	pr, pw := io.Pipe()
	go func() {
		_, err := out.Convert(func(frame []byte) error {
			_, werr := pw.Write(frame)
			return werr
		})
		pw.CloseWithError(err)
	}()
	// 复用 trae 无关的通用聚合：两个 backend 的 Convert 都产出 OpenAI SSE。
	resp, err := upstream.Aggregate(pr)
	if err != nil {
		return nil, err
	}
	if resp["id"] == nil || resp["id"] == "" {
		resp["id"] = chatID
	}
	resp["model"] = model
	return resp, nil
}

// shortID 生成本请求的短随机 id（拼 chatcmpl 前缀用）。
func shortID() string {
	s, err := trae.RandomHex(4)
	if err != nil {
		return "x"
	}
	return s
}
