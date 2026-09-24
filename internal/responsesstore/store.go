// Package responsesstore 为 Responses 增量会话（previous_response_id +
// function_call_output）提供跨请求的 function_call 上下文存储。
//
// Codex 新版固定走增量：后续请求只带 previous_response_id 与 function_call_output，
// 不带全量 input。而 DeepSeek 系上游要求 assistant 的 tool_call 与 reasoning_content
// 紧邻 tool 结果，否则 400（典型症状：答一句就停）。本包缓存每次响应产出的 call item，
// 在请求转换为 Chat 之前补全缺失的 function_call。
//
// 蓝本：cc-switch 的 codex_chat_history.rs（三级索引 + LRU）。本项目将其接口化：
// 默认进程内 LRU（单实例够用，对齐 cc-switch），可选 Redis 后端（多实例，后续扩展）。
package responsesstore

import (
	"encoding/json"
	"sync"
)

// DefaultMaxResponses 默认缓存响应条数上限（对齐 cc-switch 的 512）。
const DefaultMaxResponses = 512

// callItemTypes 需要缓存的 call item 类型。
var callItemTypes = map[string]bool{
	"function_call":    true,
	"custom_tool_call": true,
	"tool_search_call": true,
}

// callOutputItemTypes 引用 call 的输出 item 类型。
var callOutputItemTypes = map[string]bool{
	"function_call_output":    true,
	"custom_tool_call_output": true,
	"tool_search_output":      true,
}

// enrichFields 补全时只填充的空字段（不覆盖已有值）。
var enrichFields = []string{
	"name", "namespace", "arguments", "input", "status", "execution",
	"reasoning_content", "reasoning",
}

// Store 是增量会话存储接口。
type Store interface {
	// Record 记录一次响应产出的 call items（response_id → calls）。
	Record(responseID string, output []any)
	// Fill 用缓存的 call items 补全请求 input，返回补全后的请求体与补全数量。
	// 无 previous_response_id 或无可补全内容时原样返回 body。
	Fill(body []byte) ([]byte, int)
}

// callItem 一个待缓存/补全的 call item。
type callItem struct {
	callID string
	item   map[string]any
}

// cachedResponse 某个 response 产出的全部 call items（保序）。
type cachedResponse struct {
	callsByID map[string]map[string]any
	callOrder []string
}

// memoryStore 进程内 LRU 实现（三级索引：responses / responseOrder / callIndex）。
type memoryStore struct {
	mu            sync.RWMutex
	responses     map[string]*cachedResponse
	responseOrder []string
	callIndex     map[string][]string
	maxResponses  int
}

// NewMemoryStore 构造进程内存储；maxResponses <= 0 时用默认 512。
func NewMemoryStore(maxResponses int) Store {
	if maxResponses <= 0 {
		maxResponses = DefaultMaxResponses
	}
	return &memoryStore{
		responses:    map[string]*cachedResponse{},
		callIndex:    map[string][]string{},
		maxResponses: maxResponses,
	}
}

// Record 实现 Store。
func (s *memoryStore) Record(responseID string, output []any) {
	if responseID == "" || len(output) == 0 {
		return
	}
	calls := make([]callItem, 0, len(output))
	for _, raw := range output {
		item, ok := raw.(map[string]any)
		if !ok || !isCallItemType(itemType(item)) {
			continue
		}
		callID := itemCallID(item)
		if callID == "" {
			continue
		}
		calls = append(calls, callItem{callID: callID, item: item})
	}
	if len(calls) == 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.insertCallsLocked(responseID, calls)
}

// Fill 实现 Store。
func (s *memoryStore) Fill(body []byte) ([]byte, int) {
	var req map[string]any
	if err := json.Unmarshal(body, &req); err != nil {
		return body, 0
	}
	input, ok := req["input"]
	if !ok {
		return body, 0
	}
	previousID, _ := req["previous_response_id"].(string)

	items, wasObject, ok := normalizeInput(input)
	if !ok {
		return body, 0
	}

	outputCallIDs, existingCallIDs := scanCallIDs(items)
	requested := unionCallIDs(outputCallIDs, existingCallIDs)

	s.mu.RLock()
	previous := s.responses[previousID]
	fallback := s.uniqueFallbackLocked(requested, previous)
	s.mu.RUnlock()

	restoreGroup := buildRestoreGroup(previous, fallback, outputCallIDs, existingCallIDs)
	newItems, changed := mergeCalls(items, restoreGroup, previous, fallback)
	if changed == 0 {
		return body, 0
	}

	if wasObject && len(newItems) == 1 {
		req["input"] = newItems[0]
	} else {
		req["input"] = newItems
	}
	out, err := json.Marshal(req)
	if err != nil {
		return body, 0
	}
	return out, changed
}

// ---- 内部实现 ----

func (s *memoryStore) insertCallsLocked(responseID string, calls []callItem) {
	resp, ok := s.responses[responseID]
	if !ok {
		resp = &cachedResponse{callsByID: map[string]map[string]any{}}
		s.responses[responseID] = resp
		s.responseOrder = append(s.responseOrder, responseID)
	}
	for _, call := range calls {
		if _, exists := resp.callsByID[call.callID]; !exists {
			resp.callOrder = append(resp.callOrder, call.callID)
		}
		resp.callsByID[call.callID] = call.item
		s.indexCallLocked(call.callID, responseID)
	}
	s.pruneLocked()
}

func (s *memoryStore) indexCallLocked(callID, responseID string) {
	ids := s.callIndex[callID]
	for _, id := range ids {
		if id == responseID {
			return
		}
	}
	s.callIndex[callID] = append(ids, responseID)
}

func (s *memoryStore) pruneLocked() {
	for len(s.responseOrder) > s.maxResponses {
		oldest := s.responseOrder[0]
		s.responseOrder = s.responseOrder[1:]
		delete(s.responses, oldest)
		s.removeFromCallIndexLocked(oldest)
	}
}

func (s *memoryStore) removeFromCallIndexLocked(responseID string) {
	for callID, ids := range s.callIndex {
		kept := ids[:0]
		for _, id := range ids {
			if id != responseID {
				kept = append(kept, id)
			}
		}
		if len(kept) == 0 {
			delete(s.callIndex, callID)
		} else {
			s.callIndex[callID] = kept
		}
	}
}

// uniqueFallbackLocked 对不在 previous 里的 call_id 做全局唯一反查（有歧义则丢弃）。
func (s *memoryStore) uniqueFallbackLocked(requested map[string]bool, previous *cachedResponse) *cachedResponse {
	fallback := &cachedResponse{callsByID: map[string]map[string]any{}}
	for callID := range requested {
		if previous != nil {
			if _, ok := previous.callsByID[callID]; ok {
				continue
			}
		}
		if item, ok := s.uniqueCallLocked(callID); ok {
			fallback.callsByID[callID] = item
		}
	}
	// 按 responseOrder 重建顺序，保持与 cc-switch 一致的 call_order 语义。
	for _, responseID := range s.responseOrder {
		resp := s.responses[responseID]
		if resp == nil {
			continue
		}
		for _, callID := range resp.callOrder {
			if _, ok := fallback.callsByID[callID]; ok {
				fallback.callOrder = append(fallback.callOrder, callID)
			}
		}
	}
	return fallback
}

// uniqueCallLocked 返回全局唯一的 call item；出现在多个 response 时返回 false。
func (s *memoryStore) uniqueCallLocked(callID string) (map[string]any, bool) {
	var found map[string]any
	for _, responseID := range s.callIndex[callID] {
		resp := s.responses[responseID]
		if resp == nil {
			continue
		}
		item, ok := resp.callsByID[callID]
		if !ok {
			continue
		}
		if found != nil {
			return nil, false
		}
		found = item
	}
	return found, found != nil
}

// ---- 纯函数 ----

func normalizeInput(input any) (items []any, wasObject bool, ok bool) {
	switch v := input.(type) {
	case []any:
		return v, false, true
	case map[string]any:
		return []any{v}, true, true
	default:
		return nil, false, false
	}
}

func scanCallIDs(items []any) (outputCallIDs, existingCallIDs map[string]bool) {
	outputCallIDs = map[string]bool{}
	existingCallIDs = map[string]bool{}
	for _, raw := range items {
		item, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		t := itemType(item)
		if isCallOutputItemType(t) {
			if id := itemCallID(item); id != "" {
				outputCallIDs[id] = true
			}
		} else if isCallItemType(t) {
			if id := itemCallID(item); id != "" {
				existingCallIDs[id] = true
			}
		}
	}
	return outputCallIDs, existingCallIDs
}

func unionCallIDs(a, b map[string]bool) map[string]bool {
	out := make(map[string]bool, len(a)+len(b))
	for k := range a {
		out[k] = true
	}
	for k := range b {
		out[k] = true
	}
	return out
}

// buildRestoreGroup 从 previous + fallback 挑出「被 output 引用、但 input 里没有、
// 且未分组」的 calls，按 call_order 排序。
func buildRestoreGroup(previous, fallback *cachedResponse, outputCallIDs, existingCallIDs map[string]bool) []callItem {
	group := []callItem{}
	grouped := map[string]bool{}
	appendGroup := func(resp *cachedResponse) {
		if resp == nil {
			return
		}
		for _, callID := range resp.callOrder {
			if !outputCallIDs[callID] || existingCallIDs[callID] || grouped[callID] {
				continue
			}
			if item, ok := resp.callsByID[callID]; ok {
				grouped[callID] = true
				group = append(group, callItem{callID: callID, item: item})
			}
		}
	}
	appendGroup(previous)
	appendGroup(fallback)
	return group
}

// mergeCalls 遍历 input，插入缺失的 call items 并补全已有 call 的空字段。
func mergeCalls(items []any, restoreGroup []callItem, previous, fallback *cachedResponse) ([]any, int) {
	newItems := make([]any, 0, len(items)+len(restoreGroup))
	seen := map[string]bool{}
	restoreIDs := map[string]bool{}
	for _, c := range restoreGroup {
		restoreIDs[c.callID] = true
	}
	groupInserted := false
	changed := 0

	for _, raw := range items {
		item, ok := raw.(map[string]any)
		if !ok {
			newItems = append(newItems, raw)
			continue
		}
		t := itemType(item)
		switch {
		case isCallItemType(t):
			callID := itemCallID(item)
			if cached, ok := lookupCall(previous, fallback, callID); ok {
				if enrichCallItem(item, cached) {
					changed++
				}
			}
			seen[callID] = true
			newItems = append(newItems, item)
		case isCallOutputItemType(t):
			if !groupInserted && len(restoreGroup) > 0 {
				for _, c := range restoreGroup {
					if !seen[c.callID] {
						seen[c.callID] = true
						newItems = append(newItems, c.item)
						changed++
					}
				}
				groupInserted = true
			}
			callID := itemCallID(item)
			if callID != "" && !seen[callID] && !restoreIDs[callID] {
				if cached, ok := lookupCall(previous, fallback, callID); ok {
					seen[callID] = true
					newItems = append(newItems, cached)
					changed++
				}
			}
			newItems = append(newItems, item)
		default:
			newItems = append(newItems, item)
		}
	}
	return newItems, changed
}

func lookupCall(previous, fallback *cachedResponse, callID string) (map[string]any, bool) {
	if callID == "" {
		return nil, false
	}
	if previous != nil {
		if item, ok := previous.callsByID[callID]; ok {
			return item, true
		}
	}
	if fallback != nil {
		if item, ok := fallback.callsByID[callID]; ok {
			return item, true
		}
	}
	return nil, false
}

// enrichCallItem 只补缺失字段，不覆盖已有值。
func enrichCallItem(item, cached map[string]any) bool {
	changed := false
	for _, key := range enrichFields {
		if !isEmptyValue(item[key]) {
			continue
		}
		v, ok := cached[key]
		if !ok || isEmptyValue(v) {
			continue
		}
		item[key] = v
		changed = true
	}
	return changed
}

func itemType(item map[string]any) string {
	s, _ := item["type"].(string)
	return s
}

func itemCallID(item map[string]any) string {
	if id, ok := item["call_id"].(string); ok && id != "" {
		return id
	}
	id, _ := item["id"].(string)
	return id
}

func isCallItemType(t string) bool { return callItemTypes[t] }

func isCallOutputItemType(t string) bool { return callOutputItemTypes[t] }

func isEmptyValue(v any) bool {
	switch value := v.(type) {
	case nil:
		return true
	case string:
		return len(trimSpace(value)) == 0
	case []any:
		return len(value) == 0
	case map[string]any:
		return len(value) == 0
	default:
		return false
	}
}

func trimSpace(s string) string {
	start, end := 0, len(s)
	for start < end && isSpaceByte(s[start]) {
		start++
	}
	for end > start && isSpaceByte(s[end-1]) {
		end--
	}
	return s[start:end]
}

func isSpaceByte(b byte) bool {
	return b == ' ' || b == '\t' || b == '\n' || b == '\r'
}
