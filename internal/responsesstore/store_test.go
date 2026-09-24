package responsesstore

import (
	"encoding/json"
	"testing"
)

func recordCall(s Store, responseID, callID, name, args, reasoning string) {
	s.Record(responseID, []any{map[string]any{
		"type":              "function_call",
		"call_id":           callID,
		"name":              name,
		"arguments":         args,
		"reasoning_content": reasoning,
	}})
}

func fillInput(t *testing.T, raw []byte) []any {
	t.Helper()
	var req map[string]any
	if err := json.Unmarshal(raw, &req); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	items, _ := req["input"].([]any)
	return items
}

func TestFillFromPreviousResponse(t *testing.T) {
	s := NewMemoryStore(0)
	recordCall(s, "resp_1", "call_1", "read_file", `{"path":"README.md"}`, "Need to inspect.")

	body := []byte(`{"previous_response_id":"resp_1","input":[{"type":"function_call_output","call_id":"call_1","output":"ok"}]}`)
	out, n := s.Fill(body)
	if n != 1 {
		t.Fatalf("restored=%d", n)
	}
	items := fillInput(t, out)
	if len(items) != 2 {
		t.Fatalf("items len=%d (%v)", len(items), items)
	}
	first, _ := items[0].(map[string]any)
	if first["type"] != "function_call" || first["name"] != "read_file" {
		t.Errorf("restored call=%v", first)
	}
	if first["reasoning_content"] != "Need to inspect." {
		t.Errorf("reasoning_content=%v", first["reasoning_content"])
	}
	second, _ := items[1].(map[string]any)
	if second["type"] != "function_call_output" {
		t.Errorf("output item=%v", second)
	}
}

func TestFillUniqueFallbackWithoutPrevious(t *testing.T) {
	s := NewMemoryStore(0)
	recordCall(s, "resp_1", "call_1", "read_file", "{}", "only cached call")

	body := []byte(`{"input":[{"type":"function_call_output","call_id":"call_1","output":"ok"}]}`)
	out, n := s.Fill(body)
	if n != 1 {
		t.Fatalf("restored=%d", n)
	}
	items := fillInput(t, out)
	if items[0].(map[string]any)["reasoning_content"] != "only cached call" {
		t.Errorf("fallback restore failed: %v", items)
	}
}

func TestFillAmbiguousCallIDNotRestored(t *testing.T) {
	s := NewMemoryStore(0)
	recordCall(s, "resp_1", "call_1", "read_file", "{}", "first")
	recordCall(s, "resp_2", "call_1", "read_file", "{}", "second")

	body := []byte(`{"input":[{"type":"function_call_output","call_id":"call_1","output":"ok"}]}`)
	_, n := s.Fill(body)
	if n != 0 {
		t.Fatalf("ambiguous call should not restore, got n=%d", n)
	}
}

func TestFillEnrichesExistingCall(t *testing.T) {
	s := NewMemoryStore(0)
	recordCall(s, "resp_1", "call_1", "read_file", "{}", "Need to inspect.")

	body := []byte(`{"previous_response_id":"resp_1","input":[{"type":"function_call","call_id":"call_1","name":"read_file","arguments":"{}"}]}`)
	out, n := s.Fill(body)
	if n != 1 {
		t.Fatalf("enriched=%d", n)
	}
	items := fillInput(t, out)
	if items[0].(map[string]any)["reasoning_content"] != "Need to inspect." {
		t.Errorf("enrich failed: %v", items[0])
	}
}

func TestFillNoChangeReturnsOriginal(t *testing.T) {
	s := NewMemoryStore(0)
	body := []byte(`{"input":[{"type":"function_call_output","call_id":"missing","output":"ok"}]}`)
	out, n := s.Fill(body)
	if n != 0 {
		t.Fatalf("n=%d", n)
	}
	if string(out) != string(body) {
		t.Errorf("body changed: %s", out)
	}
}

func TestLRUEviction(t *testing.T) {
	s := NewMemoryStore(2)
	recordCall(s, "resp_1", "call_a", "a", "{}", "")
	recordCall(s, "resp_2", "call_b", "b", "{}", "")
	recordCall(s, "resp_3", "call_c", "c", "{}", "") // resp_1 被淘汰

	body := []byte(`{"previous_response_id":"resp_1","input":[{"type":"function_call_output","call_id":"call_a","output":"x"}]}`)
	_, n := s.Fill(body)
	if n != 0 {
		t.Fatalf("evicted response should not restore, n=%d", n)
	}
}

func TestFillMultipleCallsGroupOrder(t *testing.T) {
	s := NewMemoryStore(0)
	s.Record("resp_1", []any{
		map[string]any{"type": "function_call", "call_id": "c1", "name": "a", "arguments": "{}"},
		map[string]any{"type": "function_call", "call_id": "c2", "name": "b", "arguments": "{}"},
	})
	body := []byte(`{"previous_response_id":"resp_1","input":[
		{"type":"function_call_output","call_id":"c1","output":"1"},
		{"type":"function_call_output","call_id":"c2","output":"2"}
	]}`)
	out, n := s.Fill(body)
	if n != 2 {
		t.Fatalf("restored=%d", n)
	}
	items := fillInput(t, out)
	// 期望：c1, c2 两个 call 插到第一个 output 之前，随后是两个 output。
	if len(items) != 4 {
		t.Fatalf("items len=%d (%v)", len(items), items)
	}
	if items[0].(map[string]any)["call_id"] != "c1" || items[1].(map[string]any)["call_id"] != "c2" {
		t.Errorf("group order wrong: %v", items)
	}
	if items[2].(map[string]any)["type"] != "function_call_output" {
		t.Errorf("expected output at index 2: %v", items[2])
	}
}
