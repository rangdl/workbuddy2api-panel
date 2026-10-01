package anthropic

import (
	"encoding/json"
	"strings"
	"testing"
)

// parseAnthropicSSE 把 SSE 字节流解析为 (event, data 对象) 列表。
type ssePair struct {
	event string
	data  map[string]any
}

func parseAnthropicSSE(t *testing.T, raw []byte) []ssePair {
	t.Helper()
	var out []ssePair
	for _, block := range strings.Split(string(raw), "\n\n") {
		var ev, dataLine string
		for _, line := range strings.Split(block, "\n") {
			line = strings.TrimRight(line, "\r")
			if strings.HasPrefix(line, "event: ") {
				ev = strings.TrimPrefix(line, "event: ")
			}
			if strings.HasPrefix(line, "data: ") {
				dataLine = strings.TrimPrefix(line, "data: ")
			}
		}
		if ev == "" || dataLine == "" {
			continue
		}
		var m map[string]any
		if json.Unmarshal([]byte(dataLine), &m) == nil {
			out = append(out, ssePair{ev, m})
		}
	}
	return out
}

func eventTypes(events []ssePair) []string {
	types := make([]string, 0, len(events))
	for _, e := range events {
		types = append(types, e.event)
	}
	return types
}

func chatChunk(delta map[string]any, finish string) map[string]any {
	choice := map[string]any{"index": 0, "delta": delta}
	if finish != "" {
		choice["finish_reason"] = finish
	}
	return map[string]any{
		"id": "chat-1", "model": "m", "created": float64(1),
		"choices": []any{choice},
	}
}

// 纯文本流 → message_start / text 块三事件 / message_delta / message_stop。
func TestStreamTextOnly(t *testing.T) {
	st := NewStreamState()
	var raw []byte
	raw = append(raw, st.HandleChunk(chatChunk(map[string]any{"content": "Hel"}, ""))...)
	raw = append(raw, st.HandleChunk(chatChunk(map[string]any{"content": "lo"}, "stop"))...)
	raw = append(raw, st.Finalize()...)

	events := parseAnthropicSSE(t, raw)
	types := eventTypes(events)
	want := []string{"message_start", "content_block_start", "content_block_delta", "content_block_delta", "content_block_stop", "message_delta", "message_stop"}
	if join(types) != join(want) {
		t.Fatalf("events=%v, want %v", types, want)
	}
	// 文本增量拼接
	var text strings.Builder
	for _, e := range events {
		if e.event == "content_block_delta" {
			d, _ := e.data["delta"].(map[string]any)
			if d["type"] == "text_delta" {
				text.WriteString(d["text"].(string))
			}
		}
	}
	if text.String() != "Hello" {
		t.Errorf("text=%q", text.String())
	}
	// message_delta 的 stop_reason
	var md map[string]any
	for _, e := range events {
		if e.event == "message_delta" {
			md = e.data
		}
	}
	delta, _ := md["delta"].(map[string]any)
	if delta["stop_reason"] != "end_turn" {
		t.Errorf("stop_reason=%v", delta["stop_reason"])
	}
}

// 文本+工具混合流：块互斥切换、tool_use 两段式、stop_reason=tool_use。
func TestStreamTextThenToolUse(t *testing.T) {
	st := NewStreamState()
	var raw []byte
	raw = append(raw, st.HandleChunk(chatChunk(map[string]any{"content": "Let me check."}, ""))...)
	chunkA := chatChunk(map[string]any{"tool_calls": []any{
		map[string]any{"index": 0, "id": "call_1", "type": "function",
			"function": map[string]any{"name": "shell", "arguments": ""}},
	}}, "")
	raw = append(raw, st.HandleChunk(chunkA)...)
	chunkB := chatChunk(map[string]any{"tool_calls": []any{
		map[string]any{"index": 0, "function": map[string]any{"arguments": `{"cmd":"ls"}`}},
	}}, "tool_calls")
	raw = append(raw, st.HandleChunk(chunkB)...)
	raw = append(raw, st.Finalize()...)

	events := parseAnthropicSSE(t, raw)
	// 找 tool_use 的 content_block_start：input 为空对象，name/id 正确
	var toolStart map[string]any
	for _, e := range events {
		if e.event == "content_block_start" {
			cb, _ := e.data["content_block"].(map[string]any)
			if cb["type"] == "tool_use" {
				toolStart = cb
			}
		}
	}
	if toolStart == nil {
		t.Fatal("no tool_use content_block_start")
	}
	if toolStart["name"] != "shell" || toolStart["id"] != "call_1" {
		t.Errorf("tool_start=%v", toolStart)
	}
	// input_json_delta 拼出参数
	var args strings.Builder
	for _, e := range events {
		if e.event == "content_block_delta" {
			d, _ := e.data["delta"].(map[string]any)
			if d["type"] == "input_json_delta" {
				args.WriteString(d["partial_json"].(string))
			}
		}
	}
	if args.String() != `{"cmd":"ls"}` {
		t.Errorf("args=%q", args.String())
	}
	// stop_reason=tool_use
	for _, e := range events {
		if e.event == "message_delta" {
			d, _ := e.data["delta"].(map[string]any)
			if d["stop_reason"] != "tool_use" {
				t.Errorf("stop_reason=%v", d["stop_reason"])
			}
		}
	}
	// message_delta 只出现一次
	count := 0
	for _, ty := range eventTypes(events) {
		if ty == "message_delta" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("message_delta count=%d, must be exactly 1", count)
	}
}

// 多个 finish_reason chunk（OpenRouter kimi-k2.6 形态）：message_delta 仍只有一个。
func TestStreamDuplicateFinishReason(t *testing.T) {
	st := NewStreamState()
	var raw []byte
	raw = append(raw, st.HandleChunk(chatChunk(map[string]any{"content": "x"}, "tool_calls"))...)
	raw = append(raw, st.HandleChunk(chatChunk(map[string]any{}, "tool_calls"))...)
	raw = append(raw, st.HandleChunk(chatChunk(map[string]any{}, "stop"))...) // 迟到的重复
	raw = append(raw, st.Finalize()...)

	count := 0
	for _, e := range parseAnthropicSSE(t, raw) {
		if e.event == "message_delta" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("message_delta count=%d, must be exactly 1 (dup finish_reason must be deduped)", count)
	}
}

// 异常断流：Failed 补收尾事件（CC 不会静默卡死）。
func TestStreamFailedCompletes(t *testing.T) {
	st := NewStreamState()
	var raw []byte
	raw = append(raw, st.HandleChunk(chatChunk(map[string]any{"content": "partial"}, ""))...)
	raw = append(raw, st.Failed()...)
	// 再 Finalize 不产生重复（幂等）
	if extra := st.Finalize(); extra != nil {
		t.Errorf("Finalize after Failed must be nil, got %d bytes", len(extra))
	}
	events := parseAnthropicSSE(t, raw)
	types := eventTypes(events)
	if len(types) < 2 {
		t.Fatalf("events=%v", types)
	}
	if types[len(types)-2] != "message_delta" || types[len(types)-1] != "message_stop" {
		t.Errorf("tail=%v, want ...message_delta,message_stop", types)
	}
}

// Finalize 幂等：重复调用返回 nil。
func TestFinalizeIdempotent(t *testing.T) {
	st := NewStreamState()
	st.HandleChunk(chatChunk(map[string]any{"content": "hi"}, "stop"))
	if first := st.Finalize(); first == nil {
		t.Fatal("first Finalize must produce events")
	} else if again := st.Finalize(); again != nil {
		t.Error("second Finalize must be nil")
	}
}

// usage 末帧覆盖：message_delta 的 output_tokens 取末帧 usage。
func TestStreamUsageFromLastFrame(t *testing.T) {
	st := NewStreamState()
	var raw []byte
	chunk1 := chatChunk(map[string]any{"content": "hi"}, "stop")
	chunk2 := chatChunk(map[string]any{}, "stop")
	chunk2["usage"] = map[string]any{"prompt_tokens": 10, "completion_tokens": 7}
	raw = append(raw, st.HandleChunk(chunk1)...)
	raw = append(raw, st.HandleChunk(chunk2)...)
	raw = append(raw, st.Finalize()...)
	for _, e := range parseAnthropicSSE(t, raw) {
		if e.event == "message_delta" {
			u, _ := e.data["usage"].(map[string]any)
			if u["output_tokens"] != float64(7) {
				t.Errorf("output_tokens=%v, want 7", u["output_tokens"])
			}
		}
	}
}

// 缺名工具分片：身份未齐不发 start；收尾时丢弃（畸形防御）。
func TestStreamNamelessToolDropped(t *testing.T) {
	st := NewStreamState()
	var raw []byte
	chunk := chatChunk(map[string]any{"tool_calls": []any{
		map[string]any{"index": 0, "id": "call_x", "type": "function",
			"function": map[string]any{"arguments": "{}"}},
	}}, "tool_calls")
	raw = append(raw, st.HandleChunk(chunk)...)
	raw = append(raw, st.Finalize()...)
	for _, e := range parseAnthropicSSE(t, raw) {
		if e.event == "content_block_start" {
			cb, _ := e.data["content_block"].(map[string]any)
			if cb["type"] == "tool_use" {
				t.Errorf("nameless tool block must not start: %v", cb)
			}
		}
	}
}

func join(items []string) string {
	return strings.Join(items, ",")
}
