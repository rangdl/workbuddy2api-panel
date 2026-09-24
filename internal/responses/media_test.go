package responses

import (
	"encoding/json"
	"strings"
	"testing"
)

func toChatMsgs(t *testing.T, body map[string]any) []any {
	t.Helper()
	rawBody, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	raw, _, err := ToChat(rawBody)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	msgs, _ := out["messages"].([]any)
	return msgs
}

// TestToolOutputMediaDataURLMoved 验证整串图片 data URL 被剥离并搬到合成 user 消息。
func TestToolOutputMediaDataURLMoved(t *testing.T) {
	dataURL := "data:image/png;base64," + strings.Repeat("A", 9000)
	msgs := toChatMsgs(t, map[string]any{
		"model": "m",
		"input": []any{
			map[string]any{"type": "function_call", "call_id": "c1", "name": "view_image", "arguments": "{}"},
			map[string]any{"type": "function_call_output", "call_id": "c1", "output": dataURL},
		},
	})
	if len(msgs) != 3 {
		t.Fatalf("expected assistant+tool+user, got %d (%v)", len(msgs), msgs)
	}
	tool, _ := msgs[1].(map[string]any)
	if tool["role"] != "tool" || tool["content"] != toolMediaMovedMarker {
		t.Errorf("tool message=%v", tool)
	}
	user, _ := msgs[2].(map[string]any)
	if user["role"] != "user" {
		t.Fatalf("third message should be synthetic user, got %v", user)
	}
	parts, _ := user["content"].([]any)
	if len(parts) < 2 {
		t.Fatalf("user media parts=%v", parts)
	}
	// 第一段是标注文本，其后是 image_url part。
	img, _ := parts[1].(map[string]any)
	if img["type"] != "image_url" {
		t.Fatalf("expected image_url part, got %v", img)
	}
	url, _ := img["image_url"].(map[string]any)
	if url["url"] != dataURL {
		t.Errorf("image url mismatch")
	}
}

// TestToolOutputMediaStructuredPartMoved 验证结构化 image part 被剥离。
func TestToolOutputMediaStructuredPartMoved(t *testing.T) {
	msgs := toChatMsgs(t, map[string]any{
		"model": "m",
		"input": []any{
			map[string]any{"type": "function_call", "call_id": "c1", "name": "view_image", "arguments": "{}"},
			map[string]any{"type": "function_call_output", "call_id": "c1", "output": map[string]any{
				"type":      "input_image",
				"image_url": "data:image/png;base64,AAAA",
			}},
		},
	})
	if len(msgs) != 3 {
		t.Fatalf("expected assistant+tool+user, got %d (%v)", len(msgs), msgs)
	}
	tool, _ := msgs[1].(map[string]any)
	if !strings.Contains(tool["content"].(string), toolMediaMovedMarker) {
		t.Errorf("tool content should carry marker: %v", tool["content"])
	}
	user, _ := msgs[2].(map[string]any)
	parts, _ := user["content"].([]any)
	found := false
	for _, p := range parts {
		if pm, ok := p.(map[string]any); ok && pm["type"] == "image_url" {
			found = true
		}
	}
	if !found {
		t.Errorf("no image_url part in user media: %v", parts)
	}
}

// TestToolOutputNoMediaUnchanged 验证无媒体时行为零改动（不产生合成 user 消息）。
func TestToolOutputNoMediaUnchanged(t *testing.T) {
	msgs := toChatMsgs(t, map[string]any{
		"model": "m",
		"input": []any{
			map[string]any{"type": "function_call", "call_id": "c1", "name": "shell", "arguments": "{}"},
			map[string]any{"type": "function_call_output", "call_id": "c1", "output": "plain text output"},
		},
	})
	if len(msgs) != 2 {
		t.Fatalf("no media should not add user message, got %d (%v)", len(msgs), msgs)
	}
	tool, _ := msgs[1].(map[string]any)
	if tool["content"] != "plain text output" {
		t.Errorf("tool content changed: %v", tool["content"])
	}
}

// TestToolOutputShortDataURLNotMoved 验证短 data URL 不被误判为图片（低于阈值）。
func TestToolOutputShortDataURLNotMoved(t *testing.T) {
	short := "data:image/png;base64,AAAA"
	msgs := toChatMsgs(t, map[string]any{
		"model": "m",
		"input": []any{
			map[string]any{"type": "function_call", "call_id": "c1", "name": "shell", "arguments": "{}"},
			map[string]any{"type": "function_call_output", "call_id": "c1", "output": short},
		},
	})
	if len(msgs) != 2 {
		t.Fatalf("short data url should stay inline, got %d (%v)", len(msgs), msgs)
	}
}
