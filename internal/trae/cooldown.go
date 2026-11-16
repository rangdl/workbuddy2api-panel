package trae

import (
	"sort"
	"sync"
	"time"
)

// cooldown.go 账号级冷却表。
//
// 设计取舍：进程内内存态、重启清零——与 CodeBuddy 侧的池状态（落盘 + Redis 镜像）
// 刻意不同。理由：Trae 账号数量级小（个位数），冷却时长以小时计，重启丢失的代价
// 仅是"多试一次坏账号"；而落盘会引入与 CodeBuddy 池状态文件的耦合。
// 若后续需要跨重启保持，再补持久化即可（接口不变）。

// Cooldowns 账号冷却状态表（并发安全）。
type Cooldowns struct {
	mu      sync.Mutex
	entries map[string]coolEntry
}

type coolEntry struct {
	until  time.Time
	kind   ErrKind
	reason string
}

// CoolInfo 冷却状态的只读快照（面板展示用）。
type CoolInfo struct {
	UID       string        `json:"uid"`
	Kind      string        `json:"kind"`
	Reason    string        `json:"reason"`
	Until     time.Time     `json:"until"`
	Remaining time.Duration `json:"-"`
	// RemainingSec 剩余秒数（面板直接展示，避免前端处理 duration）。
	RemainingSec int64 `json:"remaining_sec"`
}

// NewCooldowns 构造空冷却表。
func NewCooldowns() *Cooldowns {
	return &Cooldowns{entries: make(map[string]coolEntry)}
}

// Mark 标记账号冷却（d<=0 表示不冷却，空操作）。
func (c *Cooldowns) Mark(uid string, d time.Duration, kind ErrKind, reason string) {
	if c == nil || uid == "" || d <= 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries[uid] = coolEntry{until: time.Now().Add(d), kind: kind, reason: reason}
}

// Clear 清除账号冷却（登录成功 / 手动解冻 / 上游恢复时调用）。
func (c *Cooldowns) Clear(uid string) {
	if c == nil || uid == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.entries, uid)
}

// Available 账号当前是否可用（未冷却）。
func (c *Cooldowns) Available(uid string) bool {
	if c == nil || uid == "" {
		return true
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[uid]
	if !ok {
		return true
	}
	if time.Now().After(e.until) {
		delete(c.entries, uid) // 惰性清理：过期即移除
		return true
	}
	return false
}

// Snapshot 返回当前全部冷却条目（按剩余时间降序），供面板展示。
func (c *Cooldowns) Snapshot() []CoolInfo {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	out := make([]CoolInfo, 0, len(c.entries))
	for uid, e := range c.entries {
		if now.After(e.until) {
			delete(c.entries, uid)
			continue
		}
		rem := e.until.Sub(now)
		out = append(out, CoolInfo{
			UID:          uid,
			Kind:         string(e.kind),
			Reason:       e.reason,
			Until:        e.until,
			Remaining:    rem,
			RemainingSec: int64(rem.Seconds()),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Remaining > out[j].Remaining })
	return out
}

// Count 当前处于冷却中的账号数。
func (c *Cooldowns) Count() int { return len(c.Snapshot()) }
