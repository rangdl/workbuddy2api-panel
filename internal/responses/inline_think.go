package responses

import "github.com/linguo2625469/workbuddy2api-panel/internal/inlinethink"

// inlineThinkSplitter 与 Anthropic 流式路径共用同一状态机（internal/inlinethink）。
type inlineThinkSplitter = inlinethink.Splitter

// splitLeadingThinkBlock 拆分文本开头的内联思考块（共享实现）。
func splitLeadingThinkBlock(text string) (string, string, bool) {
	return inlinethink.SplitLeading(text)
}
