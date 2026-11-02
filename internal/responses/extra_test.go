package responses

import (
	"encoding/json"
	"strings"
	"testing"
)

// 孤儿结果延后到回合边界输出：不会夹在并行工具结果中间形成 tool,user,tool。
func TestOrphanDeferredUntilBoundary(t *testing.T) {
	raw, _, err := ToChat([]byte(`{"model":"gpt-5.5","input":[
	  {"type":"function_call","call_id":"c1","name":"a","arguments":"{}"},
	  {"type":"function_call_output","call_id":"orphan","output":"lost"},
	  {"type":"function_call_output","call_id":"c1","output":"ok"}
	]}`))
	if err != nil {
		t.Fatal(err)
	}
	out := decode(t, raw)
	msgs, _ := out["messages"].([]any)
	roles := make([]string, 0, len(msgs))
	for _, raw := range msgs {
		msg, _ := raw.(map[string]any)
		role, _ := msg["role"].(string)
		roles = append(roles, role)
	}
	want := []string{"assistant", "tool", "user"}
	if strings.Join(roles, ",") != strings.Join(want, ",") {
		t.Fatalf("roles=%v, want %v", roles, want)
	}
}

// typed image（Anthropic 风格）也能识别；媒体搬走后残留长 base64 被裁剪。
func TestToolMediaTypedImageAndClamp(t *testing.T) {
	longB64 := strings.Repeat("A", 20*1024)
	content, media := planToolOutputMedia([]any{
		map[string]any{"type": "image", "detail": "original",
			"source": map[string]any{"type": "base64", "media_type": "image/png", "data": "abc"}},
		map[string]any{"type": "text", "text": longB64},
	})
	if len(media) != 1 {
		t.Fatalf("media=%v", media)
	}
	part := media[0].(map[string]any)
	imageURL := part["image_url"].(map[string]any)
	if imageURL["detail"] != "auto" {
		t.Errorf("detail 应降级 auto: %v", imageURL)
	}
	if imageURL["url"] != "data:image/png;base64,abc" {
		t.Errorf("url=%v", imageURL["url"])
	}
	if strings.Contains(content, longB64) {
		t.Errorf("残留 base64 未裁剪")
	}
	if !strings.Contains(content, "omitted") {
		t.Errorf("缺少裁剪标记: %s", content)
	}
}

// custom_tool_call_output 的 tool content 是「整条 item 的 JSON」（参考口径）。
func TestCustomToolOutputWholeItemJSON(t *testing.T) {
	raw, _, err := ToChat([]byte(`{"model":"gpt-5.5","input":[
	  {"type":"custom_tool_call","call_id":"c1","name":"apply_patch","input":"patch"},
	  {"type":"custom_tool_call_output","call_id":"c1","output":"applied"}
	]}`))
	if err != nil {
		t.Fatal(err)
	}
	out := decode(t, raw)
	msgs, _ := out["messages"].([]any)
	toolMsg, _ := msgs[len(msgs)-1].(map[string]any)
	content, _ := toolMsg["content"].(string)
	var parsed map[string]any
	if err := json.Unmarshal([]byte(content), &parsed); err != nil {
		t.Fatalf("tool content 不是 JSON: %q", content)
	}
	if parsed["type"] != "custom_tool_call_output" || parsed["output"] != "applied" {
		t.Errorf("tool content=%v", parsed)
	}
}

// MiniMax base_resp 错误体可识别并保留 code。
func TestErrorToResponsesBaseResp(t *testing.T) {
	out := ErrorToResponses([]byte(`{"base_resp":{"status_code":2013,"status_msg":"invalid params"}}`))
	errObj, _ := out["error"].(map[string]any)
	if errObj["message"] != "invalid params" {
		t.Errorf("message=%v", errObj["message"])
	}
	if errObj["code"] != float64(2013) {
		t.Errorf("code=%v", errObj["code"])
	}
	if !LooksLikeError(map[string]any{"base_resp": map[string]any{}}) {
		t.Error("LooksLikeError 未识别 base_resp")
	}
}

// 无 tools 时 parallel_tool_calls 一并剔除。
func TestParallelToolCallsDroppedWithoutTools(t *testing.T) {
	raw, _, err := ToChat([]byte(`{"model":"gpt-5.5","parallel_tool_calls":true,"input":"hi"}`))
	if err != nil {
		t.Fatal(err)
	}
	out := decode(t, raw)
	if _, has := out["parallel_tool_calls"]; has {
		t.Errorf("无 tools 时不应下发 parallel_tool_calls: %v", out["parallel_tool_calls"])
	}
}

// 裸 input_text item 不再丢弃。
func TestBareInputTextItemKept(t *testing.T) {
	raw, _, err := ToChat([]byte(`{"model":"gpt-5.5","input":[
	  {"type":"input_text","text":"bare text"}
	]}`))
	if err != nil {
		t.Fatal(err)
	}
	out := decode(t, raw)
	msgs, _ := out["messages"].([]any)
	if len(msgs) != 1 {
		t.Fatalf("messages=%v", msgs)
	}
	msg, _ := msgs[0].(map[string]any)
	if msg["role"] != "user" || !strings.Contains(messageText(msg), "bare text") {
		t.Errorf("裸 input_text 丢失: %v", msg)
	}
}
