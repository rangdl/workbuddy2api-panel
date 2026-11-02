package responses

import (
	"strings"
	"testing"
)

func runThinkSplitter(deltas []string) (string, string) {
	var sp inlineThinkSplitter
	var thinking, text strings.Builder
	for _, delta := range deltas {
		th, tx := sp.Push(delta)
		thinking.WriteString(th)
		text.WriteString(tx)
	}
	th, tx := sp.Flush()
	thinking.WriteString(th)
	text.WriteString(tx)
	return thinking.String(), text.String()
}

func TestInlineThinkSplitter(t *testing.T) {
	cases := []struct {
		name   string
		deltas []string
		wantTh string
		wantTx string
	}{
		{"单块 think", []string{"\x3cthink\x3ea\x3c/think\x3eb"}, "a", "b"},
		{"thinking 标签", []string{"\x3cthinking\x3ea\x3c/thinking\x3eb"}, "a", "b"},
		{"开闭不配对", []string{"\x3cthink\x3ea\x3c/thinking\x3eb"}, "a", "b"},
		{"逐字符分片", []string{"<", "t", "h", "i", "n", "k", ">", "a", "</", "think", ">", "b"}, "a", "b"},
		{"普通文本", []string{"he", "llo"}, "", "hello"},
		{"标签后的换行是分隔符", []string{"\x3cthink\x3e\nthought\n\x3c/think\x3e\nanswer"}, "thought", "answer"},
		{"闭合前扣留半截标签", []string{"\x3cthink\x3eabc</thi", "nk>x"}, "abc", "x"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			th, tx := runThinkSplitter(c.deltas)
			if th != c.wantTh || tx != c.wantTx {
				t.Errorf("thinking=%q text=%q, want %q/%q", th, tx, c.wantTh, c.wantTx)
			}
		})
	}
}

// 未闭合的 think 块在边界冲刷时按思考内容原样下发，不丢失已收到的载荷。
func TestInlineThinkSplitterFlushUnclosed(t *testing.T) {
	var sp inlineThinkSplitter
	if th, tx := sp.Push("\x3cthink\x3eabc</thi"); th != "abc" || tx != "" {
		t.Fatalf("push=%q/%q", th, tx)
	}
	th, tx := sp.Flush()
	if th != "</thi" || tx != "" {
		t.Errorf("flush=%q/%q", th, tx)
	}
	// 幂等：再次 flush 不重复输出。
	if th, tx := sp.Flush(); th != "" || tx != "" {
		t.Errorf("second flush=%q/%q", th, tx)
	}
}

// 流式转换集成：内联 think 不应作为正文透给 Codex。
func TestStreamStripsInlineThink(t *testing.T) {
	s := NewStreamState(nil)
	var buf strings.Builder
	for _, d := range []string{"\x3cthink\x3e", "long thought", "\x3c/think\x3e", "answer text"} {
		buf.Write(s.HandleChunk(chunk(map[string]any{"content": d}, "", nil)))
	}
	buf.Write(s.HandleChunk(chunk(map[string]any{}, "stop", nil)))
	buf.Write(s.Finalize())
	out := buf.String()

	if strings.Contains(out, "\x3cthink\x3e") || strings.Contains(out, "long thought\x3c") {
		t.Errorf("内联 think 泄漏到事件流: %s", out)
	}
	if !strings.Contains(out, `"delta":"long thought"`) {
		t.Errorf("思考内容未作为 reasoning 事件下发: %s", out)
	}
	if !strings.Contains(out, `"delta":"answer text"`) {
		t.Errorf("正文未下发: %s", out)
	}
}
