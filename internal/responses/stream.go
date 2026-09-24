package responses

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
)

// streamToolCall 一个流式工具调用的累积状态。
type streamToolCall struct {
	outputIndex int
	itemID      string
	callID      string
	name        string
	arguments   strings.Builder
	added       bool
	done        bool
}

// StreamState 把 Chat Completions SSE 帧转换为 Responses SSE 事件。
//
// 用法：对每个 Chat chunk 调 HandleChunk 取得要写出的字节；流结束时调 Finalize
// 收尾（response.completed）。事件形状对齐 cc-switch 的 codex_responses_sse.rs。
type StreamState struct {
	responseID      string
	model           string
	createdAt       int64
	nextOutputIndex int

	textAdded bool
	textIndex int
	textBuf   strings.Builder

	reasoningAdded bool
	reasoningIndex int
	reasoningBuf   strings.Builder

	tools     map[int]*streamToolCall
	toolOrder []int

	latestUsage  map[string]any
	finishReason string
	started      bool
	completed    bool
	outputItems  []map[string]any
}

// NewStreamState 构造流式转换状态机。
func NewStreamState() *StreamState {
	return &StreamState{
		responseID: "resp_wb2api",
		tools:      map[int]*streamToolCall{},
	}
}

// HandleChunk 处理一个 Chat chunk，返回需要写出的 Responses 事件字节。
func (s *StreamState) HandleChunk(chunk map[string]any) []byte {
	var out bytes.Buffer

	if id := rawString(chunk, "id"); id != "" {
		s.responseID = responseIDFromChatID(id)
	}
	if model := rawString(chunk, "model"); model != "" {
		s.model = model
	}
	if created := numToInt(chunk["created"]); created != 0 {
		s.createdAt = created
	}
	s.ensureStarted(&out)

	if usage, ok := chunk["usage"]; ok && usage != nil {
		s.latestUsage = chatUsageToResponsesUsage(usage)
	}

	choices, _ := chunk["choices"].([]any)
	if len(choices) == 0 {
		return out.Bytes()
	}
	choice, _ := choices[0].(map[string]any)

	if delta, ok := choice["delta"].(map[string]any); ok {
		if reasoning := extractReasoningFieldText(delta); reasoning != "" {
			s.pushReasoning(&out, reasoning)
		}
		if content := rawString(delta, "content"); content != "" {
			s.pushText(&out, content)
		}
		if tcs, ok := delta["tool_calls"].([]any); ok {
			s.finalizeReasoning(&out)
			for _, raw := range tcs {
				if tc, ok := raw.(map[string]any); ok {
					s.pushToolCall(&out, tc)
				}
			}
		}
	}
	if fr := rawString(choice, "finish_reason"); fr != "" {
		s.finishReason = fr
	}
	return out.Bytes()
}

// Finalize 收尾：关闭所有打开的 item 并发 response.completed（幂等）。
func (s *StreamState) Finalize() []byte {
	if s.completed {
		return nil
	}
	var out bytes.Buffer
	s.ensureStarted(&out)
	s.finalizeReasoning(&out)
	s.finalizeText(&out)
	s.finalizeTools(&out)

	status := responseStatusFromFinishReason(s.finishReason)
	response := s.baseResponse(status, itemsToAny(s.outputItems))
	if status == "incomplete" {
		response["incomplete_details"] = map[string]any{"reason": "max_output_tokens"}
	}
	out.Write(sseEvent("response.completed", map[string]any{"response": response}))
	s.completed = true
	return out.Bytes()
}

// Failed 发出 response.failed（用于上游错误/空流）。
func (s *StreamState) Failed(message, errType string) []byte {
	if s.completed {
		return nil
	}
	var out bytes.Buffer
	s.ensureStarted(&out)
	s.finalizeReasoning(&out)
	s.finalizeText(&out)
	s.finalizeTools(&out)

	response := s.baseResponse("failed", itemsToAny(s.outputItems))
	response["error"] = map[string]any{"message": message, "type": firstNonEmpty(errType, "upstream_error")}
	out.Write(sseEvent("response.failed", map[string]any{"response": response}))
	s.completed = true
	return out.Bytes()
}

// OutputItems 返回已完成的 output items（供增量存储 Record）。
func (s *StreamState) OutputItems() []any {
	return itemsToAny(s.outputItems)
}

// ResponseID 返回当前 response id。
func (s *StreamState) ResponseID() string { return s.responseID }

// ---- 内部：事件发射 ----

func (s *StreamState) ensureStarted(out *bytes.Buffer) {
	if s.started {
		return
	}
	s.started = true
	response := s.baseResponse("in_progress", []any{})
	out.Write(sseEvent("response.created", map[string]any{"response": response}))
	out.Write(sseEvent("response.in_progress", map[string]any{"response": response}))
}

func (s *StreamState) pushReasoning(out *bytes.Buffer, delta string) {
	if !s.reasoningAdded {
		s.reasoningIndex = s.allocIndex()
		s.reasoningAdded = true
		itemID := "rs_" + s.responseID
		out.Write(sseEvent("response.output_item.added", map[string]any{
			"output_index": s.reasoningIndex,
			"item":         map[string]any{"id": itemID, "type": "reasoning", "status": "in_progress", "summary": []any{}},
		}))
		out.Write(sseEvent("response.reasoning_summary_part.added", map[string]any{
			"item_id": itemID, "output_index": s.reasoningIndex, "summary_index": 0,
			"part": map[string]any{"type": "summary_text", "text": ""},
		}))
	}
	s.reasoningBuf.WriteString(delta)
	out.Write(sseEvent("response.reasoning_summary_text.delta", map[string]any{
		"item_id": "rs_" + s.responseID, "output_index": s.reasoningIndex, "summary_index": 0, "delta": delta,
	}))
}

func (s *StreamState) pushText(out *bytes.Buffer, delta string) {
	if !s.textAdded {
		s.textIndex = s.allocIndex()
		s.textAdded = true
		itemID := s.responseID + "_msg"
		out.Write(sseEvent("response.output_item.added", map[string]any{
			"output_index": s.textIndex,
			"item":         map[string]any{"id": itemID, "type": "message", "status": "in_progress", "role": "assistant", "content": []any{}},
		}))
		out.Write(sseEvent("response.content_part.added", map[string]any{
			"item_id": itemID, "output_index": s.textIndex, "content_index": 0,
			"part": map[string]any{"type": "output_text", "text": "", "annotations": []any{}},
		}))
	}
	s.textBuf.WriteString(delta)
	out.Write(sseEvent("response.output_text.delta", map[string]any{
		"item_id": s.responseID + "_msg", "output_index": s.textIndex, "content_index": 0, "delta": delta,
	}))
}

func (s *StreamState) pushToolCall(out *bytes.Buffer, tc map[string]any) {
	idx := 0
	if v, ok := tc["index"].(float64); ok {
		idx = int(v)
	}
	state, ok := s.tools[idx]
	if !ok {
		state = &streamToolCall{outputIndex: s.allocIndex()}
		s.tools[idx] = state
		s.toolOrder = append(s.toolOrder, idx)
	}
	if id := rawString(tc, "id"); id != "" {
		state.callID = id
	}
	fn, _ := tc["function"].(map[string]any)
	if name := rawString(fn, "name"); name != "" {
		state.name = name
	}
	if !state.added && state.name != "" {
		state.added = true
		state.itemID = "fc_" + firstNonEmpty(state.callID, fmt.Sprintf("call_%d", idx))
		out.Write(sseEvent("response.output_item.added", map[string]any{
			"output_index": state.outputIndex,
			"item": map[string]any{
				"id": state.itemID, "type": "function_call", "status": "in_progress",
				"call_id": state.callID, "name": state.name, "arguments": "",
			},
		}))
	}
	if args := rawString(fn, "arguments"); args != "" {
		state.arguments.WriteString(args)
		if state.added {
			out.Write(sseEvent("response.function_call_arguments.delta", map[string]any{
				"item_id": state.itemID, "output_index": state.outputIndex, "delta": args,
			}))
		}
	}
}

func (s *StreamState) finalizeReasoning(out *bytes.Buffer) {
	if !s.reasoningAdded {
		return
	}
	s.reasoningAdded = false
	itemID := "rs_" + s.responseID
	text := s.reasoningBuf.String()
	out.Write(sseEvent("response.reasoning_summary_text.done", map[string]any{
		"item_id": itemID, "output_index": s.reasoningIndex, "summary_index": 0, "text": text,
	}))
	out.Write(sseEvent("response.reasoning_summary_part.done", map[string]any{
		"item_id": itemID, "output_index": s.reasoningIndex, "summary_index": 0,
		"part": map[string]any{"type": "summary_text", "text": text},
	}))
	item := map[string]any{
		"id": itemID, "type": "reasoning",
		"summary": []any{map[string]any{"type": "summary_text", "text": text}},
	}
	out.Write(sseEvent("response.output_item.done", map[string]any{"output_index": s.reasoningIndex, "item": item}))
	s.outputItems = append(s.outputItems, item)
}

func (s *StreamState) finalizeText(out *bytes.Buffer) {
	if !s.textAdded {
		return
	}
	s.textAdded = false
	itemID := s.responseID + "_msg"
	text := s.textBuf.String()
	out.Write(sseEvent("response.output_text.done", map[string]any{
		"item_id": itemID, "output_index": s.textIndex, "content_index": 0, "text": text,
	}))
	out.Write(sseEvent("response.content_part.done", map[string]any{
		"item_id": itemID, "output_index": s.textIndex, "content_index": 0,
		"part": map[string]any{"type": "output_text", "text": text, "annotations": []any{}},
	}))
	item := map[string]any{
		"id": itemID, "type": "message", "status": "completed", "role": "assistant",
		"content": []any{map[string]any{"type": "output_text", "text": text, "annotations": []any{}}},
	}
	out.Write(sseEvent("response.output_item.done", map[string]any{"output_index": s.textIndex, "item": item}))
	s.outputItems = append(s.outputItems, item)
}

func (s *StreamState) finalizeTools(out *bytes.Buffer) {
	for _, idx := range s.toolOrder {
		state := s.tools[idx]
		if state == nil || state.done || !state.added {
			continue
		}
		args := state.arguments.String()
		out.Write(sseEvent("response.function_call_arguments.done", map[string]any{
			"item_id": state.itemID, "output_index": state.outputIndex, "arguments": args,
		}))
		item := map[string]any{
			"id": state.itemID, "type": "function_call", "status": "completed",
			"call_id": state.callID, "name": state.name, "arguments": args,
		}
		out.Write(sseEvent("response.output_item.done", map[string]any{"output_index": state.outputIndex, "item": item}))
		s.outputItems = append(s.outputItems, item)
		state.done = true
	}
}

func (s *StreamState) baseResponse(status string, output []any) map[string]any {
	usage := s.latestUsage
	if usage == nil {
		usage = emptyResponsesUsage()
	}
	return map[string]any{
		"id":         s.responseID,
		"object":     "response",
		"created_at": s.createdAt,
		"status":     status,
		"model":      s.model,
		"output":     output,
		"usage":      usage,
	}
}

func (s *StreamState) allocIndex() int {
	i := s.nextOutputIndex
	s.nextOutputIndex++
	return i
}

// sseEvent 构造一个 Responses SSE 事件（event: 行 + data: 行）。
func sseEvent(eventType string, data map[string]any) []byte {
	data["type"] = eventType
	raw, err := json.Marshal(data)
	if err != nil {
		return nil
	}
	var b bytes.Buffer
	b.WriteString("event: ")
	b.WriteString(eventType)
	b.WriteString("\ndata: ")
	b.Write(raw)
	b.WriteString("\n\n")
	return b.Bytes()
}

func itemsToAny(items []map[string]any) []any {
	out := make([]any, len(items))
	for i, item := range items {
		out[i] = item
	}
	return out
}
