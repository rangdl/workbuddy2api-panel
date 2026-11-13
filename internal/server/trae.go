package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/responses"
	"github.com/linguo2625469/workbuddy2api-panel/internal/trae"
	"github.com/linguo2625469/workbuddy2api-panel/internal/usage"
)

// 本文件实现 Trae 上游的三个端点（/trae/v1/*），与 CodeBuddy 的 /v1/*、
// 第三方的 /tp/v1/* 三者互不影响。
//
// 与第三方链路的关键差异：Trae 是**私有协议**——请求体要改写成 llm_utils_chat
// 形态（internal/trae/payload.go），响应是自定义 SSE 要转换（internal/trae/sse.go），
// 且账号池带 DeviceProof 能力（internal/trae/proof.go）。

// traeRotate 账号轮转游标（进程内递增，取模选号）。
var traeRotate atomic.Uint64

// traeConfig 返回当前生效的 Trae 配置（面板热重载后即为新值；可能为 nil）。
func (h *Handler) traeConfig() *trae.Config {
	return h.traeCfg.Load()
}

// traeEnabled Trae 是否可用：配置存在、已启用、客户端已注入。
func (h *Handler) traeEnabled() bool {
	cfg := h.traeConfig()
	return cfg != nil && cfg.Enabled && h.cfg.TraeClient != nil
}

// traePickAccount 选一个可用账号（简单轮转，跳过 disabled 与冷却中）。
// 返回 nil 表示无可用账号。
func traePickAccount(cfg *trae.Config, cool *trae.Cooldowns) *trae.Account {
	return traePickAccountExcluding(cfg, cool, nil)
}

// traePickAccountExcluding 同上，但跳过 exclude 里的 uid（换号重试用）。
func traePickAccountExcluding(cfg *trae.Config, cool *trae.Cooldowns, exclude map[string]bool) *trae.Account {
	usable := make([]trae.Account, 0, len(cfg.Accounts))
	for _, a := range cfg.Accounts {
		if a.Disabled || a.AccessToken == "" {
			continue
		}
		if exclude != nil && exclude[a.UID] {
			continue
		}
		if cool != nil && !cool.Available(a.UID) {
			continue
		}
		usable = append(usable, a)
	}
	if len(usable) == 0 {
		return nil
	}
	idx := int(traeRotate.Add(1)-1) % len(usable)
	a := usable[idx]
	return &a
}

// traeApplyPolicy 按错误分类施加账号处置。
//
// 处置只有两种落点：冷却表（进程内）与日志——**不写回 trae.json**。
// 理由：配置文件可能被用户手工编辑，网关在运行期改写它会制造"谁改了配置"的困惑；
// 而"需要重新登录/重新导入设备凭证"这类问题本就该由用户在面板处理，
// 网关能做的是把它从轮转里摘出去（长冷却）并明确告知原因。
func (h *Handler) traeApplyPolicy(uid string, cls trae.Classification) {
	if uid == "" || cls.Kind == trae.ErrNone {
		return
	}
	if cls.Disable {
		// 凭证/设备问题：冷却不会自愈，用超长冷却等价于"摘出轮转"。
		h.traeCool.Mark(uid, 24*time.Hour, cls.Kind, cls.Message)
		log.Printf("WARN: [trae] 账号 %s 已摘出轮转（%s）: %s", uid, cls.Kind, cls.Message)
		return
	}
	if cls.Cooldown > 0 {
		h.traeCool.Mark(uid, cls.Cooldown, cls.Kind, cls.Message)
		log.Printf("WARN: [trae] 账号 %s 冷却 %s（%s）: %s", uid, cls.Cooldown, cls.Kind, cls.Message)
	}
}

// traeChat 处理 POST /trae/v1/chat/completions：Chat → llm_utils_chat → OpenAI。
func (h *Handler) traeChat(w http.ResponseWriter, r *http.Request) {
	if !h.traeEnabled() {
		writeOpenAIError(w, http.StatusNotFound, "not_found", "trae upstream is disabled")
		return
	}
	cfg := h.traeConfig()
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "read body: "+err.Error())
		return
	}
	var peek struct {
		Model  string `json:"model"`
		Stream bool   `json:"stream"`
	}
	_ = json.Unmarshal(body, &peek)

	st := newChatStat(time.Now(), body, peek.Stream)
	defer st.done()

	// 带换号重试地打开上游流。prepare 每次按当前账号重新构造请求体
	// （user_id / device_id 等字段与账号绑定）。
	attempt := h.traeOpenWithRotate(r.Context(), cfg, func(acc *trae.Account) ([]byte, error) {
		return trae.PrepareChatBody(body, trae.PrepareOptions{
			UID:            acc.UID,
			DeviceID:       acc.DeviceID,
			MachineID:      acc.MachineID,
			IDEVersion:     cfg.IDEVersion,
			IDEVersionCode: cfg.IDEVersionCode,
			AppID:          cfg.AppID,
			// function 归属以上游目录为准（硬编码表可能与上游不一致 → 4001）。
			Function: h.traeFunctionFor(cfg, acc, peek.Model),
		})
	})
	acc := attempt.acc
	if acc == nil {
		// 没有任何可用账号是「服务暂不可用」，不是上游错误。
		st.status = http.StatusServiceUnavailable
		writeOpenAIError(w, http.StatusServiceUnavailable, "no_healthy_account",
			"no usable trae account (check trae.json)")
		return
	}
	st.uid = acc.UID
	st.nick = acc.Name

	// 从这里起账号已确定：开启运行期计数，defer 保证任何 return 都结算一次
	//（在途数必须归零，否则面板会永远显示「1 个在途」）。
	finishStat := h.traeStats.Begin(acc.UID)
	ok := false
	var statDelta trae.UsageDelta
	defer func() { finishStat(ok, statDelta) }()

	if attempt.err != nil {
		st.status = http.StatusBadGateway
		writeOpenAIError(w, http.StatusBadGateway, "upstream_error", attempt.err.Error())
		return
	}
	if attempt.status >= 400 {
		st.status = attempt.status
		writeOpenAIError(w, attempt.status, traeErrorCode(attempt.status), upstreamText(attempt.body))
		return
	}
	rc := attempt.rc
	defer rc.Close()

	// 200 但流内首个事件是 error（额度不足等）：必须在 HTTP 边界变现，
	// 否则客户端只看到"流意外结束"然后重试多次，每次都在烧风控额度。
	peeked, perr := trae.PeekStream(rc)
	if perr != nil {
		st.status = http.StatusBadGateway
		writeOpenAIError(w, http.StatusBadGateway, "upstream_error", perr.Error())
		return
	}
	if peeked.Error != nil {
		cls := trae.Classify(0, nil, peeked.Error.Code, peeked.Error.Message)
		h.traeApplyPolicy(acc.UID, cls)
		status := cls.HTTPStatus()
		st.status = status
		writeOpenAIError(w, status, traeErrorCode(status), pickNonEmpty(cls.Message, peeked.Error.Message))
		return
	}
	// 排队过深：记录日志并给账号一个短冷却，让后续请求优先落到更空闲的号。
	// 本轮请求已经拿到流，仍照常转发（中断它反而让用户多等一轮）。
	if peeked.HasQueue {
		h.traeNoteQueue(acc.UID, peeked.QueuePosition)
	}
	peekBody := peeked.Header

	chatID := "chatcmpl-trae-" + shortID()
	if peek.Stream {
		streamStart := time.Now()
		res := h.traeForwardStream(w, io.MultiReader(bytes.NewReader(peekBody), rc), chatID, peek.Model, st)
		// 只有真的产出过内容才算成功：中途 error 事件（配额耗尽等）要计入失败，
		// 否则面板会显示「全部成功」而实际一直在撞额度。
		ok = res.ErrorCode == 0 && res.SentAny
		statDelta = deltaFromStream(res, st.ttfb, time.Since(streamStart))
		return
	}
	resp, streamErr, err := trae.Aggregate(io.MultiReader(bytes.NewReader(peekBody), rc), chatID, peek.Model)
	if err != nil {
		st.status = http.StatusBadGateway
		writeOpenAIError(w, http.StatusBadGateway, "upstream_parse", err.Error())
		return
	}
	if streamErr != nil {
		h.traeApplyPolicy(acc.UID, trae.Classify(0, nil, streamErr.Code, streamErr.Message))
		st.status = http.StatusBadGateway
		writeOpenAIError(w, http.StatusBadGateway, "upstream_error", streamErr.Message)
		return
	}
	// 成功即清冷却：账号恢复后不必等冷却自然到期。
	h.traeCool.Clear(acc.UID)
	st.status = http.StatusOK
	st.toks = completionTokens(resp)
	ok = true
	statDelta = deltaFromResponse(resp, peek.Model)
	h.traeRecord(acc.UID, peek.Model, resp)
	writeJSON(w, http.StatusOK, resp)
}

// traeForwardStream 把 SOLO SSE 转成 OpenAI SSE 边转边发，返回转换结果供调用方
// 记账（成功/失败、token 用量、剩余额度）。
func (h *Handler) traeForwardStream(w http.ResponseWriter, rc io.Reader, chatID, model string, st *chatStat) trae.StreamResult {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	st.status = http.StatusOK

	ctrl := http.NewResponseController(w)
	start := st.start
	res, err := trae.ConvertStream(rc, chatID, model, func(b []byte) error {
		if len(b) == 0 {
			return nil
		}
		if _, werr := w.Write(b); werr != nil {
			return werr // 客户端断连：中止转换
		}
		return ctrl.Flush()
	})
	if err != nil {
		// 写失败（客户端断连）或上游读错误：状态码已发出，只能记日志。
		return res
	}
	if res.SentAny {
		st.ttfb = time.Since(start)
	}
	if res.ErrorCode != 0 {
		h.traeApplyPolicy(st.uid, trae.Classify(0, nil, res.ErrorCode, res.ErrorMsg))
		st.status = http.StatusBadGateway
	} else if res.SentAny {
		// 有内容成功产出：清冷却（账号已恢复）。
		h.traeCool.Clear(st.uid)
	}
	// notify_usage 是唯一能区分 IDE / Work 积分的口径；额度见底时提前把账号
	// 降温，避免后续请求都撞"额度不足"白烧风控额度。
	if res.HasRemain {
		h.traeNoteRemain(st.uid, res.RemainIDE, res.RemainWork)
		h.traeStats.NoteRemain(st.uid, res.RemainIDE, res.RemainWork)
	}
	h.traeRecordUsage(st.uid, model, res.Usage)
	return res
}

// deltaFromStream 从流式转换结果提取用量增量（供 trae.Stats 记账）。
//
// 上游 token_usage 的字段可能是 float64 / json.Number / 字符串，统一走
// usageInt 宽容取值；缺失的字段不参与累计（Has* 为假）。
//
// ttfb / elapsed 由调用方计时（StreamResult 只装上游给的数据，不含本地时钟）。
func deltaFromStream(res trae.StreamResult, ttfb, elapsed time.Duration) trae.UsageDelta {
	var d trae.UsageDelta
	if ttfb > 0 {
		d.HasLatencyMs, d.LatencyMs = true, int64(ttfb/time.Millisecond)
	}
	u := res.Usage
	if u == nil {
		return d
	}
	if v, ok := usageInt(u["prompt_tokens"]); ok {
		d.HasPromptTokens, d.PromptTokens = true, v
	}
	if v, ok := usageInt(u["completion_tokens"]); ok {
		d.HasCompletionTokens, d.CompletionTokens = true, v
	}
	if v, ok := usageInt(u["total_tokens"]); ok {
		d.HasTotalTokens, d.TotalTokens = true, v
	}
	// 吞吐只在「有完成 token 且生成期有效」时才有意义。
	// 生成期 = 总耗时 - TTFB：把这算进分母会把等首字节的时间摊薄成"更快"，
	// 首字节前的时间与解码速度无关。生成期过短（<100ms，常见于假上游/极短回复）
	// 时除出来的数会荒谬地大，宁可不记。
	gen := elapsed - ttfb
	if d.HasCompletionTokens && d.CompletionTokens > 0 && gen >= 100*time.Millisecond {
		d.HasTokensPerSecond = true
		d.TokensPerSecond = float64(d.CompletionTokens) / gen.Seconds()
	}
	return d
}

// deltaFromResponse 从聚合后的 chat.completion 提取用量增量。
//
// 这里显式传 model（而不是从 resp["model"] 里取）：payload 会改写模型名，
// 面板要展示的是客户端请求的那个名字，两者可能不同。
func deltaFromResponse(resp map[string]any, model string) trae.UsageDelta {
	d := trae.UsageDelta{Model: model}
	u, _ := resp["usage"].(map[string]any)
	if u == nil {
		return d
	}
	if v, ok := usageInt(u["prompt_tokens"]); ok {
		d.HasPromptTokens, d.PromptTokens = true, v
	}
	if v, ok := usageInt(u["completion_tokens"]); ok {
		d.HasCompletionTokens, d.CompletionTokens = true, v
	}
	if v, ok := usageInt(u["total_tokens"]); ok {
		d.HasTotalTokens, d.TotalTokens = true, v
	}
	return d
}

// usageInt 宽容取整（上游可能给 float64 / json.Number / 字符串数字）。
func usageInt(v any) (int64, bool) {
	switch n := v.(type) {
	case float64:
		return int64(n), true
	case int64:
		return n, true
	case int:
		return int64(n), true
	case json.Number:
		i, err := n.Int64()
		return i, err == nil
	case string:
		if i, err := strconv.ParseInt(strings.TrimSpace(n), 10, 64); err == nil {
			return i, true
		}
	}
	return 0, false
}

// traeRemainFloor IDE 积分低于该值时提前冷却账号。
const traeRemainFloor = 50

// traeNoteRemain 按 notify_usage 的余额决定是否提前冷却。
func (h *Handler) traeNoteRemain(uid string, ideCredits, workCredits float64) {
	if uid == "" {
		return
	}
	if ideCredits > 0 && ideCredits < traeRemainFloor {
		h.traeCool.Mark(uid, 30*time.Minute, trae.ErrPlanLimit,
			fmt.Sprintf("IDE 积分仅剩 %.0f（Work 剩余 %.0f）", ideCredits, workCredits))
		log.Printf("[trae] 账号 %s IDE 积分见底（%.0f），冷却 30 分钟", uid, ideCredits)
	}
}

// traeResponses 处理 POST /trae/v1/responses：Responses → Chat → llm_utils_chat，
// 回程复用 responsesWriter 投影回 Responses。
func (h *Handler) traeResponses(w http.ResponseWriter, r *http.Request) {
	if !h.traeEnabled() {
		writeResponsesError(w, http.StatusNotFound, "not_found", "trae upstream is disabled")
		return
	}
	cfg := h.traeConfig()
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		writeResponsesError(w, http.StatusBadRequest, "invalid_request", "read body: "+err.Error())
		return
	}
	var peek struct {
		Model  string `json:"model"`
		Stream bool   `json:"stream"`
	}
	_ = json.Unmarshal(raw, &peek)

	chatBody, toolCtx, err := responses.ToChat(raw)
	if err != nil {
		writeResponsesError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}

	st := newChatStat(time.Now(), raw, peek.Stream)
	defer st.done()

	attempt := h.traeOpenWithRotate(r.Context(), cfg, func(acc *trae.Account) ([]byte, error) {
		return trae.PrepareChatBody(chatBody, trae.PrepareOptions{
			UID:            acc.UID,
			DeviceID:       acc.DeviceID,
			MachineID:      acc.MachineID,
			IDEVersion:     cfg.IDEVersion,
			IDEVersionCode: cfg.IDEVersionCode,
			AppID:          cfg.AppID,
			Function:       h.traeFunctionFor(cfg, acc, peek.Model),
		})
	})
	acc := attempt.acc
	if acc == nil {
		st.status = http.StatusServiceUnavailable
		writeResponsesError(w, http.StatusServiceUnavailable, "no_healthy_account",
			"no usable trae account (check trae.json)")
		return
	}
	st.uid = acc.UID
	st.nick = acc.Name

	finishStat := h.traeStats.Begin(acc.UID)
	ok := false
	var statDelta trae.UsageDelta
	defer func() { finishStat(ok, statDelta) }()

	if attempt.err != nil {
		st.status = http.StatusBadGateway
		writeResponsesError(w, http.StatusBadGateway, "upstream_error", attempt.err.Error())
		return
	}
	if attempt.status >= 400 {
		st.status = attempt.status
		writeResponsesError(w, attempt.status, traeErrorCode(attempt.status), upstreamText(attempt.body))
		return
	}
	rc := attempt.rc
	defer rc.Close()

	peeked, perr := trae.PeekStream(rc)
	if perr != nil {
		st.status = http.StatusBadGateway
		writeResponsesError(w, http.StatusBadGateway, "upstream_error", perr.Error())
		return
	}
	if peeked.Error != nil {
		cls := trae.Classify(0, nil, peeked.Error.Code, peeked.Error.Message)
		h.traeApplyPolicy(acc.UID, cls)
		status := cls.HTTPStatus()
		st.status = status
		writeResponsesError(w, status, traeErrorCode(status),
			pickNonEmpty(cls.Message, peeked.Error.Message))
		return
	}
	rc = io.NopCloser(io.MultiReader(bytes.NewReader(peeked.Header), rc))

	rw := newResponsesWriter(w, toolCtx)
	chatID := "chatcmpl-trae-" + shortID()
	if peek.Stream {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		st.status = http.StatusOK
		streamStart := time.Now()
		res, _ := trae.ConvertStream(rc, chatID, peek.Model, func(b []byte) error {
			if len(b) == 0 {
				return nil
			}
			if _, werr := rw.Write(b); werr != nil {
				return werr
			}
			rw.Flush()
			return nil
		})
		rw.finish()
		if res.HasRemain {
			h.traeStats.NoteRemain(acc.UID, res.RemainIDE, res.RemainWork)
		}
		ok = res.ErrorCode == 0 && res.SentAny
		statDelta = deltaFromStream(res, st.ttfb, time.Since(streamStart))
		return
	}
	resp, streamErr, err := trae.Aggregate(rc, chatID, peek.Model)
	if err != nil {
		st.status = http.StatusBadGateway
		writeResponsesError(w, http.StatusBadGateway, "upstream_parse", err.Error())
		return
	}
	if streamErr != nil {
		h.traeApplyPolicy(acc.UID, trae.Classify(0, nil, streamErr.Code, streamErr.Message))
		st.status = http.StatusBadGateway
		writeResponsesError(w, http.StatusBadGateway, "upstream_error", streamErr.Message)
		return
	}
	h.traeCool.Clear(acc.UID)
	st.status = http.StatusOK
	st.toks = completionTokens(resp)
	ok = true
	statDelta = deltaFromResponse(resp, peek.Model)
	b, _ := json.Marshal(resp)
	w.Header().Set("Content-Type", "application/json")
	if _, err := rw.Write(b); err != nil {
		st.status = http.StatusBadGateway
		return
	}
	rw.finish()
	h.traeRecord(acc.UID, peek.Model, resp)
}

// traeModels 处理 GET /trae/v1/models：返回配置内置的模型目录。
//
// 说明：Trae 官方目录来自 /api/ide/v1/get_detail_param（需有效账号），
// 阶段一先用内置表（与 payload.go 的模型映射同源），避免无账号时列表为空。
func (h *Handler) traeModels(w http.ResponseWriter, r *http.Request) {
	if !h.traeEnabled() {
		writeOpenAIError(w, http.StatusNotFound, "not_found", "trae upstream is disabled")
		return
	}
	cfg := h.traeConfig()
	// 优先用上游目录：新增模型免发版，且能拿到真实的 output/context 上限。
	// 拉取失败自动回落到内置目录（FetchCatalog 内部处理）。
	var cat *trae.Catalog
	if acc := traePickAccount(cfg, h.traeCool); acc != nil {
		cat = h.cfg.TraeClient.FetchCatalog(r.Context(), cfg, acc, r.URL.Query().Get("refresh") == "1")
	}
	if cat == nil {
		cat = &trae.Catalog{}
		for _, id := range trae.KnownModels() {
			cat.Models = append(cat.Models, trae.CatalogModel{ID: id, DisplayName: id})
		}
	}
	data := make([]map[string]any, 0, len(cat.Models))
	for _, m := range cat.Models {
		item := map[string]any{
			"id":       m.ID,
			"object":   "model",
			"created":  1753600000,
			"owned_by": "trae",
		}
		if m.DisplayName != "" && m.DisplayName != m.ID {
			item["display_name"] = m.DisplayName
		}
		if m.MaxTokens > 0 {
			item["max_output_tokens"] = m.MaxTokens
		}
		if m.PromptMaxTokens > 0 {
			item["context_window"] = m.PromptMaxTokens
		}
		data = append(data, item)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"object": "list",
		"data":   data,
		// live=false 表示拉取失败、返回的是内置目录。
		"live": cat.Live,
	})
}

// traeRecord 非流式路径的用量记账（独立 realm="trae" 分账）。
func (h *Handler) traeRecord(uid, model string, resp map[string]any) {
	u, _ := resp["usage"].(map[string]any)
	h.traeRecordUsage(uid, model, u)
}

// traeRecordUsage 写入用量时序：realm 固定 "trae"、uid 用 Trae 账号 uid。
func (h *Handler) traeRecordUsage(uid, model string, u map[string]any) {
	if h.cfg.Usage == nil || u == nil {
		return
	}
	pt := int64(0)
	ct := int64(0)
	tt := int64(0)
	if v, ok := u["prompt_tokens"].(float64); ok {
		pt = int64(v)
	}
	if v, ok := u["completion_tokens"].(float64); ok {
		ct = int64(v)
	}
	if v, ok := u["total_tokens"].(float64); ok {
		tt = int64(v)
	}
	h.cfg.Usage.Add(time.Now(), "trae", uid, model, usage.Delta{
		PromptTokens:     pt,
		HasPromptTokens:  pt > 0,
		CompletionTokens: ct,
		HasCompletion:    ct > 0,
		TotalTokens:      tt,
		HasTotal:         tt > 0,
	}, tt > 0 || ct > 0 || pt > 0)
}

// traeErrorCode Trae 上游 HTTP 状态 → 稳定 code（与 thirdparty 同口径）。
func traeErrorCode(status int) string {
	switch status {
	case http.StatusUnauthorized:
		return "invalid_api_key"
	case http.StatusForbidden:
		return "permission_denied"
	case http.StatusNotFound:
		return "model_not_found"
	case http.StatusTooManyRequests:
		return "rate_limit_exceeded"
	case http.StatusRequestTimeout, http.StatusGatewayTimeout:
		return "upstream_timeout"
	default:
		if status >= 500 {
			return "upstream_error"
		}
		return "invalid_request"
	}
}

// upstreamText 上游错误体转可读文本（原文优先，空体给兜底）。
// pickNonEmpty 返回第一个非空串。
func pickNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func upstreamText(body []byte) string {
	s := strings.TrimSpace(string(body))
	if s == "" {
		return "trae upstream returned an empty error body"
	}
	return s
}

// shortID 生成本请求的短随机 id（拼 chatcmpl 前缀用）。
func shortID() string {
	s, err := trae.RandomHex(4)
	if err != nil {
		return "x"
	}
	return s
}

// traeChatAttempt 一次 chat 尝试的结果。
type traeChatAttempt struct {
	// acc 本次使用的账号。
	acc *trae.Account
	// rc 上游流（成功时非 nil，调用方负责关闭）。
	rc io.ReadCloser
	// status HTTP 状态（>=400 表示失败）。
	status int
	// body 失败响应体（用于分类）。
	body []byte
	// err 传输层错误（网络等）。
	err error
	// cls 失败分类（成功时为 ErrNone）。
	cls trae.Classification
}

// traeOpenWithRotate 带换号重试地打开上游流。
//
// 换号规则（对齐 dsh 的 FAILOVER_KINDS）：
//   - 只换 ShouldRotate() 为真的错误（凭证/额度/限流/5xx/风控）；
//   - 模型不可用（4001/4023）与请求本身有问题（4xx）**不换号**——换号只会
//     把同一个错误烧两遍，还白白消耗别的账号的风控额度；
//   - 最多 trae.MaxRotate 次，且不重复用同一个账号。
//
// 返回的 attempt 里 status/err/cls 描述**最后一次**失败；成功时 rc 非 nil。
// 调用方负责关闭 rc。
func (h *Handler) traeOpenWithRotate(ctx context.Context, cfg *trae.Config,
	prepare func(*trae.Account) ([]byte, error)) traeChatAttempt {

	tried := make(map[string]bool, len(cfg.Accounts))
	var last traeChatAttempt

	for i := 0; i < trae.MaxRotate; i++ {
		acc := traePickAccountExcluding(cfg, h.traeCool, tried)
		if acc == nil {
			if last.err == nil && last.status == 0 {
				last.err = fmt.Errorf("no usable trae account")
			}
			return last
		}
		tried[acc.UID] = true

		body, err := prepare(acc)
		if err != nil {
			// 请求体构造失败与账号无关，换号也没用。
			last = traeChatAttempt{acc: acc, err: err}
			return last
		}

		rc, status, respBody, err := h.cfg.TraeClient.ChatStream(ctx, cfg, acc, body)
		if err != nil {
			last = traeChatAttempt{acc: acc, err: err}
			// 传输层错误（超时/连接）视为上游不可用：换号可能有用。
			if i < trae.MaxRotate-1 {
				log.Printf("WARN: [trae] 账号 %s 请求失败，尝试换号: %v", acc.UID, err)
				continue
			}
			return last
		}
		if status < 400 {
			return traeChatAttempt{acc: acc, rc: rc, status: status}
		}

		cls := trae.Classify(status, respBody, 0, "")
		last = traeChatAttempt{acc: acc, status: status, body: respBody, cls: cls}
		h.traeApplyPolicy(acc.UID, cls)
		if !cls.ShouldRotate() || i == trae.MaxRotate-1 {
			return last
		}
		log.Printf("[trae] 账号 %s 失败（%s），换号重试", acc.UID, cls.Kind)
	}
	return last
}

// traeFunctionFor 查模型对应的 function（以上游目录为准）。
//
// 目录查询本身是一次上游请求，所以失败/超时都静默回落到硬编码规则——
// function 选错的代价是 4001，而查目录失败不该拖慢正常请求。
func (h *Handler) traeFunctionFor(cfg *trae.Config, acc *trae.Account, model string) string {
	model = strings.TrimSpace(model)
	if model == "" {
		return ""
	}
	cat := h.cfg.TraeClient.FetchCatalog(context.Background(), cfg, acc, false)
	if cat == nil || !cat.Live {
		return "" // 内置目录的 function 与硬编码规则一致，回落即可
	}
	// 目录里的 id 是 wire 名（不带 __dev），客户端可能传显示名或带后缀。
	id := strings.TrimSuffix(model, "__dev")
	if fn := cat.FunctionForConfigName(id); fn != "" {
		return fn
	}
	// 再按展示名匹配一次（大小写不敏感）。
	lower := strings.ToLower(id)
	for _, m := range cat.Models {
		if strings.ToLower(m.DisplayName) == lower {
			return m.Function
		}
	}
	return ""
}

// traeQueueFloor 排队位置超过该值时给账号短冷却。
//
// chat_v3 通用池实测常 1000+，solo 池较轻。排队深意味着这个号（或这个
// function）当下不划算，短冷却能让轮转把后续请求分给更空闲的账号。
const traeQueueFloor = 500

// traeNoteQueue 按排队深度决定是否短冷却。
func (h *Handler) traeNoteQueue(uid string, position int64) {
	if uid == "" || position < traeQueueFloor {
		return
	}
	h.traeCool.Mark(uid, 2*time.Minute, trae.ErrSoftRate,
		fmt.Sprintf("排队 %d 位，短暂让出", position))
	log.Printf("[trae] 账号 %s 排队 %d 位，冷却 2 分钟", uid, position)
}
