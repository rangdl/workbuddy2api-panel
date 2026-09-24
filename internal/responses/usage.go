package responses

// chatUsageToResponsesUsage 把 Chat usage 映射为 Responses usage。
// 字段优先级对齐 cc-switch 的 chat_usage_to_responses_usage。
func chatUsageToResponsesUsage(usage any) map[string]any {
	obj, _ := usage.(map[string]any)
	if obj == nil {
		return emptyResponsesUsage()
	}

	inputTokens := firstNum(obj, "prompt_tokens", "input_tokens")
	outputTokens := firstNum(obj, "completion_tokens", "output_tokens")
	totalTokens := numToInt(obj["total_tokens"])
	if totalTokens == 0 {
		totalTokens = inputTokens + outputTokens
	}

	result := map[string]any{
		"input_tokens":          inputTokens,
		"output_tokens":         outputTokens,
		"total_tokens":          totalTokens,
		"input_tokens_details":  map[string]any{"cached_tokens": cachedTokens(obj)},
		"output_tokens_details": outputTokensDetails(obj),
	}
	if v, ok := obj["cache_read_input_tokens"]; ok {
		result["cache_read_input_tokens"] = numToInt(v)
	}
	if n, ok := nestedNum(obj, "prompt_tokens_details", "cache_write_tokens"); ok && n > 0 {
		result["cache_creation_input_tokens"] = n
	}
	return result
}

// emptyResponsesUsage 返回全零 usage。
func emptyResponsesUsage() map[string]any {
	return map[string]any{
		"input_tokens":          int64(0),
		"output_tokens":         int64(0),
		"total_tokens":          int64(0),
		"input_tokens_details":  map[string]any{"cached_tokens": int64(0)},
		"output_tokens_details": map[string]any{"reasoning_tokens": int64(0)},
	}
}

// firstNum 返回第一个存在且非零的数字字段。
func firstNum(obj map[string]any, keys ...string) int64 {
	for _, k := range keys {
		if v, ok := obj[k]; ok {
			if n := numToInt(v); n != 0 {
				return n
			}
		}
	}
	return 0
}

// nestedNum 取 obj[outer][inner] 的数字值。
func nestedNum(obj map[string]any, outer, inner string) (int64, bool) {
	m, ok := obj[outer].(map[string]any)
	if !ok {
		return 0, false
	}
	v, ok := m[inner]
	if !ok {
		return 0, false
	}
	return numToInt(v), true
}

// cachedTokens 提取缓存命中 token 数（多来源兜底）。
func cachedTokens(obj map[string]any) int64 {
	if v, ok := obj["cache_read_input_tokens"]; ok {
		return numToInt(v)
	}
	if n, ok := nestedNum(obj, "prompt_tokens_details", "cached_tokens"); ok {
		return n
	}
	if n, ok := nestedNum(obj, "input_tokens_details", "cached_tokens"); ok {
		return n
	}
	if v, ok := obj["prompt_cache_hit_tokens"]; ok {
		return numToInt(v)
	}
	return 0
}

// outputTokensDetails 复制 completion_tokens_details 并补 reasoning_tokens=0。
func outputTokensDetails(obj map[string]any) map[string]any {
	if details, ok := obj["completion_tokens_details"].(map[string]any); ok {
		out := make(map[string]any, len(details)+1)
		for k, v := range details {
			out[k] = v
		}
		if _, ok := out["reasoning_tokens"]; !ok {
			out["reasoning_tokens"] = int64(0)
		}
		return out
	}
	return map[string]any{"reasoning_tokens": int64(0)}
}
