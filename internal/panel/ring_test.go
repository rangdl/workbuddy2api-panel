package panel

import (
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestClassifyLine(t *testing.T) {
	cases := map[string]string{
		"| #001 | glm-5.2 | stream | 200 | uid=c8a3e793 | TTFB=120ms |": ChChat,
		"school c8a3e793: ★ 分享任务完成":                                     ChTask,
		"streak-bonus 5c162cc9: 🎊 新手礼包 +100c":                           ChTask,
		"blackcat c8a3e793: 完成 3 次夜间对话":                                 ChTask,
		"checkin 5c162cc9: 已签到":                                         ChTask,
		"panel: 任务动作 uid=x code=chat_5":                                 ChTask,
		"panel: 队列启动：6 项（并发 2）":                                         ChTask,
		"panel: revive uid=x":                                           ChSys,
		"workbuddy2api listening on :7863":                              ChSys,
		"scheduler: 余额后台刷新每 5m0s":                                       ChSys,
	}
	for line, want := range cases {
		if got := classifyLine(line); got != want {
			t.Errorf("classifyLine(%q)=%q want %q", line, got, want)
		}
	}
}

func TestRingWriteStripsTimestamp(t *testing.T) {
	r := NewRing(4)
	if _, err := r.Write([]byte("2026/09/14 00:12:34 school x: done\n")); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Write([]byte("| #002 | glm | stream | 200 | ok |")); err != nil {
		t.Fatal(err)
	}
	es := r.Snapshot()
	if len(es) != 2 {
		t.Fatalf("entries=%d want 2", len(es))
	}
	if strings.HasPrefix(es[0].Text, "2026/") {
		t.Errorf("timestamp not stripped: %q", es[0].Text)
	}
	if es[0].Ch != ChTask || es[1].Ch != ChChat {
		t.Errorf("channels: %q %q", es[0].Ch, es[1].Ch)
	}
	if time.Since(es[0].TS) > 5*time.Second {
		t.Errorf("stale ts: %v", es[0].TS)
	}
}

// TestRingQueryPagination 分页游标必须基于 Seq 而不是下标：
// 环形缓冲淘汰旧行后下标会整体前移，用下标翻页会重复或漏行。
func TestRingQueryPagination(t *testing.T) {
	r := NewRing(10)
	for i := 1; i <= 6; i++ {
		r.Write([]byte("panel: 队列 " + strconv.Itoa(i) + " 项\n"))
	}
	// 第一页：最新 2 条（升序）→ 5, 6
	p1 := r.Query(LogQuery{Limit: 2})
	if len(p1.Entries) != 2 || p1.Total != 6 {
		t.Fatalf("p1 len=%d total=%d want 2/6", len(p1.Entries), p1.Total)
	}
	if !strings.HasSuffix(p1.Entries[0].Text, "5 项") || !strings.HasSuffix(p1.Entries[1].Text, "6 项") {
		t.Errorf("p1 = %q / %q", p1.Entries[0].Text, p1.Entries[1].Text)
	}
	if p1.NextCursor == 0 {
		t.Fatal("NextCursor = 0, 还有更早的条目")
	}
	// 第二页：应拿到 3, 4（不是 4, 5）
	p2 := r.Query(LogQuery{Limit: 2, Cursor: p1.NextCursor})
	if len(p2.Entries) != 2 || !strings.HasSuffix(p2.Entries[0].Text, "3 项") || !strings.HasSuffix(p2.Entries[1].Text, "4 项") {
		t.Errorf("p2 = %q / %q", p2.Entries[0].Text, p2.Entries[1].Text)
	}
	// 写入新日志导致淘汰：游标仍然有效，不重复已读条目
	for i := 7; i <= 12; i++ {
		r.Write([]byte("panel: 队列 " + strconv.Itoa(i) + " 项\n"))
	}
	p3 := r.Query(LogQuery{Limit: 5, Cursor: p2.NextCursor})
	for _, e := range p3.Entries {
		if strings.HasSuffix(e.Text, "3 项") || strings.HasSuffix(e.Text, "4 项") {
			t.Errorf("重复返回已读条目: %q", e.Text)
		}
		if e.Seq >= p2.NextCursor {
			t.Errorf("返回了不早于游标的条目: seq=%d cursor=%d", e.Seq, p2.NextCursor)
		}
	}
}

// TestRingQueryFilter 频道与关键词筛选，以及 total 只统计命中项。
func TestRingQueryFilter(t *testing.T) {
	r := NewRing(50)
	r.Write([]byte("school x: 分享任务完成\n"))
	r.Write([]byte("| #001 | glm-5.2 | stream | 200 | ok |\n"))
	r.Write([]byte("panel: revive uid=y\n"))
	r.Write([]byte("checkin x: 已签到\n"))

	if p := r.Query(LogQuery{Ch: ChTask}); len(p.Entries) != 2 || p.Total != 2 {
		t.Errorf("ch=task got %d/%d want 2/2", len(p.Entries), p.Total)
	}
	// 关键词大小写不敏感，空格分词 AND
	if p := r.Query(LogQuery{Q: "GLM"}); len(p.Entries) != 1 || !strings.Contains(p.Entries[0].Text, "glm") {
		t.Errorf("q=GLM got %d entries", len(p.Entries))
	}
	if p := r.Query(LogQuery{Q: "panel revive"}); len(p.Entries) != 1 {
		t.Errorf("q='panel revive' got %d want 1", len(p.Entries))
	}
	if p := r.Query(LogQuery{Q: "panel school"}); len(p.Entries) != 0 {
		t.Errorf("q='panel school' got %d want 0 (AND 语义)", len(p.Entries))
	}
}

// TestRingQueryLimit 默认与上限：limit<=0 取 50，超过 500 截断到 500。
func TestRingQueryLimit(t *testing.T) {
	r := NewRing(1000)
	for i := 0; i < 600; i++ {
		r.Write([]byte("scheduler: 余额后台刷新\n"))
	}
	if p := r.Query(LogQuery{}); len(p.Entries) != 50 {
		t.Errorf("默认 limit got %d want 50", len(p.Entries))
	}
	if p := r.Query(LogQuery{Limit: 9999}); len(p.Entries) != 500 {
		t.Errorf("limit 上限 got %d want 500", len(p.Entries))
	}
	if p := r.Query(LogQuery{Limit: 600}); len(p.Entries) != 500 || p.NextCursor == 0 {
		t.Errorf("limit 600 应截断到 500 且仍可翻页: got %d 条, cursor=%d", len(p.Entries), p.NextCursor)
	}
	// 缓冲区里正好 500 条时一次取完 → 没有更早的条目
	r2 := NewRing(1000)
	for i := 0; i < 500; i++ {
		r2.Write([]byte("scheduler: 余额后台刷新\n"))
	}
	if p := r2.Query(LogQuery{Limit: 500}); len(p.Entries) != 500 || p.NextCursor != 0 {
		t.Errorf("一次取完时 NextCursor 应为 0: got %d 条, cursor=%d", len(p.Entries), p.NextCursor)
	}
}
