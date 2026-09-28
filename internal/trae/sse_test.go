package trae

import (
	"encoding/json"
	"strings"
	"testing"
)

// runConvert 跑一遍流式转换，返回原始帧与结果统计。
func runConvert(t *testing.T, sse string) (string, StreamResult) {
	t.Helper()
	var sb strings.Builder
	res, err := ConvertStream(strings.NewReader(sse), "chatcmpl-test", "glm-5.2", func(b []byte) error {
		sb.Write(b)
		return nil
	})
	if err != nil {
		t.Fatalf("ConvertStream: %v", err)
	}
	return sb.String(), res
}

// deltas 从 SSE 输出里取出所有 chunk 的 delta 对象。
func deltas(t *testing.T, out string) []map[string]any {
	t.Helper()
	var list []map[string]any
	for _, block := range strings.Split(out, "\n\n") {
		block = strings.TrimSpace(block)
		if !strings.HasPrefix(block, "data: ") {
			continue
		}
		payload := strings.TrimPrefix(block, "data: ")
		if payload == "[DONE]" {
			continue
		}
		var chunk map[string]any
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			t.Fatalf("chunk 非 JSON: %s", payload)
		}
		choices, _ := chunk["choices"].([]any)
		if len(choices) == 0 {
			continue
		}
		c, _ := choices[0].(map[string]any)
		if d, ok := c["delta"].(map[string]any); ok && len(d) > 0 {
			list = append(list, d)
		}
	}
	return list
}

// TestConvertStreamBasic 基本流：output → content delta，done → finish + [DONE]。
func TestConvertStreamBasic(t *testing.T) {
	sse := "event: metadata\ndata: {\"session_id\":\"s1\"}\n\n" +
		"event: output\ndata: {\"response\":\"你\"}\n\n" +
		"event: output\ndata: {\"response\":\"好\"}\n\n" +
		"event: timing_cost\ndata: {\"ms\":12}\n\n" +
		"event: token_usage\ndata: {\"prompt_tokens\":3,\"completion_tokens\":5,\"total_tokens\":8}\n\n" +
		"event: done\ndata: {\"finish_reason\":\"stop\"}\n\n"

	out, res := runConvert(t, sse)
	if !strings.Contains(out, "data: [DONE]") {
		t.Fatalf("缺少 [DONE]: %s", out)
	}
	ds := deltas(t, out)
	if len(ds) != 2 {
		t.Fatalf("content delta 数 = %d, want 2: %v", len(ds), ds)
	}
	if ds[0]["content"] != "你" || ds[1]["content"] != "好" {
		t.Fatalf("content 不符: %v", ds)
	}
	if res.Content.String() != "你好" {
		t.Errorf("累积 content = %q", res.Content.String())
	}
	if res.ErrorCode != 0 {
		t.Errorf("不应有错误，得到 %d/%s", res.ErrorCode, res.ErrorMsg)
	}
	if res.Usage == nil || res.Usage["total_tokens"] != float64(8) {
		t.Errorf("usage 未采信: %v", res.Usage)
	}
	if !res.SentAny {
		t.Error("SentAny 应为 true")
	}
	// 最后一帧带 finish_reason=stop 与 usage
	if !strings.Contains(out, `"finish_reason":"stop"`) {
		t.Errorf("缺少 finish_reason: %s", out)
	}
	if !strings.Contains(out, `"total_tokens":8`) {
		t.Errorf("末帧未附 usage: %s", out)
	}
}

// TestConvertStreamReasoning reasoning_content 必须透出（思考链）。
func TestConvertStreamReasoning(t *testing.T) {
	sse := "event: output\ndata: {\"reasoning_content\":\"想一下\"}\n\n" +
		"event: output\ndata: {\"response\":\"答案\"}\n\n" +
		"event: done\ndata: {\"finish_reason\":\"stop\"}\n\n"
	out, res := runConvert(t, sse)
	ds := deltas(t, out)
	if len(ds) != 2 {
		t.Fatalf("delta 数 = %d: %v", len(ds), ds)
	}
	if ds[0]["reasoning_content"] != "想一下" {
		t.Errorf("reasoning 未透出: %v", ds[0])
	}
	if res.Reasoning.String() != "想一下" {
		t.Errorf("累积 reasoning = %q", res.Reasoning.String())
	}
}

// TestConvertStreamToolCalls tool_calls 的 function_call → function，
// 且 namespace / partial_arguments 被清理。
func TestConvertStreamToolCalls(t *testing.T) {
	sse := "event: output\ndata: {\"tool_calls\":[{\"index\":0,\"id\":\"c1\",\"function_call\":" +
		"{\"name\":\"read_file\",\"arguments\":\"{}\",\"namespace\":\"ns\",\"partial_arguments\":\"x\"}}]}\n\n" +
		"event: done\ndata: {\"finish_reason\":\"tool_calls\"}\n\n"
	out, _ := runConvert(t, sse)
	ds := deltas(t, out)
	if len(ds) != 1 {
		t.Fatalf("delta 数 = %d: %v", len(ds), ds)
	}
	tcs, ok := ds[0]["tool_calls"].([]any)
	if !ok || len(tcs) != 1 {
		t.Fatalf("tool_calls 不符: %v", ds[0])
	}
	tc, _ := tcs[0].(map[string]any)
	fn, ok := tc["function"].(map[string]any)
	if !ok {
		t.Fatalf("function_call 未改名: %v", tc)
	}
	if _, bad := fn["namespace"]; bad {
		t.Error("namespace 应被清理")
	}
	if _, bad := fn["partial_arguments"]; bad {
		t.Error("partial_arguments 应被清理")
	}
	if fn["name"] != "read_file" {
		t.Errorf("function.name = %v", fn["name"])
	}
}

// TestConvertStreamErrorBeforeData 首事件即 error 且未下发数据：
// 不下发错误帧，交给调用方决定重试（SentAny=false）。
func TestConvertStreamErrorBeforeData(t *testing.T) {
	sse := "event: error\ndata: {\"code\":1005,\"message\":\"plan limit\"}\n\n"
	out, res := runConvert(t, sse)
	if res.ErrorCode != 1005 || res.ErrorMsg != "plan limit" {
		t.Fatalf("错误信息不符: %d/%s", res.ErrorCode, res.ErrorMsg)
	}
	if res.SentAny {
		t.Error("未下发数据时 SentAny 应为 false（可重试）")
	}
	if strings.Contains(out, "data: [DONE]") {
		t.Error("未下发数据时不应补 [DONE]")
	}
	if strings.Contains(out, "\"error\"") {
		t.Error("未下发数据时错误帧不应下发（应由调用方换号重试）")
	}
}

// TestConvertStreamErrorAfterData 已有数据流出后 error：就地透传 + 补 [DONE]。
func TestConvertStreamErrorAfterData(t *testing.T) {
	sse := "event: output\ndata: {\"response\":\"部分\"}\n\n" +
		"event: error\ndata: {\"code\":500,\"message\":\"boom\"}\n\n"
	out, res := runConvert(t, sse)
	if !res.SentAny {
		t.Fatal("SentAny 应为 true")
	}
	if !strings.Contains(out, `"code":500`) {
		t.Errorf("错误帧未透传: %s", out)
	}
	if !strings.Contains(out, "data: [DONE]") {
		t.Errorf("错误后未补 [DONE]: %s", out)
	}
	if res.ErrorCode != 500 {
		t.Errorf("ErrorCode = %d", res.ErrorCode)
	}
}

// TestConvertStreamNoDone 上游中断（无 done 无 error）→ 幂等补 [DONE]。
func TestConvertStreamNoDone(t *testing.T) {
	sse := "event: output\ndata: {\"response\":\"半截\"}\n\n"
	out, res := runConvert(t, sse)
	if !strings.Contains(out, "data: [DONE]") {
		t.Fatalf("无 done 时应补 [DONE]: %s", out)
	}
	if res.ErrorCode != 0 {
		t.Errorf("不应有错误: %d", res.ErrorCode)
	}
}

// TestConvertStreamEmptyDeltaPlaceholder 空 delta 的首个 output 发 role 占位帧，
// 保证 SentAny 与真实下发一致（否则后续错误会被误判为可重试）。
func TestConvertStreamEmptyDeltaPlaceholder(t *testing.T) {
	sse := "event: output\ndata: {}\n\n" +
		"event: error\ndata: {\"code\":1,\"message\":\"x\"}\n\n"
	out, res := runConvert(t, sse)
	if !res.SentAny {
		t.Fatal("空 delta 首个 output 后 SentAny 应为 true")
	}
	if !strings.Contains(out, `"role":"assistant"`) {
		t.Errorf("缺少 role 占位帧: %s", out)
	}
	if !strings.Contains(out, `"code":1`) {
		t.Errorf("已有数据后错误应透传: %s", out)
	}
}

// TestConvertStreamKeepAliveAndGarbage 注释行（keep-alive）与非 JSON data 不应崩溃。
func TestConvertStreamKeepAliveAndGarbage(t *testing.T) {
	sse := ": keep-alive\n\n" +
		"event: output\ndata: not-json\n\n" +
		"event: output\ndata: {\"response\":\"ok\"}\n\n" +
		"event: done\ndata: {\"finish_reason\":\"stop\"}\n\n"
	out, _ := runConvert(t, sse)
	if !strings.Contains(out, `"content":"ok"`) {
		t.Fatalf("正常内容丢失: %s", out)
	}
	if !strings.Contains(out, "data: [DONE]") {
		t.Errorf("缺少 [DONE]: %s", out)
	}
}

// TestAggregate 非流式聚合：组装完整 chat.completion。
func TestAggregate(t *testing.T) {
	sse := "event: output\ndata: {\"reasoning_content\":\"想\"}\n\n" +
		"event: output\ndata: {\"response\":\"你好\"}\n\n" +
		"event: output\ndata: {\"tool_calls\":[{\"index\":0,\"id\":\"c1\",\"function_call\":{\"name\":\"f\",\"arguments\":\"{}\"}}]}\n\n" +
		"event: token_usage\ndata: {\"prompt_tokens\":3,\"completion_tokens\":5}\n\n" +
		"event: done\ndata: {\"finish_reason\":\"tool_calls\"}\n\n"

	resp, streamErr, err := Aggregate(strings.NewReader(sse), "chatcmpl-1", "glm-5.2")
	if err != nil {
		t.Fatal(err)
	}
	if streamErr != nil {
		t.Fatalf("不应有流内错误: %+v", streamErr)
	}
	if resp["object"] != "chat.completion" {
		t.Errorf("object = %v", resp["object"])
	}
	choices, _ := resp["choices"].([]any)
	if len(choices) != 1 {
		t.Fatalf("choices 数 = %d", len(choices))
	}
	c, _ := choices[0].(map[string]any)
	if c["finish_reason"] != "tool_calls" {
		t.Errorf("finish_reason = %v", c["finish_reason"])
	}
	msg, _ := c["message"].(map[string]any)
	if msg["content"] != "你好" {
		t.Errorf("content = %v", msg["content"])
	}
	if msg["reasoning_content"] != "想" {
		t.Errorf("reasoning_content = %v", msg["reasoning_content"])
	}
	tcs, _ := msg["tool_calls"].([]any)
	if len(tcs) != 1 {
		t.Fatalf("tool_calls 数 = %d", len(tcs))
	}
	tc, _ := tcs[0].(map[string]any)
	if _, bad := tc["function_call"]; bad {
		t.Error("function_call 应已改名为 function")
	}
	usage, _ := resp["usage"].(map[string]any)
	if usage["prompt_tokens"] != int64(3) {
		t.Errorf("usage.prompt_tokens = %v", usage["prompt_tokens"])
	}
	// total 未给 → 由 pt+ct 补齐
	if usage["total_tokens"] != int64(8) {
		t.Errorf("usage.total_tokens = %v, want 8（自动补齐）", usage["total_tokens"])
	}
}

// TestAggregateStreamError 流内 error → 返回错误事件、不产出响应体。
func TestAggregateStreamError(t *testing.T) {
	sse := "event: error\ndata: {\"code\":4023,\"message\":\"model unknown\"}\n\n"
	resp, streamErr, err := Aggregate(strings.NewReader(sse), "c", "m")
	if err != nil {
		t.Fatal(err)
	}
	if streamErr == nil {
		t.Fatal("应返回流内错误")
	}
	if streamErr.Code != 4023 || streamErr.Message != "model unknown" {
		t.Errorf("错误信息不符: %+v", streamErr)
	}
	if resp != nil {
		t.Errorf("有流内错误时不应产出响应体: %v", resp)
	}
}

// TestAggregateNoDone 无 done 时 finish_reason 兜底 stop。
func TestAggregateNoDone(t *testing.T) {
	sse := "event: output\ndata: {\"response\":\"半截\"}\n\n"
	resp, _, err := Aggregate(strings.NewReader(sse), "c", "m")
	if err != nil {
		t.Fatal(err)
	}
	choices, _ := resp["choices"].([]any)
	c, _ := choices[0].(map[string]any)
	if c["finish_reason"] != "stop" {
		t.Errorf("finish_reason = %v, want stop", c["finish_reason"])
	}
}
