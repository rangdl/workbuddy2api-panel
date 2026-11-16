// idle.go 流式响应体的空闲监控（Trae）。
//
// 为什么需要：SSE 长连接上，上游可能已经卡死却不断开 TCP。没有空闲监控时，
// 客户端会一直等到外层 ctx 超时（默认可能几分钟），期间占着在途名额、连接与
// goroutine，而且表现为「请求莫名挂住」——排查方向完全对不上。
//
// 与 CodeBuddy 的关系：internal/upstream 里有一份等价实现（monitorBody），
// 但它未导出且绑定在那个包的 Client 类型上。两个上游各自维护一份的成本很低
// （逻辑不到 50 行），而把它们抽到公共包会引入「谁的 cancel 语义」的耦合。
// 更重要的是：轮转循环对两者的期望一致——**卡住的流必须能被自己掐断**。
package trae

import (
	"context"
	"io"
	"sync"
	"time"
)

// idleMonitoringBody 包装响应体，读超时未动则取消请求。
type idleMonitoringBody struct {
	rc     io.ReadCloser
	cancel context.CancelFunc
	idle   time.Duration

	mu       sync.Mutex
	lastRead time.Time
	closed   bool
	stopCh   chan struct{}
	stopOnce sync.Once
}

// newIdleBody 包装响应体：idle <= 0 时原样返回（不监控）。
//
// cancel 由调用方传入（ChatStream 派生的 ctx cancel），流卡住时用它断流；
// 正常读完或 Close 时也调用，避免 ctx 泄漏（Go 的 vet/lostcancel 要求）。
func newIdleBody(rc io.ReadCloser, idle time.Duration, cancel context.CancelFunc) io.ReadCloser {
	if idle <= 0 {
		// 不监控：仍需在 Close 时释放 ctx，否则 cancel 永远不被调用。
		return &noIdleBody{rc: rc, cancel: cancel}
	}
	b := &idleMonitoringBody{
		rc:       rc,
		cancel:   cancel,
		idle:     idle,
		lastRead: time.Now(),
		stopCh:   make(chan struct{}),
	}
	go b.watch()
	return b
}

// watch 周期检查「距上次读到数据」是否超过 idle。
func (b *idleMonitoringBody) watch() {
	t := time.NewTicker(idleTickInterval(b.idle))
	defer t.Stop()
	for {
		select {
		case <-b.stopCh:
			return
		case <-t.C:
			b.mu.Lock()
			idleFor := time.Since(b.lastRead)
			closed := b.closed
			b.mu.Unlock()
			if closed {
				return
			}
			if idleFor > b.idle {
				// 主动断流：上游卡住时不让调用方无限等待。
				b.cancel()
				return
			}
		}
	}
}

func (b *idleMonitoringBody) Read(p []byte) (int, error) {
	n, err := b.rc.Read(p)
	if n > 0 {
		b.mu.Lock()
		b.lastRead = time.Now()
		b.mu.Unlock()
	}
	return n, err
}

func (b *idleMonitoringBody) Close() error {
	b.mu.Lock()
	b.closed = true
	b.mu.Unlock()
	b.stopOnce.Do(func() { close(b.stopCh) })
	b.cancel()
	return b.rc.Close()
}

// noIdleBody 不监控的包装：只负责在 Close 时释放 ctx。
type noIdleBody struct {
	rc     io.ReadCloser
	cancel context.CancelFunc
	once   sync.Once
}

func (b *noIdleBody) Read(p []byte) (int, error) { return b.rc.Read(p) }

func (b *noIdleBody) Close() error {
	b.once.Do(b.cancel)
	return b.rc.Close()
}

// idleTickInterval 监控周期：idle/4，钳在 [10ms, 1s]。
// 与 CodeBuddy 的 idleTick 同口径（小 idle 也能快速发现，大值避免空转）。
func idleTickInterval(idle time.Duration) time.Duration {
	d := idle / 4
	if d > time.Second {
		d = time.Second
	}
	if d < 10*time.Millisecond {
		d = 10 * time.Millisecond
	}
	return d
}
