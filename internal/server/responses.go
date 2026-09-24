package server

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"github.com/linguo2625469/workbuddy2api-panel/internal/responses"
	"github.com/linguo2625469/workbuddy2api-panel/internal/responsesstore"
)

// ResponsesConfig Responses 端点配置（由 cmd/server 从独立文件/环境变量构造后注入）。
// 为降低与上游 config.go 的冲突，不进主 Config 结构。
type ResponsesConfig struct {
	// Enabled 是否响应 /v1/responses（运行时可热切换）。
	Enabled bool
	// Store 增量会话存储（previous_response_id 上下文补全）。nil = 关闭增量补全。
	Store responsesstore.Store
	// ModelMap Responses 模型名 → 上游模型名。
	ModelMap map[string]string
	// DefaultModel 未命中 ModelMap 时的回落模型（空 = 不改写）。
	DefaultModel string
}

// responses 处理 POST /v1/responses：把 Responses 请求转换为 Chat 请求，内部复用
// 既有 chatCompletions 链路（鉴权/轮转/冷却/粘性/日志），再用包装 Writer 把出口
// 转换为 Responses 形态。零改动 chatCompletions。
func (h *Handler) responses(w http.ResponseWriter, r *http.Request) {
	// 运行期读取当前配置（支持面板保存后热重载）。
	cfg := h.responsesCfg.Load()
	if cfg == nil || !cfg.Enabled {
		writeResponsesError(w, http.StatusNotFound, "not_found", "responses endpoint is disabled")
		return
	}
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		writeResponsesError(w, http.StatusBadRequest, "invalid_request", "read body: "+err.Error())
		return
	}

	// 1. 增量补全：previous_response_id → 取缓存 calls 插到 input 前。
	if cfg.Store != nil {
		if filled, n := cfg.Store.Fill(raw); n > 0 {
			raw = filled
		}
	}

	// 2. Responses → Chat（含工具上下文，供回程还原）。
	chatBody, toolCtx, err := responses.ToChat(raw)
	if err != nil {
		writeResponsesError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}

	// 3. 模型名映射（Codex 发 gpt-5-codex，上游只认 CodeBuddy 模型名）。
	chatBody = applyResponsesModelMap(chatBody, cfg)

	// 4. 构造等价 Chat 请求（复制 header，替换 body），走既有链路。
	nr := r.Clone(r.Context())
	nr.Body = io.NopCloser(bytes.NewReader(chatBody))
	nr.ContentLength = int64(len(chatBody))
	nr.Header = r.Header.Clone()
	nr.Header.Set("Content-Type", "application/json")

	rw := newResponsesWriter(w, toolCtx)
	h.chatCompletions(rw, nr)
	rw.finish()

	// 5. 记录本回合 output items 供下次 previous_response_id 补全。
	if cfg.Store != nil {
		if items := rw.OutputItems(); len(items) > 0 {
			cfg.Store.Record(rw.ResponseID(), items)
		}
	}
}

// applyResponsesModelMap 按配置改写请求体 model（命中映射或回落默认；空则不写）。
func applyResponsesModelMap(body []byte, cfg *ResponsesConfig) []byte {
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

// writeResponsesError 写 Responses 错误形状。
func writeResponsesError(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]any{"error": map[string]any{
		"message": msg,
		"type":    "api_error",
		"code":    code,
		"param":   nil,
	}})
}

// responsesWriter 包装 http.ResponseWriter，把 chatCompletions 的出口（Chat JSON 或
// Chat SSE）转换为 Responses 形态。流式实时转换；非流式缓冲后一次转换。
type responsesWriter struct {
	dst     http.ResponseWriter
	toolCtx *responses.ToolContext

	status     int
	headerSent bool
	isSSE      *bool

	sseState  *responses.StreamState
	streamBuf bytes.Buffer

	bodyBuf     bytes.Buffer
	outputItems []any
	responseID  string
}

func newResponsesWriter(dst http.ResponseWriter, toolCtx *responses.ToolContext) *responsesWriter {
	return &responsesWriter{dst: dst, toolCtx: toolCtx, sseState: responses.NewStreamState(toolCtx)}
}

func (w *responsesWriter) Header() http.Header { return w.dst.Header() }

func (w *responsesWriter) WriteHeader(code int) { w.status = code }

func (w *responsesWriter) Write(p []byte) (int, error) {
	if w.isSSE == nil {
		w.detectMode()
	}
	if *w.isSSE {
		return w.writeSSE(p)
	}
	return w.bodyBuf.Write(p)
}

// Flush 实现 http.Flusher：流式下把已产生的 Responses 事件刷给客户端。
func (w *responsesWriter) Flush() {
	w.flushHeader()
	if fl, ok := w.dst.(http.Flusher); ok {
		fl.Flush()
	}
}

func (w *responsesWriter) detectMode() {
	isSSE := strings.Contains(w.dst.Header().Get("Content-Type"), "text/event-stream")
	w.isSSE = &isSSE
	if isSSE {
		w.flushHeader()
	}
}

func (w *responsesWriter) flushHeader() {
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

func (w *responsesWriter) writeSSE(p []byte) (int, error) {
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

func (w *responsesWriter) handleSSEBlock(block string) error {
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
		if msg == "" {
			msg = "upstream error"
		}
		return w.writeOut(w.sseState.Failed(msg, "upstream_error"))
	}
	return w.writeOut(w.sseState.HandleChunk(chunk))
}

func (w *responsesWriter) writeOut(b []byte) error {
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

// finish 在 chatCompletions 返回后调用：流式补 response.completed；非流式做整体转换。
func (w *responsesWriter) finish() {
	if w.isSSE != nil && *w.isSSE {
		_ = w.writeOut(w.sseState.Finalize())
		return
	}
	w.finalizeNonStream()
}

func (w *responsesWriter) finalizeNonStream() {
	status := w.status
	if status == 0 {
		status = http.StatusOK
	}
	body := w.bodyBuf.Bytes()

	var parsed map[string]any
	if json.Unmarshal(body, &parsed) != nil {
		writeResponsesError(w.dst, status, "upstream_error", string(body))
		return
	}
	if _, hasErr := parsed["error"]; hasErr {
		writeJSON(w.dst, status, responses.ErrorToResponses(body))
		return
	}
	resp, err := responses.FromChat(parsed, w.toolCtx)
	if err != nil {
		writeResponsesError(w.dst, http.StatusBadGateway, "upstream_parse", err.Error())
		return
	}
	w.responseID, _ = resp["id"].(string)
	w.outputItems, _ = resp["output"].([]any)
	writeJSON(w.dst, status, resp)
}

// OutputItems 返回本回合已完成的 output items（供增量存储）。
func (w *responsesWriter) OutputItems() []any {
	if w.isSSE != nil && *w.isSSE {
		return w.sseState.OutputItems()
	}
	return w.outputItems
}

// ResponseID 返回本回合 response id。
func (w *responsesWriter) ResponseID() string {
	if w.isSSE != nil && *w.isSSE {
		return w.sseState.ResponseID()
	}
	return w.responseID
}

// takeSSEBlock 从缓冲中取一个以空行分隔的 SSE 块。
func takeSSEBlock(buf *bytes.Buffer) (string, bool) {
	data := buf.Bytes()
	idx := bytes.Index(data, []byte("\n\n"))
	if idx < 0 {
		return "", false
	}
	block := string(data[:idx])
	buf.Next(idx + 2)
	return block, true
}

// extractDataLine 取 SSE 块的 data 行内容。
func extractDataLine(block string) string {
	for _, line := range strings.Split(block, "\n") {
		line = strings.TrimRight(line, "\r")
		if strings.HasPrefix(line, "data: ") {
			return strings.TrimPrefix(line, "data: ")
		}
		if line == "data:" {
			return ""
		}
	}
	return ""
}
