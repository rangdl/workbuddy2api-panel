package pool

import (
	"context"
	"encoding/json"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/gateway"
)

// capableAccount 测试用账号：可选地实现某一组特色能力接口。
type capableAccount struct {
	uid     string
	backend string
	// 各能力的调用计数（未实现的能力为 nil）。
	travel   *atomic.Int32
	blindbox *atomic.Int32
	streak   *atomic.Int32
	// unsupported 为 true 时各能力返回 ErrUnsupported（模拟企业版/global）。
	unsupported bool
}

func (a capableAccount) UID() string      { return a.uid }
func (a capableAccount) Nickname() string { return "nick-" + a.uid }
func (a capableAccount) Realm() string    { return "cn" }
func (a capableAccount) Backend() string  { return a.backend }
func (a capableAccount) Enterprise() bool { return a.unsupported }

func (a capableAccount) Credits(context.Context) (*gateway.Credits, error) {
	return nil, gateway.ErrUnsupported
}
func (a capableAccount) Checkin(context.Context) (*gateway.CheckinResult, error) {
	return nil, gateway.ErrUnsupported
}
func (a capableAccount) Keepalive(context.Context) error { return gateway.ErrUnsupported }

// travelOnlyAccount 只实现 Traveler。
type travelOnlyAccount struct{ capableAccount }

func (a travelOnlyAccount) Travel(ctx context.Context, _ gateway.TravelOptions) (gateway.TravelOutcome, error) {
	if a.unsupported {
		return gateway.TravelOutcome{}, gateway.ErrUnsupported
	}
	a.travel.Add(1)
	return gateway.TravelOutcome{Action: "departed"}, nil
}

func (a travelOnlyAccount) TravelState(context.Context) (gateway.TravelSnapshot, error) {
	if a.unsupported {
		return gateway.TravelSnapshot{}, gateway.ErrUnsupported
	}
	return gateway.TravelSnapshot{State: "idle"}, nil
}

// blindnessAccount 只实现 Blindboxer。
type blindnessAccount struct{ capableAccount }

func (a blindnessAccount) Blindbox(ctx context.Context, apply bool, _ int) (gateway.BlindboxOutcome, error) {
	a.blindbox.Add(1)
	return gateway.BlindboxOutcome{WillOpen: 1, Opened: apply, Decision: "ok"}, nil
}

// TestTravelAllSkipsAccountsWithoutCapability 未实现 Traveler 的账号被静默跳过。
//
// 这是「池不认识上游」的核心保证：没有旅行玩法的上游不该被卷进旅行流程，
// 也不该报错（它不是失败，是「没这个概念」）。
func TestTravelAllSkipsAccountsWithoutCapability(t *testing.T) {
	defer SetCapabilityDelay(0)
	SetCapabilityDelay(0)

	capable := travelOnlyAccount{capableAccount{uid: "t1", backend: "codebuddy", travel: &atomic.Int32{}}}
	plain := capableAccount{uid: "t2", backend: "trae"}

	p := New("")
	p.Add(capable)
	p.Add(plain)

	got := p.TravelAll(context.Background())
	if len(got) != 1 {
		t.Fatalf("应只返回 1 个结果，实际 %d", len(got))
	}
	if got[0].UID != "t1" {
		t.Errorf("UID = %q, want t1", got[0].UID)
	}
	if n := capable.travel.Load(); n != 1 {
		t.Errorf("travel 调用 %d 次，want 1", n)
	}
}

// TestTravelAllTreatsUnsupportedAsSkip ErrUnsupported 不算失败（不记 Err）。
func TestTravelAllTreatsUnsupportedAsSkip(t *testing.T) {
	SetCapabilityDelay(0)
	acc := travelOnlyAccount{capableAccount{
		uid: "t1", backend: "codebuddy", travel: &atomic.Int32{}, unsupported: true,
	}}
	p := New("")
	p.Add(acc)

	got := p.TravelAll(context.Background())
	if len(got) != 1 {
		t.Fatalf("应返回 1 个结果（含跳过），实际 %d", len(got))
	}
	if !got[0].Unsupported {
		t.Error("应标记 Unsupported")
	}
	if got[0].Err != nil {
		t.Errorf("Unsupported 不应带 Err，实际 %v", got[0].Err)
	}
}

// TestTravelAllSkipsDisabledByDefault 已禁用账号默认不参与。
func TestTravelAllSkipsDisabledByDefault(t *testing.T) {
	SetCapabilityDelay(0)
	counter := &atomic.Int32{}
	p := New("")
	p.Add(travelOnlyAccount{capableAccount{uid: "t1", backend: "codebuddy", travel: counter}})
	p.Disable("t1", "test")

	if got := p.TravelAll(context.Background()); len(got) != 0 {
		t.Fatalf("禁用账号不应参与，实际返回 %d 个", len(got))
	}
	if n := counter.Load(); n != 0 {
		t.Errorf("travel 调用 %d 次，want 0", n)
	}
}

// TestTravelAllIncludesPaused 暂停选号的账号照常参与（旅行是纯 RPC）。
func TestTravelAllIncludesPaused(t *testing.T) {
	SetCapabilityDelay(0)
	counter := &atomic.Int32{}
	p := New("")
	p.Add(travelOnlyAccount{capableAccount{uid: "t1", backend: "codebuddy", travel: counter}})
	p.Pause("t1")

	if got := p.TravelAll(context.Background()); len(got) != 1 {
		t.Fatalf("暂停账号应参与旅行，实际返回 %d 个", len(got))
	}
	if n := counter.Load(); n != 1 {
		t.Errorf("travel 调用 %d 次，want 1", n)
	}
}

// TestTravelAllRemembersRetryTomorrow RetryTomorrow 后当日只跳过领养，不跳过整趟巡检。
//
// 关键语义：猫可能已被领养、行程可能到站需要领奖，这些与领养门槛无关。
// 所以台账只影响 SkipAdopt 标志，账号仍会被调用。
func TestTravelAllRemembersRetryTomorrow(t *testing.T) {
	SetCapabilityDelay(0)
	var calls atomic.Int32
	acc := &retryTomorrowAccount{count: &calls}
	p := New("")
	p.Add(acc)

	p.TravelAll(context.Background()) // 第 1 次：领养门槛未达，记台账
	if calls.Load() != 1 {
		t.Fatalf("首次应调用 1 次，实际 %d", calls.Load())
	}
	p.TravelAll(context.Background()) // 第 2 次：仍会调用，但带 SkipAdopt
	if calls.Load() != 2 {
		t.Fatalf("第二次仍应调用（只跳过领养），实际 %d", calls.Load())
	}
	if !acc.lastSkipAdopt.Load() {
		t.Error("第二次应带 SkipAdopt=true")
	}
}

// retryTomorrowAccount 首次返回 RetryTomorrow，之后记录 SkipAdopt。
type retryTomorrowAccount struct {
	capableAccount
	count         *atomic.Int32
	lastSkipAdopt atomic.Bool
}

func (a *retryTomorrowAccount) Travel(_ context.Context, opts gateway.TravelOptions) (gateway.TravelOutcome, error) {
	n := a.count.Add(1)
	if n == 1 {
		return gateway.TravelOutcome{Action: "skipped", RetryTomorrow: true}, nil
	}
	a.lastSkipAdopt.Store(opts.SkipAdopt)
	return gateway.TravelOutcome{Action: "skipped"}, nil
}

func (a *retryTomorrowAccount) TravelState(context.Context) (gateway.TravelSnapshot, error) {
	return gateway.TravelSnapshot{State: "idle"}, nil
}

// TestBlindboxAllPassesApplyFlag dry-run 与真实开盒的区分透传到账号实现。
func TestBlindboxAllPassesApplyFlag(t *testing.T) {
	SetCapabilityDelay(0)
	counter := &atomic.Int32{}
	p := New("")
	p.Add(blindnessAccount{capableAccount{uid: "t1", backend: "codebuddy", blindbox: counter}})

	dry := p.BlindboxAll(context.Background(), false, 5)
	if len(dry) != 1 || dry[0].Opened {
		t.Errorf("dry-run 不应开盒，实际 %+v", dry)
	}
	real := p.BlindboxAll(context.Background(), true, 5)
	if len(real) != 1 || !real[0].Opened {
		t.Errorf("apply 应开盒，实际 %+v", real)
	}
	if n := counter.Load(); n != 2 {
		t.Errorf("Blindbox 调用 %d 次，want 2", n)
	}
}

// TestCapabilityMethodsNoAccountsSafe 空池调用全部批量方法不应 panic。
func TestCapabilityMethodsNoAccountsSafe(t *testing.T) {
	SetCapabilityDelay(0)
	p := New("")
	ctx := context.Background()
	if got := p.TravelAll(ctx); len(got) != 0 {
		t.Errorf("空池 TravelAll 应返回空，实际 %d", len(got))
	}
	if got := p.BlindboxAll(ctx, false, 5); len(got) != 0 {
		t.Errorf("空池 BlindboxAll 应返回空，实际 %d", len(got))
	}
	if got := p.StreakAll(ctx); len(got) != 0 {
		t.Errorf("空池 StreakAll 应返回空，实际 %d", len(got))
	}
	if got := p.BlackcatAll(ctx); len(got) != 0 {
		t.Errorf("空池 BlackcatAll 应返回空，实际 %d", len(got))
	}
	if got := p.CompensationStatusAll(ctx); len(got) != 0 {
		t.Errorf("空池 CompensationStatusAll 应返回空，实际 %d", len(got))
	}
	if got := p.CompensationClaimAll(ctx); len(got) != 0 {
		t.Errorf("空池 CompensationClaimAll 应返回空，实际 %d", len(got))
	}
	if got := p.ActivityAll(ctx); len(got) != 0 {
		t.Errorf("空池 ActivityAll 应返回空，实际 %d", len(got))
	}
}

// TestCompensationClaimOneRejectsDisabled 单号入口必须挡住已禁用账号。
//
// 否则会出现「批量按钮跳过禁用号、手动按钮却能领」的割裂。
func TestCompensationClaimOneRejectsDisabled(t *testing.T) {
	p := New("")
	p.Add(capableAccount{uid: "u1", backend: "codebuddy"})
	p.Disable("u1", "test")

	res, err := p.CompensationClaimOne(context.Background(), "u1")
	if err != nil {
		t.Fatalf("不应报错: %v", err)
	}
	if res.Skip != CompSkipDisabled {
		t.Errorf("skip=%q want %q", res.Skip, CompSkipDisabled)
	}
}

// TestCompensationClaimOneUnknownAccount 不存在的账号返回 ErrNoSuchAccount。
func TestCompensationClaimOneUnknownAccount(t *testing.T) {
	p := New("")
	_, err := p.CompensationClaimOne(context.Background(), "nope")
	if !errors.Is(err, ErrNoSuchAccount) {
		t.Errorf("err = %v, want ErrNoSuchAccount", err)
	}
}

// TestBlindboxResultJSONContract 盲盒结果的 JSON 字段名是前端契约。
//
// 回归点：gateway.BlindboxOutcome 曾完全没有 json tag，经 BlindboxResult 嵌入
// 摊平后序列化成 TravelState/WillOpen 这种 Go 字段名，而前端读的是
// travel_state / will_open —— 面板表格整列显示空白。
func TestBlindboxResultJSONContract(t *testing.T) {
	r := BlindboxResult{
		UID:      "u1",
		Nickname: "甲",
		BlindboxOutcome: gateway.BlindboxOutcome{
			TravelState: "idle",
			Energy:      125,
			Affordable:  5,
			CostPerOpen: 10,
			MaxOpen:     5,
			WillOpen:    3,
			Decision:    "可开 3 次",
			Opened:      true,
			Prize:       "量子喵",
		},
	}
	raw, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	// 前端 panel/js/accounts.js 实际读取的键。
	for _, k := range []string{
		"uid", "nickname", "travel_state", "energy", "affordable",
		"cost_per_open", "max_open_count", "will_open", "decision", "opened", "prize",
	} {
		if _, ok := m[k]; !ok {
			t.Errorf("缺少 JSON 键 %q（前端契约）", k)
		}
	}
	// Go 字段名不应泄漏到线上。
	for _, k := range []string{"TravelState", "WillOpen", "MaxOpen", "CostPerOpen"} {
		if _, ok := m[k]; ok {
			t.Errorf("泄漏了 Go 字段名 %q", k)
		}
	}
}

// TestCompensationStatusAllKeepsDisabledRow 禁用账号必须以 skip 行出现在面板结果里。
//
// 回归点：CompensationStatusAll 曾走 IncludeDisabled=false 的账号集合，禁用号
// 被直接过滤掉——面板表格里凭空少一行，用户以为账号丢了。旧 compTargets 是
// 明确把 disabled 作为 skip 行返回的。
func TestCompensationStatusAllKeepsDisabledRow(t *testing.T) {
	p := New("")
	p.Add(capableAccount{uid: "u1", backend: "codebuddy"})
	p.Add(capableAccount{uid: "u2", backend: "codebuddy"})
	p.Disable("u2", "凭证失效")

	rows := p.CompensationStatusAll(context.Background())
	if len(rows) != 2 {
		t.Fatalf("应返回 2 行（含禁用号），实际 %d", len(rows))
	}
	var found bool
	for _, r := range rows {
		if r.UID != "u2" {
			continue
		}
		found = true
		if r.Skip != CompSkipDisabled {
			t.Errorf("禁用号 skip=%q, want %q", r.Skip, CompSkipDisabled)
		}
		if r.SkipMsg == "" {
			t.Error("禁用号应有 skip 说明（面板要显示原因）")
		}
	}
	if !found {
		t.Error("禁用号 u2 从结果里消失了")
	}
}

// TestBlackcatAllRespectsWindow 窗口外不得发起任何上游调用。
//
// 回归点：窗口守卫原只在 scheduler 层，任何新的 Pool.BlackcatAll 调用方都会
// 绕过它，在白天白烧真实对话额度。
func TestBlackcatAllRespectsWindow(t *testing.T) {
	SetCapabilityDelay(0)
	defer SetBlackcatWindowCheck(nil)

	calls := &atomic.Int32{}
	p := New("")
	p.Add(blackcatAccount{capableAccount{uid: "u1", backend: "codebuddy"}, calls})

	// 窗口外：直接返回空，不打上游。
	SetBlackcatWindowCheck(func(time.Time) bool { return false })
	if got := p.BlackcatAll(context.Background()); len(got) != 0 {
		t.Errorf("窗口外应返回空，实际 %d 行", len(got))
	}
	if n := calls.Load(); n != 0 {
		t.Errorf("窗口外不应发起上游调用，实际 %d 次", n)
	}

	// 窗口内：正常执行。
	SetBlackcatWindowCheck(func(time.Time) bool { return true })
	got := p.BlackcatAll(context.Background())
	if len(got) != 1 {
		t.Errorf("窗口内应返回 1 行，实际 %d", len(got))
	}
	if n := calls.Load(); n != 1 {
		t.Errorf("窗口内应调用 1 次，实际 %d", n)
	}
}

// blackcatAccount 只实现 BlackcatRunner。
type blackcatAccount struct {
	capableAccount
	calls *atomic.Int32
}

func (a blackcatAccount) RunBlackcat(context.Context) (gateway.BlackcatOutcome, error) {
	a.calls.Add(1)
	return gateway.BlackcatOutcome{Need: 1, Completed: 1}, nil
}
