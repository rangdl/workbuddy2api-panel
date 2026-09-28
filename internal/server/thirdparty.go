package server

import (
	"encoding/json"
	"io"
	"net/http"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/responses"
	"github.com/linguo2625469/workbuddy2api-panel/internal/thirdparty"
	"github.com/linguo2625469/workbuddy2api-panel/internal/usage"
)

// 本文件实现第三方 OpenAI 兼容上游的三个端点（/tp/v1/*），与 CodeBuddy 的 /v1/*
// 完全隔离——第三方请求不进入账号池、不做冷却/熔断/粘性，也不套用 CodeBuddy 的
// 提示词改写与指纹脱敏（第三方上游是标准协议，多做的改写只会制造非法参数）。
//
// 复用面（不改一行）：鉴权 withAuth、请求流水日志（chatStat/chatStatsReader）、
// 用量记账（usage.Recorder，独立 realm="tp" 分账）、Responses ⇄ Chat 转换
// （internal/responses + 同包 responsesWriter）。

// thirdpartyConfig 返回当前生效的第三方配置（面板热重载后即为新值；可能为 nil）。
func (h *Handler) thirdpartyConfig() *thirdparty.Config {
	return h.thirdpartyCfg.Load()
}

// thirdpartyEnabled 第三方是否可用：配置存在、已启用、且转发客户端已注入。
func (h *Handler) thirdpartyEnabled() bool {
	cfg := h.thirdpartyConfig()
	return cfg != nil && cfg.Enabled && h.cfg.ThirdPartyClient != nil
}

// thirdpartyChat 处理 POST /tp/v1/chat/completions：Chat → Chat 转发。
//
// 出站 stream 跟随客户端（第三方上游原生支持非流式，无需像 CodeBuddy 那样
// 强制 stream:true 再本地聚合）。
func (h *Handler) thirdpartyChat(w http.ResponseWriter, r *http.Request) {
	if !h.thirdpartyEnabled() {
		writeOpenAIError(w, http.StatusNotFound, "not_found", "third-party upstream is disabled")
		return
	}
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

	p := h.thirdpartyConfig().ProviderForModel(peek.Model)
	if p == nil {
		writeOpenAIError(w, http.StatusNotFound, "model_not_found",
			"no third-party provider serves model "+peek.Model+" (check third_party.json)")
		return
	}

	st := newChatStat(time.Now(), body, peek.Stream)
	defer st.done()
	st.uid = p.Name
	st.nick = p.Name

	rc, status, respBody, err := h.cfg.ThirdPartyClient.Chat(r.Context(), p, body, peek.Stream)
	if err != nil {
		st.status = http.StatusBadGateway
		writeOpenAIError(w, http.StatusBadGateway, "upstream_error", err.Error())
		return
	}
	if status >= 400 {
		shape := thirdparty.Classify(status, respBody)
		st.status = shape.Status
		writeOpenAIError(w, shape.Status, shape.Code, shape.Message)
		return
	}

	if peek.Stream {
		defer rc.Close()
		h.thirdpartyForwardStream(w, rc, st)
		return
	}
	// 非流式：Chat 已读全响应体并关闭连接，rc 为 nil（勿再 Close）。
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(respBody)
	st.status = status
	st.toks = h.thirdpartyRecord(peek.Model, p.Name, respBody, time.Since(st.start))
}

// thirdpartyForwardStream 流式透传第三方 SSE：原样转发（不做帧重建——第三方
// 上游本就是标准 OpenAI SSE），同时经 chatStatsReader 采集 TTFB 与 usage。
func (h *Handler) thirdpartyForwardStream(w http.ResponseWriter, rc io.Reader, st *chatStat) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	st.status = http.StatusOK

	ctrl := http.NewResponseController(w)
	stats := newChatStatsReaderSince(rc, st.start)
	buf := make([]byte, 8192)
	for {
		n, err := stats.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				break // 客户端断连：停止读取，上游 body 由调用方 defer Close
			}
			_ = ctrl.Flush()
		}
		if err != nil {
			break
		}
	}
	st.ttfb = stats.TTFB()
	if toks, ok := stats.Tokens(); ok {
		st.toks = toks
	}
	delta := stats.Usage()
	h.thirdpartyRecordDelta(st.model, st.nick, delta, time.Since(st.start))
}

// thirdpartyResponses 处理 POST /tp/v1/responses：Responses → Chat 转换后转发，
// 回程复用 responsesWriter（Chat SSE → Responses SSE / Chat JSON → Responses 对象）。
//
// 与 CodeBuddy 侧 h.responses 的差异：中间那段「内部调用 h.chatCompletions」
// 换成「直连第三方上游」——协议转换、增量补全、错误形状全部照旧。
func (h *Handler) thirdpartyResponses(w http.ResponseWriter, r *http.Request) {
	if !h.thirdpartyEnabled() {
		writeResponsesError(w, http.StatusNotFound, "not_found", "third-party upstream is disabled")
		return
	}
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

	p := h.thirdpartyConfig().ProviderForModel(peek.Model)
	if p == nil {
		writeResponsesError(w, http.StatusNotFound, "model_not_found",
			"no third-party provider serves model "+peek.Model+" (check third_party.json)")
		return
	}

	// Responses → Chat（含工具上下文，供回程还原）。
	chatBody, toolCtx, err := responses.ToChat(raw)
	if err != nil {
		writeResponsesError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}

	st := newChatStat(time.Now(), chatBody, peek.Stream)
	defer st.done()
	st.uid = p.Name
	st.nick = p.Name

	rc, status, respBody, err := h.cfg.ThirdPartyClient.Chat(r.Context(), p, chatBody, peek.Stream)
	if err != nil {
		st.status = http.StatusBadGateway
		writeResponsesError(w, http.StatusBadGateway, "upstream_error", err.Error())
		return
	}
	if status >= 400 {
		shape := thirdparty.Classify(status, respBody)
		st.status = shape.Status
		writeResponsesError(w, shape.Status, shape.Code, shape.Message)
		return
	}

	// responsesWriter 靠 dst 的 Content-Type 判定流式/非流式，故须在写入前设定。
	rw := newResponsesWriter(w, toolCtx)
	if peek.Stream {
		defer rc.Close()
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		st.status = http.StatusOK
		stats := newChatStatsReaderSince(rc, st.start)
		buf := make([]byte, 8192)
		for {
			n, rerr := stats.Read(buf)
			if n > 0 {
				if _, werr := rw.Write(buf[:n]); werr != nil {
					break
				}
				rw.Flush()
			}
			if rerr != nil {
				break
			}
		}
		st.ttfb = stats.TTFB()
		if toks, ok := stats.Tokens(); ok {
			st.toks = toks
		}
		rw.finish()
		h.thirdpartyRecordDelta(st.model, p.Name, stats.Usage(), time.Since(st.start))
		return
	}

	// 非流式：Chat 已读全响应体并关闭连接，rc 为 nil（勿再 Close）。
	w.Header().Set("Content-Type", "application/json")
	st.status = http.StatusOK
	if _, err := rw.Write(respBody); err != nil {
		st.status = http.StatusBadGateway
		return
	}
	rw.finish()
	st.toks = h.thirdpartyRecord(peek.Model, p.Name, respBody, time.Since(st.start))
}

// thirdpartyModels 处理 GET /tp/v1/models：列出配置中声明的第三方模型。
// 兜底 provider（models 为空）不贡献条目——它承接任意模型名，无法枚举。
func (h *Handler) thirdpartyModels(w http.ResponseWriter, r *http.Request) {
	if !h.thirdpartyEnabled() {
		writeOpenAIError(w, http.StatusNotFound, "not_found", "third-party upstream is disabled")
		return
	}
	models := h.thirdpartyConfig().AllModels()
	data := make([]map[string]any, 0, len(models))
	for _, m := range models {
		data = append(data, map[string]any{
			"id":       m,
			"object":   "model",
			"created":  1753600000,
			"owned_by": "thirdparty",
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": data})
}

// thirdpartyRecord 非流式路径的用量记账；返回 completion_tokens（-1 = 未观测到，
// 与 chatStat 的「-1 = 缺失」哨兵一致，不伪造 0）。响应非 JSON 时只记账不取 token。
func (h *Handler) thirdpartyRecord(model, provider string, respBody []byte, elapsed time.Duration) int {
	var resp map[string]any
	if json.Unmarshal(respBody, &resp) != nil {
		return -1
	}
	h.thirdpartyRecordDelta(model, provider, usageDeltaFromResponse(resp), elapsed)
	return completionTokens(resp)
}

// thirdpartyRecordDelta 写入用量时序：realm 固定 "tp"、uid 用 provider 名，
// 使面板「用量」视图能把第三方流量与 CodeBuddy 账号分账显示（不污染账号维度）。
func (h *Handler) thirdpartyRecordDelta(model, provider string, d pool.TokenUsageDelta, elapsed time.Duration) {
	if h.cfg.Usage == nil {
		return
	}
	ms := elapsed.Milliseconds()
	if ms < 1 {
		ms = 1
	}
	tps := 0.0
	hasTPS := false
	if d.HasCompletionTokens && d.CompletionTokens > 0 {
		tps = float64(d.CompletionTokens) * 1000 / float64(ms)
		hasTPS = true
	}
	h.cfg.Usage.Add(time.Now(), "tp", provider, model, usage.Delta{
		PromptTokens:     d.PromptTokens,
		HasPromptTokens:  d.HasPromptTokens,
		CompletionTokens: d.CompletionTokens,
		HasCompletion:    d.HasCompletionTokens,
		TotalTokens:      d.TotalTokens,
		HasTotal:         d.HasTotalTokens,
		LatencyMs:        ms,
		HasLatency:       true,
		TokensPerSecond:  tps,
		HasTPS:           hasTPS,
	}, d.HasTotalTokens || d.HasCompletionTokens || d.HasPromptTokens)
}
