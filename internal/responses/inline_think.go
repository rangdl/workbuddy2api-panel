package responses

import "strings"

// 内联思考标签：<think>（MiniMax M3 等）与 <thinking>（DeepSeek 系等）。
// 只识别正文**开头**的块；正文中间出现的标签按普通文本处理。
var inlineThinkTagPairs = [][2]string{
	{"\x3cthink\x3e", "\x3c/think\x3e"},
	{"\x3cthinking\x3e", "\x3c/thinking\x3e"},
}

// 标签与内容之间的分隔符：只认换行（空格/制表符可能是正文缩进，不能吞）。
const thinkSeparators = "\r\n"

type inlineThinkMode int

const (
	inlineThinkDetecting inlineThinkMode = iota
	inlineThinkReasoning
	inlineThinkText
)

type thinkPrefixDecision int

const (
	thinkNeedMore thinkPrefixDecision = iota
	thinkReasoning
	thinkPlainText
)

// leadingThinkPrefixDecision 判断缓冲区开头是否最终会构成某个 think 开标签。
func leadingThinkPrefixDecision(buffer string) thinkPrefixDecision {
	trimmed := strings.TrimLeftFunc(buffer, isInlineSpace)
	if trimmed == "" {
		return thinkNeedMore
	}
	for _, pair := range inlineThinkTagPairs {
		if strings.HasPrefix(trimmed, pair[0]) {
			return thinkReasoning
		}
	}
	for _, pair := range inlineThinkTagPairs {
		if strings.HasPrefix(pair[0], trimmed) {
			return thinkNeedMore
		}
	}
	return thinkPlainText
}

func isInlineSpace(r rune) bool {
	switch r {
	case ' ', '\t', '\r', '\n', '\v', '\f', 0x85, 0xA0:
		return true
	}
	return false
}

// stripLeadingThinkOpenTag 剥掉开头的空白与开标签，返回标签之后的内容。
func stripLeadingThinkOpenTag(text string) (string, bool) {
	trimmed := strings.TrimLeftFunc(text, isInlineSpace)
	for _, pair := range inlineThinkTagPairs {
		if rest, ok := strings.CutPrefix(trimmed, pair[0]); ok {
			return rest, true
		}
	}
	return "", false
}

// findThinkCloseTag 在文本里找最早出现的闭合标签。容忍开闭不配对
// （<think> 开、</thinking> 闭）：只认配对闭合会让整段连同正文一起被吞。
func findThinkCloseTag(text string) (int, string, bool) {
	best, bestTag := -1, ""
	for _, pair := range inlineThinkTagPairs {
		if idx := strings.Index(text, pair[1]); idx >= 0 && (best < 0 || idx < best) {
			best, bestTag = idx, pair[1]
		}
	}
	return best, bestTag, best >= 0
}

// reasoningHoldbackLen Reasoning 态下需扣住不下发的尾部长度：末尾可能是某个
// 闭合标签真前缀的最长后缀，连同它前面的换行（闭合标签到了就是分隔符，
// 没到再补发）。
func reasoningHoldbackLen(buffer string) int {
	partial := 0
	for _, pair := range inlineThinkTagPairs {
		closeTag := pair[1]
		for l := len(closeTag) - 1; l >= 1; l-- {
			if strings.HasSuffix(buffer, closeTag[:l]) {
				if l > partial {
					partial = l
				}
				break
			}
		}
	}
	body := buffer[:len(buffer)-partial]
	return len(buffer) - len(strings.TrimRight(body, thinkSeparators))
}

// inlineThinkSplitter 跨 chunk 剥离正文开头的 think 块（对齐 cc-switch
// inline_think.rs / #7741）。
//
// Detecting 扣住可能构成开标签的前缀；Reasoning 即时下发思考内容、只扣住
// 可能是闭合标签前缀的短尾；Text 透传正文。思考内容不能整段缓冲到闭合标签
// 才下发——外层静默超时量的是转换后的流，长思考期间没有输出会被判成上游卡死。
type inlineThinkSplitter struct {
	mode           inlineThinkMode
	buffer         string
	stripSeparator bool // 标签刚结束，后续增量开头的换行仍是分隔符
}

// push 喂入一个 content 增量，返回 (思考增量, 正文增量)。
func (s *inlineThinkSplitter) push(delta string) (string, string) {
	switch s.mode {
	case inlineThinkText:
		return "", s.pushText(delta)
	case inlineThinkDetecting:
		s.buffer += delta
		switch leadingThinkPrefixDecision(s.buffer) {
		case thinkNeedMore:
			return "", ""
		case thinkReasoning:
			s.mode = inlineThinkReasoning
			s.stripSeparator = true
			rest, _ := stripLeadingThinkOpenTag(s.buffer)
			s.buffer = rest
			return s.drainReasoning()
		default:
			s.mode = inlineThinkText
			text := s.buffer
			s.buffer = ""
			return "", text
		}
	default: // inlineThinkReasoning
		s.buffer += delta
		return s.drainReasoning()
	}
}

func (s *inlineThinkSplitter) pushText(delta string) string {
	if !s.stripSeparator {
		return delta
	}
	rest := strings.TrimLeft(delta, thinkSeparators)
	if rest == "" {
		return ""
	}
	s.stripSeparator = false
	return rest
}

// drainReasoning 闭合标签完整出现 → 立即拆块转 Text；否则除扣住的短尾外，
// 思考内容全部即时下发。
func (s *inlineThinkSplitter) drainReasoning() (string, string) {
	if s.stripSeparator {
		seps := len(s.buffer) - len(strings.TrimLeft(s.buffer, thinkSeparators))
		s.buffer = s.buffer[seps:]
		if s.buffer == "" {
			return "", ""
		}
		s.stripSeparator = false
	}
	if idx, closeTag, ok := findThinkCloseTag(s.buffer); ok {
		buffered := s.buffer
		s.buffer = ""
		s.mode = inlineThinkText
		s.stripSeparator = true
		thinking := strings.TrimRight(buffered[:idx], thinkSeparators)
		return thinking, s.pushText(buffered[idx+len(closeTag):])
	}
	hold := reasoningHoldbackLen(s.buffer)
	splitAt := len(s.buffer) - hold
	thinking := s.buffer[:splitAt]
	s.buffer = s.buffer[splitAt:]
	return thinking, ""
}

// flush 边界（工具调用 / finish_reason / [DONE] / 截断）冲刷残留，幂等：
// 未闭合的 think 块按思考内容原样下发，Detecting 缓冲按正文下发。
func (s *inlineThinkSplitter) flush() (string, string) {
	buffered := s.buffer
	s.buffer = ""
	wasReasoning := s.mode == inlineThinkReasoning
	s.mode = inlineThinkText
	s.stripSeparator = false
	if wasReasoning {
		return buffered, ""
	}
	return "", buffered
}
