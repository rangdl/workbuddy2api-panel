package trae

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"sync/atomic"
	"time"
)

// sse.go Trae SOLO 自定义 SSE → OpenAI SSE / 聚合结果。
//
// 上游事件（2026-09 实测，蓝本 TraeWorkAssistant 的 api_server/sse.rs）：
//
//	event:metadata      会话元数据（忽略）
//	event:timing_cost   耗时统计（忽略）
//	event:output        ×N 增量内容（response / reasoning_content / tool_calls）
//	event:thought       create_agent_task 时代的等价事件（保留兼容）
//	event:extra_info    额外信息（忽略）
//	event:token_usage   token 统计（附到最后一个 chunk 的 usage）
//	event:done          结束信号（finish_reason）
//	event:turn_completion 旧结束信号（等价 finish_reason=stop）
//	event:error         流内错误（code + message）
//
// 转换要点：output → delta.content/reasoning_content/tool_calls（清理 namespace /
// partial_arguments 等 SOLO 专属字段）；done → finish_reason + [DONE]；
// 上游中断无 done 时幂等兜底仍写 [DONE]。

// soloEvent 一个解析后的上游事件。
type soloEvent struct {
	Event string
	// Data 原始 data 行内容（诊断用，见 SetRawDump）。
	Data         string
	Response     string
	Reasoning    string
	ToolCalls    []any
	Usage        map[string]any
	FinishReason string
	ErrorCode    int64
	ErrorMsg     string
	// RemainIDE / RemainWork notify_usage 给出的剩余额度（IDE / Work 积分）。
	RemainIDE  float64
	RemainWork float64
	HasRemain  bool
	// BillingMode notify_usage 的计费模式（如 credits / dollar）。
	BillingMode string
	// QueuePosition request_wait_in_queue 的排队位置。
	QueuePosition int64
	HasQueue      bool
	// Raw notify_usage 的完整原始 payload（排查扣费口径用；不参与业务逻辑）。
	Raw map[string]any
}

// StreamResult 流式转换的结果统计。
type StreamResult struct {
	// ErrorCode / ErrorMsg 上游流内错误（0 = 无错误）。
	ErrorCode int64
	ErrorMsg  string
	// SentAny 是否已向客户端下发过内容（决定错误能否由调用方重试）。
	SentAny bool
	// Usage 上游 token 统计（可能为 nil）。
	Usage map[string]any
	// Content / Reasoning 累积的文本（供调用方记账与日志）。
	Content   strings.Builder
	Reasoning strings.Builder
	// RemainIDE / RemainWork notify_usage 给出的剩余额度（HasRemain 为真时有效）。
	RemainIDE  float64
	RemainWork float64
	HasRemain  bool
	// RemainRaw notify_usage 的原始 payload（可观测性：确认上游是否给出本次扣费）。
	RemainRaw map[string]any
}

// ConvertStream 把上游 SOLO SSE 流转成 OpenAI SSE 帧，逐帧交给 emit。
//
// emit 返回错误即中止（客户端断连）。返回的 StreamResult 携带错误码与用量，
// 供调用方决定重试或记账：
//   - 若首个事件即 error 且尚未下发任何数据（SentAny=false），调用方可换号重试；
//   - 已有数据流出时错误会就地透传给客户端并补 [DONE]（无法再改状态码）。
func ConvertStream(r io.Reader, chatID, model string, emit func([]byte) error) (StreamResult, error) {
	var res StreamResult
	var pendingUsage map[string]any
	sawDone := false

	sc := newSSEScanner(r)
	for {
		ev, err := sc.next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return res, err
		}
		if ev == nil {
			continue
		}
		dumpEvent(ev)
		switch ev.Event {
		case "output", "thought":
			delta := map[string]any{}
			if ev.Response != "" {
				delta["content"] = ev.Response
				res.Content.WriteString(ev.Response)
			}
			if ev.Reasoning != "" {
				delta["reasoning_content"] = ev.Reasoning
				res.Reasoning.WriteString(ev.Reasoning)
			}
			if calls := convertToolCalls(ev.ToolCalls); len(calls) > 0 {
				delta["tool_calls"] = calls
			}
			if len(delta) == 0 {
				if !res.SentAny {
					// 空 delta 的首个 output：上游已开始产出，发一个仅含 role 的
					// 占位 chunk，让 SentAny 语义与真实下发一致（否则后续错误
					// 会被误判为"可重试"，而客户端其实已经收到流开始）。
					if err := emit(chunkBytes(chatID, model, map[string]any{"role": "assistant"}, "", nil)); err != nil {
						return res, err
					}
					res.SentAny = true
				}
				continue
			}
			if err := emit(chunkBytes(chatID, model, delta, "", pendingUsage)); err != nil {
				return res, err
			}
			res.SentAny = true
		case "token_usage":
			if ev.Usage != nil {
				pendingUsage = ev.Usage
			}
		case "notify_usage":
			// 只保留最后一次：一次对话可能多次上报，最后一条才是当前余额。
			if ev.HasRemain {
				res.RemainIDE = ev.RemainIDE
				res.RemainWork = ev.RemainWork
				res.HasRemain = true
				res.RemainRaw = ev.Raw
			}
		case "done", "turn_completion":
			finish := ev.FinishReason
			if finish == "" {
				finish = "stop"
			}
			if err := emit(chunkBytes(chatID, model, map[string]any{}, finish, pendingUsage)); err != nil {
				return res, err
			}
			if err := emit([]byte("data: [DONE]\n\n")); err != nil {
				return res, err
			}
			res.Usage = pendingUsage
			res.SentAny = true
			sawDone = true
		case "error":
			res.ErrorCode = ev.ErrorCode
			res.ErrorMsg = ev.ErrorMsg
			if res.SentAny {
				// 已开流：就地透传错误帧并收尾（HTTP 头早已发出，只能这样告知）。
				frame := map[string]any{"error": map[string]any{
					"message": ev.ErrorMsg,
					"type":    "api_error",
					"code":    ev.ErrorCode,
				}}
				b, _ := json.Marshal(frame)
				if err := emit(append(append([]byte("data: "), b...), '\n', '\n')); err != nil {
					return res, err
				}
				if err := emit([]byte("data: [DONE]\n\n")); err != nil {
					return res, err
				}
				sawDone = true
			}
		}
	}
	if !sawDone && res.ErrorCode == 0 {
		// 上游中断（无 done 无 error）：补 [DONE] 保证客户端能正常收流。
		if err := emit([]byte("data: [DONE]\n\n")); err != nil {
			return res, err
		}
	}
	if res.Usage == nil {
		res.Usage = pendingUsage
	}
	return res, nil
}

// StreamError 上游流内错误（供调用方做分类与冷却决策）。
type StreamError struct {
	// Code 上游业务码（如 1005 套餐额度用尽、20405 缺少设备签名）。
	Code int64
	// Message 可读原因（上游原文优先）。
	Message string
}

// Aggregate 把 SOLO SSE 流聚合为单个 OpenAI chat.completion 对象（非流式请求用）。
// 返回的第二个值为上游流内错误（nil = 无错误）。
func Aggregate(r io.Reader, chatID, model string) (map[string]any, *StreamError, error) {
	content := strings.Builder{}
	reasoning := strings.Builder{}
	var toolCalls []any
	var usage map[string]any
	finish := ""
	var streamErr *StreamError

	sc := newSSEScanner(r)
	for {
		ev, err := sc.next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, nil, err
		}
		if ev == nil {
			continue
		}
		switch ev.Event {
		case "output", "thought":
			content.WriteString(ev.Response)
			reasoning.WriteString(ev.Reasoning)
			if calls := convertToolCalls(ev.ToolCalls); len(calls) > 0 {
				toolCalls = append(toolCalls, calls...)
			}
		case "token_usage":
			usage = ev.Usage
		case "done", "turn_completion":
			finish = ev.FinishReason
			if finish == "" {
				finish = "stop"
			}
		case "error":
			streamErr = &StreamError{Code: ev.ErrorCode, Message: ev.ErrorMsg}
		}
	}
	if streamErr != nil {
		return nil, streamErr, nil
	}
	if finish == "" {
		finish = "stop"
	}

	msg := map[string]any{"role": "assistant", "content": content.String()}
	if reasoning.Len() > 0 {
		msg["reasoning_content"] = reasoning.String()
	}
	if len(toolCalls) > 0 {
		msg["tool_calls"] = toolCalls
	}
	resp := map[string]any{
		"id":      chatID,
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   model,
		"choices": []any{map[string]any{
			"index":         0,
			"message":       msg,
			"finish_reason": finish,
		}},
	}
	if usage != nil {
		resp["usage"] = normalizeUsage(usage)
	}
	return resp, nil, nil
}

// convertToolCalls 把 SOLO 的 tool_calls 归一化为 OpenAI 形态：
// function_call → function，并剔除 namespace / partial_arguments 等专属字段。
func convertToolCalls(calls []any) []any {
	if len(calls) == 0 {
		return nil
	}
	out := make([]any, 0, len(calls))
	for _, raw := range calls {
		c, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if fc, exists := c["function_call"]; exists {
			c["function"] = fc
			delete(c, "function_call")
		}
		if fn, ok := c["function"].(map[string]any); ok {
			clean := make(map[string]any, len(fn))
			for k, v := range fn {
				if k == "namespace" || k == "partial_arguments" {
					continue
				}
				clean[k] = v
			}
			c["function"] = clean
		}
		out = append(out, c)
	}
	return out
}

// normalizeUsage 把上游 usage 归一化为 OpenAI 字段名（上游已是同名，
// 此处只做数值类型兜底与 total 补齐）。
func normalizeUsage(u map[string]any) map[string]any {
	out := map[string]any{}
	pt := intOf(u["prompt_tokens"])
	ct := intOf(u["completion_tokens"])
	tt := intOf(u["total_tokens"])
	if tt == 0 {
		tt = pt + ct
	}
	if _, ok := u["prompt_tokens"]; ok || pt > 0 {
		out["prompt_tokens"] = pt
	}
	if _, ok := u["completion_tokens"]; ok || ct > 0 {
		out["completion_tokens"] = ct
	}
	out["total_tokens"] = tt

	// 缓存拆分（OpenAI 兼容形态）：上游给 cache_read_input_tokens /
	// cache_creation_input_tokens，这里映射成 prompt_tokens_details.cached_tokens
	// 与 cache_creation_tokens。
	//
	// 为什么重要：命中缓存的输入 token 计费远低于未命中，不区分就无法解释
	// 「同样的 token 数为什么这次扣得多」。多轮长对话尤其明显。
	cacheRead := intOf(u["cache_read_input_tokens"])
	cacheCreate := intOf(u["cache_creation_input_tokens"])
	reasoning := intOf(u["reasoning_tokens"])
	if cacheRead > 0 || cacheCreate > 0 {
		details := map[string]any{}
		if cacheRead > 0 {
			details["cached_tokens"] = cacheRead
		}
		if cacheCreate > 0 {
			details["cache_creation_tokens"] = cacheCreate
		}
		out["prompt_tokens_details"] = details
	}
	if reasoning > 0 {
		out["completion_tokens_details"] = map[string]any{"reasoning_tokens": reasoning}
	}
	return out
}

// intOf 宽容取整（上游可能给 float64 / json.Number / 字符串）。
func intOf(v any) int64 {
	switch n := v.(type) {
	case float64:
		return int64(n)
	case int64:
		return n
	case int:
		return int64(n)
	case json.Number:
		i, _ := n.Int64()
		return i
	default:
		return 0
	}
}

// chunkBytes 构造一个 OpenAI SSE chunk 帧（含 data: 前缀与空行）。
func chunkBytes(chatID, model string, delta map[string]any, finish string, usage map[string]any) []byte {
	var fr any
	if finish != "" {
		fr = finish
	}
	chunk := map[string]any{
		"id":      chatID,
		"object":  "chat.completion.chunk",
		"created": time.Now().Unix(),
		"model":   model,
		"choices": []any{map[string]any{
			"index":         0,
			"delta":         delta,
			"finish_reason": fr,
		}},
	}
	if usage != nil {
		chunk["usage"] = normalizeUsage(usage)
	}
	b, err := json.Marshal(chunk)
	if err != nil {
		return nil
	}
	return append(append([]byte("data: "), b...), '\n', '\n')
}

// ---- SSE 行扫描 ----

// sseScanner 逐事件解析 SSE 流（支持多行 data 与注释行）。
type sseScanner struct {
	sc *bufio.Scanner
}

func newSSEScanner(r io.Reader) *sseScanner {
	sc := bufio.NewScanner(r)
	// 上游单帧可能较大（含 tool_calls 参数），放宽到 1MB。
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	return &sseScanner{sc: sc}
}

// next 读取下一个事件；流结束返回 (nil, io.EOF)。
// 未知/空事件返回 (nil, nil)（调用方 continue）。
func (s *sseScanner) next() (*soloEvent, error) {
	var event string
	var data strings.Builder
	sawField := false

	for s.sc.Scan() {
		line := strings.TrimRight(s.sc.Text(), "\r")
		if line == "" {
			if !sawField {
				continue // 空行分隔符，忽略连续空行
			}
			return parseSoloEvent(event, data.String()), nil
		}
		if strings.HasPrefix(line, ":") {
			continue // SSE 注释（keep-alive）
		}
		sawField = true
		if v, ok := strings.CutPrefix(line, "event:"); ok {
			event = strings.TrimSpace(v)
			continue
		}
		if v, ok := strings.CutPrefix(line, "data:"); ok {
			if data.Len() > 0 {
				data.WriteByte('\n')
			}
			data.WriteString(strings.TrimSpace(v))
		}
	}
	if err := s.sc.Err(); err != nil {
		return nil, err
	}
	if sawField {
		// 流在事件中途结束（上游未发尾随空行）：仍解析已收内容。
		return parseSoloEvent(event, data.String()), nil
	}
	return nil, io.EOF
}

// parseSoloEvent 把 (event, data) 解析为结构化事件。
// 未知事件类型返回事件对象本身（调用方按 event 名过滤）。
func parseSoloEvent(event, data string) *soloEvent {
	ev := &soloEvent{Event: strings.TrimSpace(event), Data: data}
	if strings.TrimSpace(data) == "" {
		return ev
	}
	var raw map[string]any
	if err := json.Unmarshal([]byte(data), &raw); err != nil {
		return ev // 非 JSON data：保留事件名，字段全空
	}
	switch ev.Event {
	case "output", "thought":
		if s, ok := raw["response"].(string); ok {
			ev.Response = s
		}
		if s, ok := raw["thought"].(string); ok && ev.Response == "" {
			ev.Response = s
		}
		if s, ok := raw["reasoning_content"].(string); ok {
			ev.Reasoning = s
		}
		if tc, ok := raw["tool_calls"].([]any); ok && tc != nil {
			ev.ToolCalls = tc
		}
	case "token_usage":
		ev.Usage = raw
	case "done":
		if s, ok := raw["finish_reason"].(string); ok {
			ev.FinishReason = s
		}
	case "turn_completion":
		ev.FinishReason = "stop"
	case "error":
		ev.ErrorCode = intOf(raw["code"])
		if s, ok := raw["message"].(string); ok {
			ev.ErrorMsg = s
		}
		if ev.ErrorMsg == "" {
			ev.ErrorMsg = fmt.Sprintf("upstream error code=%d", ev.ErrorCode)
		}
	case "notify_usage":
		// 真实的剩余额度来源：cn_credits_remain_info.{ide_credits, work_credits}。
		// 积分查询接口的口径包含 work 额度，会高估"可用"；这里是唯一能区分
		// IDE 积分（对话真正消耗的那份）与 Work 积分的地方。
		if info, ok := raw["cn_credits_remain_info"].(map[string]any); ok {
			ev.RemainIDE = floatOf(info["ide_credits"])
			ev.RemainWork = floatOf(info["work_credits"])
			ev.HasRemain = true
		}
		if s, ok := raw["billing_mode"].(string); ok {
			ev.BillingMode = s
		}
		ev.Raw = raw
	case "request_wait_in_queue":
		// 排队位置：可用于判断是否该换号（chat_v3 通用池常 1000+，solo 较轻）。
		ev.QueuePosition = intOf(raw["position"])
		if ev.QueuePosition == 0 {
			ev.QueuePosition = intOf(raw["queue_position"])
		}
		ev.HasQueue = true
	}
	return ev
}

// PeekResult peek 上游流的结果。
type PeekResult struct {
	// Header 预读消耗的原始字节，调用方必须拼回流前面（io.MultiReader）。
	Header []byte
	// Error 流内错误（nil = 无错误，可正常转发）。
	Error *StreamError
	// QueuePosition 预读阶段看到的排队位置（HasQueue 为真时有效）。
	//
	// 上游在开始产出内容前可能先发 request_wait_in_queue。排队过深时
	// 换一个 function/账号往往比干等更快（chat_v3 通用池常 1000+，
	// solo 池较轻 —— 实测差异明显）。
	QueuePosition int64
	HasQueue      bool
}

// peekMaxBytes 预读上限：够覆盖诊断事件 + error 事件即可。
const peekMaxBytes = 64 * 1024

// PeekStream 预读流开头，判断上游是否在 HTTP 200 的 SSE 里返回了错误。
//
// 为什么需要：Trae 可能对「额度不足 / 模型不可用」这类拒绝返回 HTTP 200，
// 然后在流里发的第一个事件就是 `event:error`。若直接透传 200，客户端只会
// 看到 "stream ended without finish_reason" 并自行重试多次——每次都烧上游
// 风控额度，且真正的错误原因永远不暴露（dsh issue #19 的根因）。
//
// 实现：用 TeeReader 边解析边留底。每条事件解析前记录已读偏移，判定为
// 「诊断类」就把该事件对应的字节保留在 header 里继续往后看；判定为内容/done
// 就停止预读，把剩余已读字节一并交给调用方重放。
//
// 注意：`metadata` / `timing_cost` 等诊断事件**不算已应答**，因为拒绝可能
// 出现在它们之后（实测序列 metadata → timing_cost → error）。
func PeekStream(r io.Reader) (PeekResult, error) {
	var captured bytes.Buffer
	var queuePos int64
	var sawQueue bool
	tee := io.TeeReader(r, &captured)
	sc := newSSEScanner(tee)

	// consumed 是解析到当前事件为止，TeeReader 已写入 captured 的字节数。
	for {
		ev, err := sc.next()

		if err == io.EOF {
			// 流提前结束：已读的全部交回，让上层按"正常结束"处理。
			return PeekResult{Header: captured.Bytes(), QueuePosition: queuePos, HasQueue: sawQueue}, nil
		}
		if err != nil {
			return PeekResult{Header: captured.Bytes()}, err
		}
		if ev == nil {
			continue
		}

		switch ev.Event {
		case "error":
			return PeekResult{
				Header: captured.Bytes(),
				Error:  &StreamError{Code: ev.ErrorCode, Message: ev.ErrorMsg},
			}, nil
		case "metadata", "timing_cost", "extra_info", "request_wait_in_queue",
			"progress_notice", "notify_usage", "thought":
			// 诊断/元数据类：不算已应答，继续看下一条。
			// thought 虽属内容，但只有推理没有正文时也可能后接 error；
			// 为稳妥起见把它归入"继续看"，预读上限会兜住。
			if ev.HasQueue {
				queuePos, sawQueue = ev.QueuePosition, true
			}
			if captured.Len() >= peekMaxBytes {
				// 预读超限：放弃判定，把已读内容全交回（宁可不判错，也不吞流）。
				return PeekResult{Header: captured.Bytes(), QueuePosition: queuePos, HasQueue: sawQueue}, nil
			}
			continue
		default:
			// 内容事件 / done：上游已正常应答，停止预读。
			return PeekResult{Header: captured.Bytes(), QueuePosition: queuePos, HasQueue: sawQueue}, nil
		}
	}
}

// rawDump 原始事件转储开关（诊断用：确认上游到底给了哪些字段）。
//
// 为什么做成包级开关而不是配置项：它只在排障时临时开启（面板「聊天原始事件」
// 或环境变量），常态关闭。开着会把每次对话的完整事件流打进日志，
// 量大且含对话内容，不适合长期启用。
var rawDump atomic.Bool

// SetRawDump 开启/关闭原始事件转储。
func SetRawDump(on bool) { rawDump.Store(on) }

// RawDumpEnabled 当前是否开启。
func RawDumpEnabled() bool { return rawDump.Load() }

// DumpEvent 在转储开启时回调每个解析前的事件（event 名 + 原始 data）。
//
// 由 server 层注入日志函数；nil 时静默。回调在流式热路径上，
// 实现方必须自己判断是否真的写日志（本函数已先检查 rawDump）。
var DumpEvent func(event, data string)

// dumpEvent 内部调用点：只有开关打开且回调非空才动作。
func dumpEvent(ev *soloEvent) {
	if ev == nil || !rawDump.Load() {
		return
	}
	if fn := DumpEvent; fn != nil {
		fn(ev.Event, ev.Data)
	}
}
