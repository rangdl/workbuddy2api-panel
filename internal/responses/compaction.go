package responses

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"strings"
)

// Codex 远程压缩（Responses compaction V2）桥接（对齐参考实现 cc-switch
// codex_compaction.rs）。
//
// Codex 对名为 "OpenAI" 的 provider 会发 V2 压缩请求：普通流式 /responses 请求，
// input 末尾是 {"type":"compaction_trigger"}；它要求响应里**恰好一个**
// {"type":"compaction","encrypted_content":...} 条目，否则直接 Fatal。
// 第三方模型产不出官方密文，这里把它当普通摘要模型用：触发条目换成 Codex 的压缩
// 提示词，历史里的压缩条目解回摘要正文；响应侧把摘要包进 encrypted_content 交回，
// 下次回放再由本网关解回。

const (
	// compactionEnvelopePrefix 本项目写入 encrypted_content 的信封前缀。
	// 其他前缀（官方/别家）解不开，按不可读密文降级为说明文本。
	compactionEnvelopePrefix = "wb2api-compaction-v1:"

	// compactPrompt 与 codex-rs prompts/templates/compact/prompt.md 一致。
	compactPrompt = "You are performing a CONTEXT CHECKPOINT COMPACTION. Create a handoff summary for another LLM that will resume the task.\n\n" +
		"Include:\n" +
		"- Current progress and key decisions made\n" +
		"- Important context, constraints, or user preferences\n" +
		"- What remains to be done (clear next steps)\n" +
		"- Any critical data, examples, or references needed to continue\n\n" +
		"Be concise, structured, and focused on helping the next LLM seamlessly continue the work.\n"

	// compactSummaryPrefix 与 codex-rs summary_prefix.md 一致：Codex 本地压缩把
	// 摘要写成 "{prefix}\n{summary}" 的用户消息。
	compactSummaryPrefix = "Another language model started to solve this problem and produced a summary of its thinking process. You also have access to the state of the tools that were used by that language model. Use this to build on the work that has already been done and avoid duplicating work. Here is the summary produced by the other language model, use the information in this summary to assist with your own analysis:"

	// opaqueCompactionNote 历史压缩条目是别家密文时给模型的占位说明。
	opaqueCompactionNote = "[earlier conversation was compacted; the summary is stored in a format this model cannot read]"
)

// isCompactionItem 报告 item 是否历史压缩条目。
func isCompactionItem(item map[string]any) bool {
	switch stringField(item, "type") {
	case "compaction", "compaction_summary", "context_compaction":
		return true
	}
	return false
}

// isCompactionTrigger 报告 item 是否压缩触发条目。
func isCompactionTrigger(item map[string]any) bool {
	return stringField(item, "type") == "compaction_trigger"
}

// compactionRequested 扫描 input 判断是否为压缩回合。
func compactionRequested(input any) bool {
	switch v := input.(type) {
	case []any:
		for _, raw := range v {
			if item, ok := raw.(map[string]any); ok && isCompactionTrigger(item) {
				return true
			}
		}
	case map[string]any:
		return isCompactionTrigger(v)
	}
	return false
}

// encodeCompactionSummary 把摘要封进 encrypted_content。
func encodeCompactionSummary(summary string) string {
	return compactionEnvelopePrefix + base64.RawURLEncoding.EncodeToString([]byte(summary))
}

// decodeCompactionSummary 解开本项目写入的摘要；其他密文/损坏内容返回 false。
func decodeCompactionSummary(encrypted string) (string, bool) {
	encoded, ok := strings.CutPrefix(encrypted, compactionEnvelopePrefix)
	if !ok {
		return "", false
	}
	raw, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return "", false
	}
	return string(raw), true
}

// compactionItemReplayText 历史压缩条目回放给模型时的文本。
// 无载荷的 context_compaction 标记返回 ("", false)。
func compactionItemReplayText(item map[string]any) (string, bool) {
	encrypted := strings.TrimSpace(rawString(item, "encrypted_content"))
	if encrypted == "" {
		return "", false
	}
	if summary, ok := decodeCompactionSummary(encrypted); ok {
		return compactSummaryPrefix + "\n" + summary, true
	}
	return opaqueCompactionNote, true
}

// compactionPromptItem 压缩触发条目替换成的用户消息。
func compactionPromptItem() map[string]any {
	return map[string]any{
		"role":    "user",
		"content": []any{map[string]any{"type": "text", "text": compactPrompt}},
	}
}

// compactionSummaryUserItem 历史摘要回放成的用户消息。
func compactionSummaryUserItem(text string) map[string]any {
	return map[string]any{
		"role":    "user",
		"content": []any{map[string]any{"type": "text", "text": text}},
	}
}

// compactionOutputItem 压缩回合交回 Codex 的唯一 compaction 条目。
func compactionOutputItem(summary string) map[string]any {
	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err != nil {
		// 随机失败不影响语义：用固定后缀保证 id 唯一性足够（单次响应内唯一即可）。
		copy(buf, []byte("wb2api00"))
	}
	return map[string]any{
		"type":              "compaction",
		"id":                "cmp_" + hex.EncodeToString(buf),
		"encrypted_content": encodeCompactionSummary(summary),
	}
}

// compactionIncompleteReason 压缩回合的非正常结束：返回 incomplete reason；
// 正常 stop 返回空串。
func compactionIncompleteReason(finishReason string) string {
	reason := strings.TrimSpace(finishReason)
	if reason == "" {
		return ""
	}
	switch strings.ToLower(reason) {
	case "stop", "end_turn", "stop_sequence":
		return ""
	case "length", "max_tokens", "model_context_window_exceeded":
		return "max_output_tokens"
	case "content_filter", "sensitive", "refusal":
		return "content_filter"
	default:
		return reason
	}
}

// compactionSummaryFromItems 从最终 output items 里拼接摘要正文。
func compactionSummaryFromItems(items []outputItem) string {
	raw := make([]any, len(items))
	for i, it := range items {
		raw[i] = it.item
	}
	return compactionSummaryFromAnyItems(raw)
}

// compactionSummaryFromAnyItems 拼接 message 的 output_text（去空后按空行连接）。
func compactionSummaryFromAnyItems(items []any) string {
	var parts []string
	for _, raw := range items {
		it, ok := raw.(map[string]any)
		if !ok || stringField(it, "type") != "message" {
			continue
		}
		content, _ := it["content"].([]any)
		for _, partRaw := range content {
			part, ok := partRaw.(map[string]any)
			if !ok || stringField(part, "type") != "output_text" {
				continue
			}
			if text := strings.TrimSpace(rawString(part, "text")); text != "" {
				parts = append(parts, text)
			}
		}
	}
	return strings.Join(parts, "\n\n")
}
