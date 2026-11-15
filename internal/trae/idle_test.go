package trae

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"
)

// blockingBody 读一次后永久阻塞，直到 ctx 取消（模拟上游卡死不吐数据）。
type blockingBody struct {
	first   bool
	unblock chan struct{}
	closed  bool
}

func newBlockingBody() *blockingBody {
	return &blockingBody{unblock: make(chan struct{})}
}

func (b *blockingBody) Read(p []byte) (int, error) {
	if !b.first {
		b.first = true
		return copy(p, "data: {}\n\n"), nil
	}
	// 卡住：只有被测代码主动断流（cancel → 底层实现收到关闭）才会退出。
	<-b.unblock
	return 0, io.EOF
}

func (b *blockingBody) Close() error {
	if !b.closed {
		b.closed = true
		close(b.unblock)
	}
	return nil
}

// TestIdleBodyCancelsOnStall 流卡住超过 idle 时必须主动断流。
//
// 回归点：Trae 此前**完全没有**流中空闲监控（CodeBuddy 有）。表现为请求挂住
// 直到外层 ctx 超时，期间占着在途名额与 goroutine，排查时看日志只有"没反应"。
func TestIdleBodyCancelsOnStall(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var cancelled int32
	body := newIdleBody(newBlockingBody(), 100*time.Millisecond, func() {
		cancelled = 1
		cancel()
	})
	defer body.Close()

	// 第一次读成功（有数据），之后卡住。
	buf := make([]byte, 64)
	if _, err := body.Read(buf); err != nil {
		t.Fatalf("首次读应成功: %v", err)
	}

	// 等 idle 触发：100ms idle → 监控周期 25ms，给足余量。
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cancelled == 1 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if cancelled != 1 {
		t.Fatal("流卡住超过 idle 后未触发 cancel（空闲监控未生效）")
	}
	select {
	case <-ctx.Done():
	default:
		t.Fatal("cancel 后 ctx 应已取消")
	}
}

// TestIdleBodyNoMonitorWhenDisabled idle<=0 时不监控，但 Close 仍释放 ctx。
func TestIdleBodyNoMonitorWhenDisabled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	var released bool
	body := newIdleBody(io.NopCloser(strings.NewReader("hello")), 0, func() {
		released = true
		cancel()
	})
	if _, err := io.ReadAll(body); err != nil {
		t.Fatal(err)
	}
	// idle=0 时不会因"空闲"取消，但 Close 必须释放（否则 ctx 泄漏）。
	if released {
		t.Fatal("idle<=0 不应主动 cancel")
	}
	if err := body.Close(); err != nil {
		t.Fatal(err)
	}
	if !released {
		t.Fatal("Close 必须调用 cancel，否则 ctx 泄漏")
	}
	_ = ctx
}

// TestIdleBodyReadsResetTimer 持续读到数据不应触发 cancel。
func TestIdleBodyReadsResetTimer(t *testing.T) {
	var cancelled bool
	pr, pw := io.Pipe()
	body := newIdleBody(pr, 80*time.Millisecond, func() { cancelled = true })
	defer body.Close()

	// 每 20ms 写一点，持续 400ms（远超 idle），期间保持活跃。
	go func() {
		for i := 0; i < 20; i++ {
			if _, err := pw.Write([]byte("x")); err != nil {
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
		pw.Close()
	}()

	buf := make([]byte, 16)
	for {
		if _, err := body.Read(buf); err != nil {
			break
		}
	}
	if cancelled {
		t.Fatal("持续有数据时不应判定为空闲")
	}
}

// TestSetTimeoutsAppliesHeaderTimeout 注入的首字节超时生效。
func TestSetTimeoutsAppliesHeaderTimeout(t *testing.T) {
	c := NewClient()
	c.SetTimeouts(45*time.Second, 90*time.Second)
	if got := c.transport.ResponseHeaderTimeout; got != 45*time.Second {
		t.Errorf("ResponseHeaderTimeout=%v want 45s", got)
	}
	if c.idleTimeout != 90*time.Second {
		t.Errorf("idleTimeout=%v want 90s", c.idleTimeout)
	}
	// <=0 保留内置默认，避免把超时误设成 0（=不限制）。
	c2 := NewClient()
	c2.SetTimeouts(0, 0)
	if got := c2.transport.ResponseHeaderTimeout; got != defaultStreamHeaderTimeout {
		t.Errorf("header=0 应保留内置默认 %v，得到 %v", defaultStreamHeaderTimeout, got)
	}
	if c2.idleTimeout != 0 {
		t.Errorf("idle=0 应表示不监控，得到 %v", c2.idleTimeout)
	}
}
