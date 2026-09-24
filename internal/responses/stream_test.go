package responses

import (
	"bytes"
	"strings"
	"testing"
)

func chunk(delta map[string]any, finishReason string, usage map[string]any) map[string]any {
	choice := map[string]any{"index": float64(0), "delta": delta}
	if finishReason != "" {
		choice["finish_reason"] = finishReason
	}
	c := map[string]any{"id": "chatcmpl-1", "model": "glm-5.2", "created": float64(100), "choices": []any{choice}}
	if usage != nil {
		c["usage"] = usage
	}
	return c
}

func TestStreamTextSequence(t *testing.T) {
	s := NewStreamState()
	var buf bytes.Buffer
	buf.Write(s.HandleChunk(chunk(map[string]any{"role": "assistant", "content": "你好"}, "", nil)))
	buf.Write(s.HandleChunk(chunk(map[string]any{}, "stop", map[string]any{
		"prompt_tokens": float64(1), "completion_tokens": float64(1), "total_tokens": float64(2),
	})))
	buf.Write(s.Finalize())
	out := buf.String()

	for _, ev := range []string{
		"event: response.created",
		"event: response.in_progress",
		"event: response.output_item.added",
		"event: response.content_part.added",
		"event: response.output_text.delta",
		"event: response.output_text.done",
		"event: response.content_part.done",
		"event: response.output_item.done",
		"event: response.completed",
	} {
		if !strings.Contains(out, ev) {
			t.Errorf("missing %q\n%s", ev, out)
		}
	}
	if !strings.Contains(out, `"delta":"你好"`) {
		t.Errorf("text delta missing: %s", out)
	}
	if !strings.Contains(out, `"input_tokens":1`) {
		t.Errorf("usage missing: %s", out)
	}
	items := s.OutputItems()
	if len(items) != 1 {
		t.Fatalf("items=%d", len(items))
	}
	msg := items[0].(map[string]any)
	if msg["type"] != "message" {
		t.Errorf("item=%v", msg)
	}
}

func TestStreamToolCallSequence(t *testing.T) {
	s := NewStreamState()
	var buf bytes.Buffer
	buf.Write(s.HandleChunk(chunk(map[string]any{"tool_calls": []any{
		map[string]any{"index": float64(0), "id": "call_1", "type": "function",
			"function": map[string]any{"name": "shell", "arguments": ""}},
	}}, "", nil)))
	buf.Write(s.HandleChunk(chunk(map[string]any{"tool_calls": []any{
		map[string]any{"index": float64(0), "function": map[string]any{"arguments": `{"cmd":`}},
	}}, "", nil)))
	buf.Write(s.HandleChunk(chunk(map[string]any{"tool_calls": []any{
		map[string]any{"index": float64(0), "function": map[string]any{"arguments": `"ls"}`}},
	}}, "tool_calls", nil)))
	buf.Write(s.Finalize())
	out := buf.String()

	if !strings.Contains(out, "event: response.function_call_arguments.delta") {
		t.Errorf("missing args delta\n%s", out)
	}
	if !strings.Contains(out, "event: response.function_call_arguments.done") {
		t.Errorf("missing args done\n%s", out)
	}
	items := s.OutputItems()
	if len(items) != 1 {
		t.Fatalf("items=%d (%v)", len(items), items)
	}
	fc := items[0].(map[string]any)
	if fc["type"] != "function_call" || fc["name"] != "shell" || fc["call_id"] != "call_1" {
		t.Errorf("tool item=%v", fc)
	}
	if fc["arguments"] != `{"cmd":"ls"}` {
		t.Errorf("arguments=%v", fc["arguments"])
	}
}

func TestStreamReasoningSequence(t *testing.T) {
	s := NewStreamState()
	var buf bytes.Buffer
	buf.Write(s.HandleChunk(chunk(map[string]any{"reasoning_content": "思考中"}, "", nil)))
	buf.Write(s.HandleChunk(chunk(map[string]any{"content": "答案"}, "stop", nil)))
	buf.Write(s.Finalize())
	out := buf.String()

	for _, ev := range []string{
		"event: response.output_item.added",
		"event: response.reasoning_summary_part.added",
		"event: response.reasoning_summary_text.delta",
		"event: response.reasoning_summary_text.done",
		"event: response.reasoning_summary_part.done",
		"event: response.output_text.delta",
		"event: response.completed",
	} {
		if !strings.Contains(out, ev) {
			t.Errorf("missing %q\n%s", ev, out)
		}
	}
	items := s.OutputItems()
	if len(items) != 2 {
		t.Fatalf("items=%d (%v)", len(items), items)
	}
	if items[0].(map[string]any)["type"] != "reasoning" {
		t.Errorf("first item=%v", items[0])
	}
	if items[1].(map[string]any)["type"] != "message" {
		t.Errorf("second item=%v", items[1])
	}
}

func TestStreamFailed(t *testing.T) {
	s := NewStreamState()
	s.HandleChunk(chunk(map[string]any{"content": "partial"}, "", nil))
	out := string(s.Failed("boom", "upstream_error"))
	if !strings.Contains(out, "event: response.failed") {
		t.Errorf("missing failed event: %s", out)
	}
	if strings.Contains(out, "event: response.completed") {
		t.Errorf("should not complete: %s", out)
	}
}

func TestStreamFinalizeIdempotent(t *testing.T) {
	s := NewStreamState()
	s.HandleChunk(chunk(map[string]any{"content": "hi"}, "stop", nil))
	first := s.Finalize()
	second := s.Finalize()
	if len(first) == 0 {
		t.Fatal("first finalize empty")
	}
	if len(second) != 0 {
		t.Errorf("second finalize should be empty: %s", second)
	}
}
