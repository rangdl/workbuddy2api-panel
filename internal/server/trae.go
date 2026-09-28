package server

import (
	"encoding/json"
	"io"
	"log"
	"net/http"
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
	usable := make([]trae.Account, 0, len(cfg.Accounts))
	for _, a := range cfg.Accounts {
		if a.Disabled || a.AccessToken == "" {
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

	acc := traePickAccount(cfg, h.traeCool)
	if acc == nil {
		writeOpenAIError(w, http.StatusServiceUnavailable, "no_healthy_account",
			"no usable trae account (check trae.json)")
		return
	}

	st := newChatStat(time.Now(), body, peek.Stream)
	defer st.done()
	st.uid = acc.UID
	st.nick = acc.Name

	chatBody, err := trae.PrepareChatBody(body, trae.PrepareOptions{
		UID:            acc.UID,
		DeviceID:       acc.DeviceID,
		MachineID:      acc.MachineID,
		IDEVersion:     cfg.IDEVersion,
		IDEVersionCode: cfg.IDEVersionCode,
		AppID:          cfg.AppID,
	})
	if err != nil {
		st.status = http.StatusBadRequest
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}

	rc, status, respBody, err := h.cfg.TraeClient.ChatStream(r.Context(), cfg, acc, chatBody)
	if err != nil {
		st.status = http.StatusBadGateway
		writeOpenAIError(w, http.StatusBadGateway, "upstream_error", err.Error())
		return
	}
	if status >= 400 {
		cls := trae.Classify(status, respBody, 0, "")
		h.traeApplyPolicy(acc.UID, cls)
		st.status = status
		writeOpenAIError(w, status, traeErrorCode(status), upstreamText(respBody))
		return
	}
	defer rc.Close()

	chatID := "chatcmpl-trae-" + shortID()
	if peek.Stream {
		h.traeForwardStream(w, rc, chatID, peek.Model, st)
		return
	}
	resp, streamErr, err := trae.Aggregate(rc, chatID, peek.Model)
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
	h.traeRecord(acc.UID, peek.Model, resp)
	writeJSON(w, http.StatusOK, resp)
}

// traeForwardStream 把 SOLO SSE 转成 OpenAI SSE 边转边发。
func (h *Handler) traeForwardStream(w http.ResponseWriter, rc io.Reader, chatID, model string, st *chatStat) {
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
		return
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
	h.traeRecordUsage(st.uid, model, res.Usage)
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

	acc := traePickAccount(cfg, h.traeCool)
	if acc == nil {
		writeResponsesError(w, http.StatusServiceUnavailable, "no_healthy_account",
			"no usable trae account (check trae.json)")
		return
	}

	chatBody, toolCtx, err := responses.ToChat(raw)
	if err != nil {
		writeResponsesError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	prepared, err := trae.PrepareChatBody(chatBody, trae.PrepareOptions{
		UID:            acc.UID,
		DeviceID:       acc.DeviceID,
		MachineID:      acc.MachineID,
		IDEVersion:     cfg.IDEVersion,
		IDEVersionCode: cfg.IDEVersionCode,
		AppID:          cfg.AppID,
	})
	if err != nil {
		writeResponsesError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}

	st := newChatStat(time.Now(), prepared, peek.Stream)
	defer st.done()
	st.uid = acc.UID
	st.nick = acc.Name

	rc, status, respBody, err := h.cfg.TraeClient.ChatStream(r.Context(), cfg, acc, prepared)
	if err != nil {
		st.status = http.StatusBadGateway
		writeResponsesError(w, http.StatusBadGateway, "upstream_error", err.Error())
		return
	}
	if status >= 400 {
		h.traeApplyPolicy(acc.UID, trae.Classify(status, respBody, 0, ""))
		st.status = status
		writeResponsesError(w, status, traeErrorCode(status), upstreamText(respBody))
		return
	}
	defer rc.Close()

	rw := newResponsesWriter(w, toolCtx)
	chatID := "chatcmpl-trae-" + shortID()
	if peek.Stream {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		st.status = http.StatusOK
		_, _ = trae.ConvertStream(rc, chatID, peek.Model, func(b []byte) error {
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
	data := make([]map[string]any, 0, len(trae.KnownModels()))
	for _, m := range trae.KnownModels() {
		data = append(data, map[string]any{
			"id":       m,
			"object":   "model",
			"created":  1753600000,
			"owned_by": "trae",
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": data})
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
