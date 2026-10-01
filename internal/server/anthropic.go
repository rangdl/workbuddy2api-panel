package server

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync/atomic"

	"github.com/linguo2625469/workbuddy2api-panel/internal/anthropic"
)

// AnthropicConfig Anthropic Messages 端点配置（由 cmd/server 从独立文件构造后注入，
// 模式与 ResponsesConfig 一致）。
type AnthropicConfig struct {
	// Enabled 是否响应 /v1/messages（运行时可热切换）。
	Enabled bool
	// ModelMap Anthropic 客户端模型名 → 上游模型名。
	ModelMap map[string]string
	// DefaultModel 未命中 ModelMap 时的回落模型（空 = 不改写）。
	DefaultModel string
}

// anthropicMessages 处理 POST /v1/messages：把 Anthropic Messages 请求转换为 Chat
// 请求，内部复用既有 chatCompletions 链路（鉴权/轮转/冷却/粘性/日志），再用包装
// Writer 把出口转换为 Anthropic 形态。零改动 chatCompletions。
//
// 与 h.responses（Codex）的编排同构；差异点：
//   - 鉴权走 VerifyAnthropic（x-api-key 优先，Bearer 回落）——CC 客户端不发 Authorization；
//   - 非流式是 CC 的常态路径（claude -p 等场景 stream=false），Chat 上游强制 stream:true
//     后由 anthropicWriter 的非流式分支聚合转换（复用 responsesWriter 的聚合思路）。
func (h *Handler) anthropicMessages(w http.ResponseWriter, r *http.Request) {
	cfg := h.anthropicCfg.Load()
	if cfg == nil || !cfg.Enabled {
		writeAnthropicError(w, http.StatusNotFound, "not_found_error", "anthropic endpoint is disabled")
		return
	}
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", "read body: "+err.Error())
		return
	}

	// 1. Anthropic → Chat。
	chatBody, err := anthropic.ToChat(raw)
	if err != nil {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}

	// 2. 模型名映射（照抄 applyResponsesModelMap 的口径）。
	chatBody = applyAnthropicModelMap(chatBody, cfg)

	// 3. 构造等价 Chat 请求（复制 header，替换 body），走既有链路。
	nr := r.Clone(r.Context())
	nr.Body = io.NopCloser(bytes.NewReader(chatBody))
	nr.ContentLength = int64(len(chatBody))
	nr.Header = r.Header.Clone()
	nr.Header.Set("Content-Type", "application/json")

	aw := newAnthropicWriter(w)
	h.chatCompletions(aw, nr)
	aw.finish()
}

// applyAnthropicModelMap 按配置改写请求体 model（照抄 applyResponsesModelMap）。
func applyAnthropicModelMap(body []byte, cfg *AnthropicConfig) []byte {
	if cfg == nil || (len(cfg.ModelMap) == 0 && cfg.DefaultModel == "") {
		return body
	}
	var req map[string]any
	if json.Unmarshal(body, &req) != nil {
		return body
	}
	model, _ := req["model"].(string)
	mapped := ""
	if m, ok := cfg.ModelMap[model]; ok {
		mapped = m
	} else if cfg.DefaultModel != "" {
		mapped = cfg.DefaultModel
	}
	if mapped == "" || mapped == model {
		return body
	}
	req["model"] = mapped
	out, err := json.Marshal(req)
	if err != nil {
		return body
	}
	return out
}

// writeAnthropicError 写 Anthropic 错误形状（{"type":"error","error":{...}}）。
func writeAnthropicError(w http.ResponseWriter, status int, typ, msg string) {
	writeJSON(w, status, map[string]any{
		"type":  "error",
		"error": map[string]any{"type": typ, "message": msg},
	})
}

// anthropicWriter 包装 http.ResponseWriter，把 chatCompletions 的出口（Chat JSON 或
// Chat SSE）转换为 Anthropic 形态。模式照抄 responsesWriter：流式实时转换，
// 非流式缓冲后一次转换。
type anthropicWriter struct {
	dst http.ResponseWriter

	status     int
	headerSent bool
	isSSE      *bool

	sseState  *anthropic.StreamState
	streamBuf bytes.Buffer

	bodyBuf bytes.Buffer
}

func newAnthropicWriter(dst http.ResponseWriter) *anthropicWriter {
	return &anthropicWriter{dst: dst, sseState: anthropic.NewStreamState()}
}

func (w *anthropicWriter) Header() http.Header { return w.dst.Header() }

func (w *anthropicWriter) WriteHeader(code int) { w.status = code }

func (w *anthropicWriter) Write(p []byte) (int, error) {
	if w.isSSE == nil {
		w.detectMode()
	}
	if *w.isSSE {
		return w.writeSSE(p)
	}
	return w.bodyBuf.Write(p)
}

// writeSSE 逐块解析 Chat SSE 并实时转换为 Anthropic SSE（照抄 responsesWriter.writeSSE）。
func (w *anthropicWriter) writeSSE(p []byte) (int, error) {
	w.streamBuf.Write(p)
	for {
		block, ok := takeSSEBlock(&w.streamBuf)
		if !ok {
			break
		}
		if err := w.handleSSEBlock(block); err != nil {
			return 0, err
		}
	}
	return len(p), nil
}

func (w *anthropicWriter) handleSSEBlock(block string) error {
	data := extractDataLine(block)
	if data == "" {
		return nil
	}
	if data == "[DONE]" {
		return w.writeOut(w.sseState.Finalize())
	}
	var chunk map[string]any
	if json.Unmarshal([]byte(data), &chunk) != nil {
		return nil
	}
	if errObj, ok := chunk["error"].(map[string]any); ok {
		msg, _ := errObj["message"].(string)
		// 上游错误：发规范流内 error 事件（含上游原文）+ message_delta/message_stop
		// 收尾——CC 收到 error 事件会向用户展示错误并停止重试解析，而不是把
		// 这个回合渲染成 completed-but-empty 的空消息（A5 增强落地）。
		return w.writeOut(w.sseState.FailedWithMessage(msg, "api_error"))
	}
	return w.writeOut(w.sseState.HandleChunk(chunk))
}

// Flush 实现 http.Flusher：流式下把已产生的 Anthropic 事件刷给客户端。
func (w *anthropicWriter) Flush() {
	w.flushHeader()
	if fl, ok := w.dst.(http.Flusher); ok {
		fl.Flush()
	}
}

func (w *anthropicWriter) detectMode() {
	isSSE := strings.Contains(w.dst.Header().Get("Content-Type"), "text/event-stream")
	w.isSSE = &isSSE
	if isSSE {
		w.flushHeader()
	}
}

func (w *anthropicWriter) flushHeader() {
	if w.headerSent {
		return
	}
	w.headerSent = true
	code := w.status
	if code == 0 {
		code = http.StatusOK
	}
	w.dst.WriteHeader(code)
}

// finish 在 chatCompletions 返回后调用：流式补收尾事件；非流式做整体转换。
func (w *anthropicWriter) finish() {
	if w.isSSE != nil && *w.isSSE {
		// 上游未发 [DONE] 就断流（异常终止）：Finalize 幂等，补收尾。
		_ = w.writeOut(w.sseState.Finalize())
		return
	}
	w.finalizeNonStream()
}

func (w *anthropicWriter) writeOut(b []byte) error {
	if len(b) == 0 {
		return nil
	}
	w.flushHeader()
	if _, err := w.dst.Write(b); err != nil {
		return err
	}
	if fl, ok := w.dst.(http.Flusher); ok {
		fl.Flush()
	}
	return nil
}

func (w *anthropicWriter) finalizeNonStream() {
	status := w.status
	if status == 0 {
		status = http.StatusOK
	}
	body := w.bodyBuf.Bytes()

	var parsed map[string]any
	if json.Unmarshal(body, &parsed) != nil {
		// 非 JSON 200 体按 502 处理：把上游原文塞进 200 会让 CC 当合法消息解析失败。
		respStatus := status
		if respStatus < 400 {
			respStatus = http.StatusBadGateway
		}
		writeAnthropicError(w.dst, respStatus, "api_error", string(body))
		return
	}
	if _, hasErr := parsed["error"]; hasErr {
		w.flushHeader()
		writeJSON(w.dst, status, anthropic.ErrorToMessages(body, status))
		return
	}
	msg, err := anthropic.FromChat(parsed)
	if err != nil {
		writeAnthropicError(w.dst, http.StatusBadGateway, "api_error", err.Error())
		return
	}
	w.writeMessageJSON(status, msg)
}

func (w *anthropicWriter) writeMessageJSON(status int, msg map[string]any) {
	w.flushHeader()
	writeJSON(w.dst, status, msg)
}

// 编译期断言：配置指针与 handler 的原子存储（与 responsesCfg 同模式）。
var _ atomic.Pointer[AnthropicConfig]
