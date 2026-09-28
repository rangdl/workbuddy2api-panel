package trae

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"strings"
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
	Event        string
	Response     string
	Reasoning    string
	ToolCalls    []any
	Usage        map[string]any
	FinishReason string
	ErrorCode    int64
	ErrorMsg     string
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
	ev := &soloEvent{Event: strings.TrimSpace(event)}
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
	}
	return ev
}
