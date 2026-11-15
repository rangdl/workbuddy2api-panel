package scheduler

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/gateway"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
)

// TestCheckinBackendsFiltersByUpstream 分上游签到开关按上游生效。
//
// 语义（schedule.checkin_backends）：
//   - 显式 false → 该上游不签到
//   - 缺键 / nil / true → 该上游照常签到
//
// 「缺键 = 启用」是刻意的：新增上游忘了配这张表时，正确行为是照常签到，
// 而不是静默停掉一整类账号的签到——那会让账号慢慢饿死且没有任何报错。
//
// 用桩账号而不是真实 backend：本测试断言的是**调度决策**（哪些上游被调用），
// 与上游协议无关。真实 backend 会牵出 OAuth/HTTP，反而把测试变成集成测试。
func TestCheckinBackendsFiltersByUpstream(t *testing.T) {
	cases := []struct {
		name   string
		table  map[string]bool
		wantCB bool
		wantTr bool
	}{
		{"trae 显式关闭", map[string]bool{"trae": false}, true, false},
		{"codebuddy 显式关闭", map[string]bool{"codebuddy": false}, false, true},
		{"空表全启用", map[string]bool{}, true, true},
		{"nil 表全启用", nil, true, true},
		{"两者都关", map[string]bool{"codebuddy": false, "trae": false}, false, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := pool.New("")
			cb := &checkinProbeAccount{uid: "cb1", backend: "codebuddy"}
			tr := &checkinProbeAccount{uid: "tr1", backend: "trae"}
			p.Add(cb)
			p.Add(tr)

			s := New(Config{
				Pool:              p,
				CheckinBackends:   c.table,
				TravelDisabled:    true,
				ActivityDisabled:  true,
				KeepaliveDisabled: true,
				BlackcatDisabled:  true,
				GrowthDisabled:    true,
			})
			s.RunCheckinNow()

			if got := atomic.LoadInt32(&cb.hits) > 0; got != c.wantCB {
				t.Errorf("codebuddy 被签到=%v want %v", got, c.wantCB)
			}
			if got := atomic.LoadInt32(&tr.hits) > 0; got != c.wantTr {
				t.Errorf("trae 被签到=%v want %v", got, c.wantTr)
			}
		})
	}
}

// TestCheckinBackendEnabledDefaultsTrue 缺键 / nil 都算启用。
func TestCheckinBackendEnabledDefaultsTrue(t *testing.T) {
	cases := []struct {
		name string
		m    map[string]bool
		want bool
	}{
		{"nil 表", nil, true},
		{"空表", map[string]bool{}, true},
		{"无该键", map[string]bool{"other": false}, true},
		{"显式 true", map[string]bool{"trae": true}, true},
		{"显式 false", map[string]bool{"trae": false}, false},
	}
	for _, c := range cases {
		s := New(Config{CheckinBackends: c.m})
		if got := s.checkinBackendEnabled("trae"); got != c.want {
			t.Errorf("%s: got %v want %v", c.name, got, c.want)
		}
	}
}

// TestPoolBackendsReflectsActualAccounts 上游列表来自池里实际存在的账号。
//
// 配置里列着但没账号的上游不该出现（跑一轮是纯空转）；反过来，新加账号的
// 上游必须出现，即使配置里没提。
func TestPoolBackendsReflectsActualAccounts(t *testing.T) {
	p := pool.New("")
	if got := p.Backends(); len(got) != 0 {
		t.Fatalf("空池不应有上游: %v", got)
	}
	p.Add(&checkinProbeAccount{uid: "tr1", backend: "trae"})
	p.Add(&checkinProbeAccount{uid: "cb1", backend: "codebuddy"})
	got := p.Backends()
	// 去重 + 稳定排序
	if len(got) != 2 || got[0] != "codebuddy" || got[1] != "trae" {
		t.Fatalf("got %v want [codebuddy trae]", got)
	}
	// 同上游多账号不重复出现
	p.Add(&checkinProbeAccount{uid: "cb2", backend: "codebuddy"})
	if got := p.Backends(); len(got) != 2 {
		t.Fatalf("同一上游不应重复: %v", got)
	}
}

// checkinProbeAccount 记录自己被 Checkin 次数的最小账号实现。
type checkinProbeAccount struct {
	uid     string
	backend string
	hits    int32
}

func (a *checkinProbeAccount) UID() string      { return a.uid }
func (a *checkinProbeAccount) Backend() string  { return a.backend }
func (a *checkinProbeAccount) Realm() string    { return "cn" }
func (a *checkinProbeAccount) Nickname() string { return "" }
func (a *checkinProbeAccount) Enterprise() bool { return false }
func (a *checkinProbeAccount) Credits(context.Context) (*gateway.Credits, error) {
	return &gateway.Credits{}, nil
}
func (a *checkinProbeAccount) Checkin(context.Context) (*gateway.CheckinResult, error) {
	atomic.AddInt32(&a.hits, 1)
	return &gateway.CheckinResult{Already: true}, nil
}
func (a *checkinProbeAccount) Keepalive(context.Context) error { return nil }

// 避免 "imported and not used"：NeedsRefresh/Freshen 是 Freshener 可选能力的
// 形状，这里不实现（不实现 = 不支持该能力，与本测试无关）。
var _ = time.Second
