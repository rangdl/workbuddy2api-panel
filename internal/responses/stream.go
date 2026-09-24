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
	// toolIDIndex call_id → 分配的 index，供上游省略 index 时按 id 归位。
	toolIDIndex map[string]int
	// nextToolIndexToAdd 下一个待释放的工具槽位（按 chat index 顺序释放）。
	nextToolIndexToAdd int
	// droppedToolCalls 因缺函数名而被丢弃的工具调用数（用于「答一句就停」防御）。
	droppedToolCalls int

	toolCtx *ToolContext

	latestUsage  map[string]any
	finishReason string
	started      bool
	completed    bool
	outputItems  []map[string]any
}

// NewStreamState 构造流式转换状态机。toolCtx 用于把 Chat 工具名还原为
// Responses 的 function_call / custom_tool_call / tool_search_call（可为 nil）。
func NewStreamState(toolCtx *ToolContext) *StreamState {
	return &StreamState{
		responseID:  "resp_wb2api",
		tools:       map[int]*streamToolCall{},
		toolIDIndex: map[string]int{},
		toolCtx:     toolCtx,
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

	// 「答一句就停」防御（对齐 cc-switch #4341）：本应 completed 的回合里丢弃过
	// 工具调用、且最终一个可用工具调用都没剩下时，Codex 会收到 status=completed
	// 但 output 无任何工具调用的回合，agent loop 必然静默收尾。此时如实报错，
	// 而不是谎报成功。只要还剩下任何一个合法工具调用，判据不成立。
	// 仅对 completed 生效：length（截断）有自己的正当终止解释，工具调用缺 name
	// 是截断后果而非上游发了畸形数据，报成 dropped 会给出错误归因。
	if status == "completed" && s.droppedToolCalls > 0 && !s.hasEmittedToolCall() {
		msg := fmt.Sprintf("Upstream returned %d tool call(s) without a function name, leaving no usable tool call in this turn", s.droppedToolCalls)
		out.Write(sseEvent("response.failed", map[string]any{"response": s.failedResponse(msg, "upstream_tool_call_dropped")}))
		s.completed = true
		return out.Bytes()
	}

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
	out.Write(sseEvent("response.failed", map[string]any{"response": s.failedResponse(message, errType)}))
	s.completed = true
	return out.Bytes()
}

// failedResponse 构造 status=failed 的 response 对象（含 error）。
func (s *StreamState) failedResponse(message, errType string) map[string]any {
	resp := s.baseResponse("failed", itemsToAny(s.outputItems))
	resp["error"] = map[string]any{"message": message, "type": firstNonEmpty(errType, "upstream_error")}
	return resp
}

// hasEmittedToolCall 报告本回合最终产出里是否至少有一个 Codex 可识别的工具调用 item。
func (s *StreamState) hasEmittedToolCall() bool {
	for _, item := range s.outputItems {
		switch item["type"] {
		case "function_call", "custom_tool_call", "tool_search_call":
			return true
		}
	}
	return false
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
	idx, hasIdx := toolCallIndex(tc)
	if !hasIdx {
		// 上游省略 index：按 id / 最近分配槽位归位（对齐 cc-switch）。
		idx = s.resolveToolKeyWithoutIndex(tc)
	}
	state, ok := s.tools[idx]
	if !ok {
		state = &streamToolCall{}
		s.tools[idx] = state
		s.toolOrder = append(s.toolOrder, idx)
	}
	if id := rawString(tc, "id"); id != "" {
		state.callID = id
		s.toolIDIndex[id] = idx
	}
	fn, _ := tc["function"].(map[string]any)
	if name := rawString(fn, "name"); name != "" {
		state.name = name
	}
	if args := rawString(fn, "arguments"); args != "" {
		state.arguments.WriteString(args)
		// 已 added 的调用才发 arguments delta；未 added 的由 flushReadyToolCalls
		// 在释放时补发（避免分片乱序先于 output_item.added）。
		// custom 工具的 input 不流式发 delta（收尾时一次性发 custom_tool_call_input）。
		if state.added && !s.isCustom(state.name) {
			out.Write(sseEvent("response.function_call_arguments.delta", map[string]any{
				"item_id": state.itemID, "output_index": state.outputIndex, "delta": args,
			}))
		}
	}
	s.flushReadyToolCalls(out)
}

// flushReadyToolCalls 按 chat index 顺序释放「call_id 与 name 均已就绪」的工具调用
// （对齐 cc-switch flush_ready_tool_calls）：保证乱序到达的分片仍按 index 顺序发出
// output_item.added，且不会因身份未齐而提前 added。
func (s *StreamState) flushReadyToolCalls(out *bytes.Buffer) {
	for {
		key := s.nextToolIndexToAdd
		state, ok := s.tools[key]
		if !ok {
			break
		}
		if state.added || state.done {
			s.nextToolIndexToAdd++
			continue
		}
		if state.callID == "" || strings.TrimSpace(state.name) == "" {
			break
		}
		state.added = true
		state.outputIndex = s.allocIndex()
		state.itemID = toolCallItemID(state.callID, state.name, s.toolCtx)
		out.Write(sseEvent("response.output_item.added", map[string]any{
			"output_index": state.outputIndex,
			"item":         toolCallItem(state.callID, state.name, "", "", "in_progress", s.toolCtx),
		}))
		if state.arguments.Len() > 0 && !s.isCustom(state.name) {
			out.Write(sseEvent("response.function_call_arguments.delta", map[string]any{
				"item_id": state.itemID, "output_index": state.outputIndex, "delta": state.arguments.String(),
			}))
		}
		s.nextToolIndexToAdd++
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
		if state == nil || state.done {
			continue
		}
		// 缺函数名（空/纯空白）：对应不到任何已发布工具，丢弃并计数
		// （供 Finalize 的「答一句就停」防御判定）。
		if strings.TrimSpace(state.name) == "" {
			state.done = true
			s.droppedToolCalls++
			continue
		}
		callID := state.callID
		if callID == "" {
			callID = fmt.Sprintf("call_%d", idx)
		}
		// name 在后续分片才到、尚未发过 output_item.added 时补发。
		if !state.added {
			state.added = true
			state.outputIndex = s.allocIndex()
			state.itemID = toolCallItemID(callID, state.name, s.toolCtx)
			out.Write(sseEvent("response.output_item.added", map[string]any{
				"output_index": state.outputIndex,
				"item":         toolCallItem(callID, state.name, "", "", "in_progress", s.toolCtx),
			}))
		}
		arguments := state.arguments.String()
		item := toolCallItem(callID, state.name, arguments, "", "completed", s.toolCtx)
		if s.isCustom(state.name) {
			// custom 工具：收尾一次性发完整 input。
			input := customToolInputFromArguments(arguments)
			if input != "" {
				out.Write(sseEvent("response.custom_tool_call_input.delta", map[string]any{
					"item_id": state.itemID, "output_index": state.outputIndex, "delta": input,
				}))
			}
			out.Write(sseEvent("response.custom_tool_call_input.done", map[string]any{
				"item_id": state.itemID, "output_index": state.outputIndex, "input": input,
			}))
		} else {
			out.Write(sseEvent("response.function_call_arguments.done", map[string]any{
				"item_id": state.itemID, "output_index": state.outputIndex, "arguments": arguments,
			}))
		}
		out.Write(sseEvent("response.output_item.done", map[string]any{"output_index": state.outputIndex, "item": item}))
		s.outputItems = append(s.outputItems, item)
		state.done = true
	}
}

// isCustom 报告 Chat 工具名是否对应 custom 工具。
func (s *StreamState) isCustom(name string) bool {
	return s.toolCtx != nil && s.toolCtx.isCustom(name)
}

// toolCallIndex 取 Chat delta 的 tool_call index（缺省返回 false）。
func toolCallIndex(tc map[string]any) (int, bool) {
	if v, ok := tc["index"].(float64); ok {
		return int(v), true
	}
	return 0, false
}

// resolveToolKeyWithoutIndex 上游省略 index 时按「id 优先、最近分配槽位兜底」归位：
//   - 无 id → 并入最近的槽位（无则 0）；
//   - id 已见过 → 归位其既有槽位（延续分片）；
//   - id 是新的 → 分配 maxKey+1（不覆盖既有调用）；
//   - 无任何既有调用 → 0。
func (s *StreamState) resolveToolKeyWithoutIndex(tc map[string]any) int {
	id := stringField(tc, "id")
	if id == "" {
		if maxKey, ok := s.maxToolKey(); ok {
			return maxKey
		}
		return 0
	}
	if idx, ok := s.toolIDIndex[id]; ok {
		return idx
	}
	if maxKey, ok := s.maxToolKey(); ok {
		return maxKey + 1
	}
	return 0
}

// maxToolKey 返回已分配的最大工具槽位（无则 false）。
func (s *StreamState) maxToolKey() (int, bool) {
	maxKey := 0
	found := false
	for k := range s.tools {
		if !found || k > maxKey {
			maxKey = k
			found = true
		}
	}
	return maxKey, found
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
