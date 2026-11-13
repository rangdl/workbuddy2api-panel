// ring.go 固定容量的结构化日志环形缓冲（并发安全，实现 io.Writer）。
// main 把 log 包输出与 chat 表格日志经 MultiWriter 镜像进来，面板
// /panel/api/logs 读取快照；超出容量的旧行按 FIFO 淘汰。
//
// 每行入环时按前缀规则归类频道（chat=对话请求表格行 / task=任务动作 /
// sys=系统与其它），面板日志视图按频道筛选——对话流量大时任务结果不被冲掉。
package panel

import (
	"regexp"
	"strings"
	"sync"
	"time"
)

// 日志频道。
const (
	ChChat = "chat"
	ChTask = "task"
	ChSys  = "sys"
)

// LogEntry 单条日志（时间戳取写入时刻；log 包行的行首日期时间已被剥离）。
type LogEntry struct {
	// Seq 单调递增序号（进程内唯一，淘汰后不复用）。
	// 分页游标必须用它而不是下标：环形缓冲会 FIFO 淘汰旧行，下标对应的条目
	// 在两次请求之间会整体前移，用下标翻页会重复或漏行。
	Seq  uint64    `json:"seq"`
	TS   time.Time `json:"ts"`
	Ch   string    `json:"ch"`
	Text string    `json:"text"`
}

// taskPrefixes 任务动作日志的行首标识（scheduler 与 panel 的既有口径）。
//
// 注意 "travel-timer:" 必须单列：前缀匹配是逐条 HasPrefix，"travel " 带空格，
// 匹配不到 "travel-timer: ..."（无 uid 的全局行），否则这条会落到 sys 频道。
var taskPrefixes = []string{
	"school ", "streak-bonus ", "travel ", "travel-timer ", "travel-timer:", "blindbox ", "blackcat ", "lottery ",
	"checkin ", "activity ", "keepalive ", "balance ", "user-resource ",
	"panel: 任务", "panel: 一键", "panel: checkin", "panel: 手动",
	"panel: 队列", "panel: 券码",
}

// tsPrefixRe log 包默认 flags（日期 时间）产生的行首时间戳。
var tsPrefixRe = regexp.MustCompile(`^\d{4}/\d{2}/\d{2} \d{2}:\d{2}:\d{2} `)

// classifyLine 按行首特征归类频道。
func classifyLine(line string) string {
	if strings.HasPrefix(line, "| #") { // chat 表格日志（server/logging.go logChatRow）
		return ChChat
	}
	for _, p := range taskPrefixes {
		if strings.HasPrefix(line, p) {
			return ChTask
		}
	}
	return ChSys
}

// Ring 日志环形缓冲。
type Ring struct {
	mu      sync.Mutex
	entries []LogEntry
	cap     int
	seq     uint64 // 下一条日志的序号（只增不减）
}

// NewRing 构建容量为 capacity 的日志环（非正值回退 500）。
func NewRing(capacity int) *Ring {
	if capacity <= 0 {
		capacity = 500
	}
	return &Ring{cap: capacity}
}

// Write 按 \n 切分入环（实现 io.Writer）。空行丢弃；超容量淘汰最旧行。
func (r *Ring) Write(p []byte) (int, error) {
	now := time.Now()
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, line := range strings.Split(strings.TrimRight(string(p), "\r\n"), "\n") {
		if line == "" {
			continue
		}
		text := tsPrefixRe.ReplaceAllString(line, "")
		r.seq++
		r.entries = append(r.entries, LogEntry{Seq: r.seq, TS: now, Ch: classifyLine(text), Text: text})
		if overflow := len(r.entries) - r.cap; overflow > 0 {
			r.entries = r.entries[overflow:]
		}
	}
	return len(p), nil
}

// Snapshot 按写入顺序返回缓冲内全部条目（拷贝，调用方可安全持有）。
func (r *Ring) Snapshot() []LogEntry {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]LogEntry, len(r.entries))
	copy(out, r.entries)
	return out
}

// LogQuery 日志读取条件。
//
// 分页语义是「翻历史」而不是「实时流」：前端正常轮询只取第一页（不带 cursor），
// 点「更早」才带 NextCursor 往回翻。这样缓冲区淘汰旧行时，用户正在看的历史页
// 不会因为新日志涌入而位移。Ch 为空表示不筛频道。
type LogQuery struct {
	Ch     string // 频道：task / chat / sys；空 = 全部
	Q      string // 关键词包含匹配（大小写不敏感，空格分词 AND）
	Limit  int    // 返回上限（默认 50，最大 500）
	Cursor uint64 // 游标：只返回 Seq < Cursor 的条目（即更早的行）；0 = 从最新开始
}

// LogPage 一页日志。
type LogPage struct {
	Entries    []LogEntry `json:"entries"`     // 时间升序（方便前端直接追加到末尾）
	NextCursor uint64     `json:"next_cursor"` // 下一页游标；0 = 没有更早的条目
	Total      int        `json:"total"`       // 当前筛选条件下的总条数（不含分页限制）
}

// Query 按条件读取一页日志（时间升序）。
func (r *Ring) Query(q LogQuery) LogPage {
	limit := q.Limit
	if limit <= 0 {
		limit = 50
	}
	if limit > 500 {
		limit = 500
	}
	// 关键词：空格分词，全部命中才算匹配（与请求记录筛选口径一致）。
	var kws []string
	for _, k := range strings.Fields(strings.ToLower(q.Q)) {
		kws = append(kws, k)
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	// 从后往前扫（最新的先入选），凑够 limit 后反转成时间升序。
	picked := make([]LogEntry, 0, limit)
	total := 0
	for i := len(r.entries) - 1; i >= 0; i-- {
		e := r.entries[i]
		if q.Cursor > 0 && e.Seq >= q.Cursor {
			continue
		}
		if q.Ch != "" && e.Ch != q.Ch {
			continue
		}
		if !matchKeywords(e.Text, kws) {
			continue
		}
		total++
		if len(picked) < limit {
			picked = append(picked, e)
		}
	}
	page := LogPage{Entries: picked, Total: total}
	// 反转成时间升序
	for i, j := 0, len(picked)-1; i < j; i, j = i+1, j-1 {
		picked[i], picked[j] = picked[j], picked[i]
	}
	// 还有更早的条目 → 游标取本页最旧一条的 Seq。
	if total > len(picked) && len(picked) > 0 {
		page.NextCursor = picked[0].Seq
	}
	return page
}

// matchKeywords 关键词包含匹配（kws 已转小写）。
func matchKeywords(text string, kws []string) bool {
	if len(kws) == 0 {
		return true
	}
	low := strings.ToLower(text)
	for _, k := range kws {
		if !strings.Contains(low, k) {
			return false
		}
	}
	return true
}
