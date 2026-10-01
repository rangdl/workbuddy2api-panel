package anthropic

import (
	"bytes"
	"encoding/json"
	"strings"
)

// StreamState 把 Chat Completions SSE 帧转换为 Anthropic Messages SSE 事件。
//
// 蓝本：cc-switch streaming.rs create_anthropic_sse_stream（149 行起）。
// 用法：对每个 Chat chunk 调 HandleChunk 取得要写出的字节；流结束时调 Finalize
// 收尾（message_delta + message_stop）。结构照抄 internal/responses.StreamState
// （HandleChunk / Finalize / Failed / ensureStarted 幂等）。
//
// 实测坑（全部来自蓝本注释，见方案 §5.2）：
//   - message_delta 只能发一次：部分上游在 tool_use 后发多个带 finish_reason 的
//     chunk，Anthropic 协议要求每流一个 message_delta，重复会导致 Claude Code
//     直接 abort 连接。策略：缓存 stop_reason+usage，[DONE] 时一次性发出；
//   - tool_use 块两段式：content_block_start 带 name，参数经 input_json_delta
//     分片累积；Chat 侧 name 只在首片出现，沿用「call_id 与 name 均就绪才发
//     start」的顺序保护（responses 包 flushReadyToolCalls 同款）；
//   - block 类型互斥：文本/思考/工具块切换时先 close 当前块再开新块。
type StreamState struct {
	responseID string
	model      string

	// 消息级事件状态。
	started             bool   // message_start 已发（幂等）
	messageDeltaPending bool   // 有待发的 message_delta（stop_reason/usage 已缓存）
	stopReason          string // 最近一次 finish_reason（首个胜出，与蓝本去重一致）
	usage               map[string]any

	// content block 状态：index 连续递增；同刻至多一个打开的非工具块。
	nextIndex     int
	openBlock     int // 当前打开的非工具块 index（-1 = 无）
	openBlockType string

	// 文本块。
	textOpen        bool
	textBuf         strings.Builder
	textClosed      bool // 文本块已关闭后不可再开（Anthropic 语义一惯：后续文本另开块）
	textBlockIndex  int

	// 思考块。
	thinkingOpen        bool
	thinkingBuf         strings.Builder
	thinkingClosed      bool
	thinkingBlockIndex  int

	// 工具块：Chat index → 状态。
	tools              map[int]*streamToolBlock
	toolOrder          []int
	toolIDIndex        map[string]int
	nextToolIndexToAdd int
	droppedToolCalls   int // 缺名调用计数（收尾防御）

	startedAt int64
	completed bool
}

// streamToolBlock 一个流式工具块的累积状态。
type streamToolBlock struct {
	blockIndex int    // Anthropic content block index
	callID     string // Chat call id（即 Anthropic tool_use.id）
	name       string
	arguments  strings.Builder
	sent       int    // 已通过 input_json_delta 发出的字节数（增量发送）
	blockOpen  bool   // content_block_start 已发
	done       bool
}

// NewStreamState 构造流式转换状态机。
func NewStreamState() *StreamState {
	return &StreamState{
		openBlock:          -1,
		tools:              map[int]*streamToolBlock{},
		toolIDIndex:        map[string]int{},
		messageDeltaPending: false,
	}
}

// HandleChunk 处理一个 Chat chunk，返回需要写出的 Anthropic SSE 事件字节。
func (s *StreamState) HandleChunk(chunk map[string]any) []byte {
	var out bytes.Buffer
	if id := rawString(chunk, "id"); id != "" {
		s.responseID = id
	}
	if model := rawString(chunk, "model"); model != "" {
		s.model = model
	}
	s.ensureStarted(&out)

	if usage, ok := chunk["usage"].(map[string]any); ok && usage != nil {
		s.usage = usage // 末帧覆盖（include_usage 语义）
	}

	choices, _ := chunk["choices"].([]any)
	if len(choices) == 0 {
		return out.Bytes()
	}
	choice, _ := choices[0].(map[string]any)

	if delta, ok := choice["delta"].(map[string]any); ok {
		if reasoning := rawString(delta, "reasoning_content"); reasoning != "" {
			s.pushThinking(&out, reasoning)
		}
		if content := rawString(delta, "content"); content != "" {
			s.pushText(&out, content)
		}
		if tcs, ok := delta["tool_calls"].([]any); ok {
			for _, raw := range tcs {
				if tc, ok := raw.(map[string]any); ok {
					s.pushToolCall(&out, tc)
				}
			}
		}
	}
	// 多个 finish_reason chunk：首个胜出（蓝本 has_emitted_message_delta 去重语义）。
	if fr := rawString(choice, "finish_reason"); fr != "" && s.stopReason == "" {
		s.stopReason = fr
		s.messageDeltaPending = true
	}
	return out.Bytes()
}

// Finalize 收尾：关闭所有打开的块 + flush message_delta + message_stop（幂等）。
func (s *StreamState) Finalize() []byte {
	if s.completed {
		return nil
	}
	var out bytes.Buffer
	s.ensureStarted(&out)
	s.closeThinking(&out)
	s.closeText(&out)
	s.finalizeTools(&out)

	// message_delta：单发（从缓存 flush）。无 finish_reason 的空流按 end_turn 处理。
	// usage 必须透传 anthropicUsage() 的完整四桶（input/cache_read/cache_creation/
	// output）——CC 以 message_delta 的 usage 记账，丢掉 input 侧桶会让 CC 转写里
	// 输入计数全 0（会话 #106 实测：CC 转写 192 条 assistant 记录 input 侧全 0，
	// 上下文仪表空白；根因即此处只发了 output_tokens）。
	stopReason := stopReasonOf(s.stopReason, s.hasToolUse())
	usage := s.anthropicUsage()
	out.Write(sseEvent("message_delta", map[string]any{
		"type":  "message_delta",
		"delta": map[string]any{"stop_reason": stopReason, "stop_sequence": nil},
		"usage": usage,
	}))
	out.Write(sseEvent("message_stop", map[string]any{"type": "message_stop"}))
	s.completed = true
	return out.Bytes()
}

// Failed 流异常时发出可终止的收尾（stop_reason=end_turn + message_stop），
// 让 CC 明确流已结束而不是静默卡死（方案 §5.2 第 5 条）。
func (s *StreamState) Failed() []byte {
	if s.completed {
		return nil
	}
	var out bytes.Buffer
	s.ensureStarted(&out)
	s.closeThinking(&out)
	s.closeText(&out)
	s.finalizeTools(&out)
	out.Write(sseEvent("message_delta", map[string]any{
		"type":  "message_delta",
		"delta": map[string]any{"stop_reason": "end_turn", "stop_sequence": nil},
		"usage": s.anthropicUsage(), // 同 Finalize：完整四桶（异常断流也如实记账）
	}))
	out.Write(sseEvent("message_stop", map[string]any{"type": "message_stop"}))
	s.completed = true
	return out.Bytes()
}

// hasToolUse 报告流中是否已出现至少一个有效工具块（stop_reason 判定用）。
func (s *StreamState) hasToolUse() bool {
	for _, tb := range s.tools {
		if strings.TrimSpace(tb.name) != "" {
			return true
		}
	}
	return false
}

// ---- 内部：事件发射 ----

func (s *StreamState) ensureStarted(out *bytes.Buffer) {
	if s.started {
		return
	}
	s.started = true
	out.Write(sseEvent("message_start", map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id":            messageIDOf(s.responseID),
			"type":          "message",
			"role":          "assistant",
			"model":         s.model,
			"content":       []any{},
			"stop_reason":   nil,
			"stop_sequence": nil,
			"usage":         map[string]any{"input_tokens": 0, "output_tokens": 0},
		},
	}))
}

// closeOpenBlock 关闭当前打开的非工具块（互斥切换用）。
func (s *StreamState) closeOpenBlock(out *bytes.Buffer) {
	if s.openBlock < 0 {
		return
	}
	switch s.openBlockType {
	case "text":
		s.closeText(out)
	case "thinking":
		s.closeThinking(out)
	}
}

func (s *StreamState) pushThinking(out *bytes.Buffer, delta string) {
	if s.thinkingClosed {
		return // 已收尾的思考块不再追加（防御异常流）
	}
	if !s.thinkingOpen {
		s.closeOpenBlock(out) // 互斥：先关文本块
		s.thinkingOpen = true
		s.openBlockType = "thinking"
		idx := s.allocIndex()
		s.openBlock = idx
		s.thinkingBlockIndex = idx
		out.Write(sseEvent("content_block_start", map[string]any{
			"type": "content_block_start", "index": idx,
			"content_block": map[string]any{"type": "thinking", "thinking": ""},
		}))
	}
	s.thinkingBuf.WriteString(delta)
	out.Write(sseEvent("content_block_delta", map[string]any{
		"type": "content_block_delta", "index": s.openBlock,
		"delta": map[string]any{"type": "thinking_delta", "thinking": delta},
	}))
}

func (s *StreamState) closeThinking(out *bytes.Buffer) {
	if !s.thinkingOpen {
		return
	}
	s.thinkingOpen = false
	s.thinkingClosed = true
	s.openBlock = -1
	out.Write(sseEvent("content_block_stop", map[string]any{
		"type": "content_block_stop", "index": s.blockIndexOf("thinking"),
	}))
}

func (s *StreamState) pushText(out *bytes.Buffer, delta string) {
	if s.textClosed {
		return
	}
	if !s.textOpen {
		s.closeOpenBlock(out)
		s.textOpen = true
		s.openBlockType = "text"
		idx := s.allocIndex()
		s.openBlock = idx
		s.textBlockIndex = idx
		out.Write(sseEvent("content_block_start", map[string]any{
			"type": "content_block_start", "index": idx,
			"content_block": map[string]any{"type": "text", "text": ""},
		}))
	}
	s.textBuf.WriteString(delta)
	out.Write(sseEvent("content_block_delta", map[string]any{
		"type": "content_block_delta", "index": s.openBlock,
		"delta": map[string]any{"type": "text_delta", "text": delta},
	}))
}

func (s *StreamState) closeText(out *bytes.Buffer) {
	if !s.textOpen {
		return
	}
	s.textOpen = false
	s.textClosed = true
	s.openBlock = -1
	out.Write(sseEvent("content_block_stop", map[string]any{
		"type": "content_block_stop", "index": s.blockIndexOf("text"),
	}))
}

func (s *StreamState) pushToolCall(out *bytes.Buffer, tc map[string]any) {
	idx, hasIdx := toolCallIndex(tc)
	if !hasIdx {
		idx = s.resolveToolKeyWithoutIndex(tc)
	}
	state, ok := s.tools[idx]
	if !ok {
		state = &streamToolBlock{}
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
	}
	// 互斥：工具块开始前关闭非工具块（只关一次，首个工具就绪时）。
	s.flushReadyToolBlocks(out)
}

// flushReadyToolBlocks 按 Chat index 顺序释放「call_id 与 name 均就绪」的工具块
// （与 responses 包 flushReadyToolCalls 同款顺序保护）。
func (s *StreamState) flushReadyToolBlocks(out *bytes.Buffer) {
	for {
		key := s.nextToolIndexToAdd
		state, ok := s.tools[key]
		if !ok {
			break
		}
		if state.done {
			s.nextToolIndexToAdd++
			continue
		}
		if state.callID == "" || strings.TrimSpace(state.name) == "" {
			break // 身份未齐：等待后续分片
		}
		if !state.blockOpen {
			s.closeText(out) // 工具块前关闭文本/思考块
			s.closeThinking(out)
			state.blockOpen = true
			state.blockIndex = s.allocIndex()
			out.Write(sseEvent("content_block_start", map[string]any{
				"type": "content_block_start", "index": state.blockIndex,
				"content_block": map[string]any{
					"type": "tool_use", "id": state.callID, "name": state.name, "input": map[string]any{},
				},
			}))
		}
		s.flushNewArguments(out, state)
		s.nextToolIndexToAdd++
	}
}

func (s *StreamState) finalizeTools(out *bytes.Buffer) {
	for _, idx := range s.toolOrder {
		state := s.tools[idx]
		if state == nil || state.done {
			continue
		}
		// 缺名调用：丢弃并计数（畸形上游防御；与 responses 包同口径）。
		if strings.TrimSpace(state.name) == "" {
			state.done = true
			s.droppedToolCalls++
			continue
		}
		callID := state.callID
		if callID == "" {
			callID = "toolu_wb2api_fallback"
		}
		if !state.blockOpen {
			state.blockOpen = true
			state.blockIndex = s.allocIndex()
			out.Write(sseEvent("content_block_start", map[string]any{
				"type": "content_block_start", "index": state.blockIndex,
				"content_block": map[string]any{
					"type": "tool_use", "id": callID, "name": state.name, "input": map[string]any{},
				},
			}))
		}
		s.flushNewArguments(out, state)
		out.Write(sseEvent("content_block_stop", map[string]any{
			"type": "content_block_stop", "index": state.blockIndex,
		}))
		state.done = true
	}
}

// blockIndexOf 返回对应块的 Anthropic block index（alloc 时分配的 nextIndex-1）。
// text/thinking 各只有一个块，取分配序即可。
func (s *StreamState) blockIndexOf(typ string) int {
	// allocIndex 在 push 时已经递增 nextIndex；块的 index = 分配时值。
	// 这里直接回溯：text/thinking 的 index 在各自 push 里已存为 openBlock
	// 分配值，但 close 时 openBlock 已清——存到字段更稳。
	if typ == "text" {
		return s.textBlockIndex
	}
	return s.thinkingBlockIndex
}

// allocIndex 分配下一个 block index。
func (s *StreamState) allocIndex() int {
	i := s.nextIndex
	s.nextIndex++
	return i
}

// resolveToolKeyWithoutIndex 上游省略 index 时按「id 优先、最近槽位兜底」归位
// （与 responses 包同款逻辑）。
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

// anthropicUsage 把缓存的 Chat usage 转成 Anthropic usage（复用 FromChat 的减法）。
func (s *StreamState) anthropicUsage() map[string]any {
	return usageOf(s.usage)
}

// toolCallIndex 取 Chat delta 的 tool_call index（缺省返回 false）。
func toolCallIndex(tc map[string]any) (int, bool) {
	if v, ok := tc["index"].(float64); ok {
		return int(v), true
	}
	return 0, false
}

// sseEvent 构造一个 Anthropic SSE 事件（event: 行 + data: 行）。
func sseEvent(eventType string, data map[string]any) []byte {
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

// flushNewArguments 把 state 中尚未发出的 arguments 增量经 input_json_delta 发出
// （蓝本按 delta 增量发；首个实现误发全量导致 CC 端 JSON 拼接重复、解析失败——
// 真实 Claude Code 端到端捕获，见 docs/anthropic-messages-plan.md §5.2）。
func (s *StreamState) flushNewArguments(out *bytes.Buffer, state *streamToolBlock) {
	args := state.arguments.String()
	if len(args) <= state.sent {
		return
	}
	delta := args[state.sent:]
	state.sent = len(args)
	out.Write(sseEvent("content_block_delta", map[string]any{
		"type": "content_block_delta", "index": state.blockIndex,
		"delta": map[string]any{"type": "input_json_delta", "partial_json": delta},
	}))
}
