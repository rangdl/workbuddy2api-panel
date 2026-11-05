// 快过期积分（prefer_expiring）的两条硬性语义：
//   - 有快过期批次的账号必须进得了 Top5 短名单（硬优先键），而不是只靠权重乘数
//     碰运气——那个乘数只对「长期闲置」的号有效；
//   - 开关关掉时上述优先级完全消失。
package pool

import (
	"fmt"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// TestExpiringCreditsEnterShortlist 快过期积分是**硬优先键**：即便该号刚被用过
// （闲置补偿为 0）、余额远低于同池高余额号，也必须进入 Top5 短名单。
//
// 这是 routingWeightOf 的 ×expiringVirtualSlots 单独做不到的事：乘数只对「长期
// 闲置」的快过期号有效（闲置补偿先把它们抬进短名单）。一个刚被用过、只剩少量
// 积分当天到期的号，权重≈1，×3 仍进不了 Top5，那批积分每轮都白白过期。
func TestExpiringCreditsEnterShortlist(t *testing.T) {
	withNoPickGap(t)
	p := New("")
	p.SetCostExploreInterval(0)
	p.SetRandomSource(func(n int64) int64 { return 0 }) // r=0 → 抽中短名单第 1 个

	// 6 个高余额、从未使用过的号：权重最高（credits 项满 + 闲置项满）。
	for i := 0; i < 6; i++ {
		uid := fmt.Sprintf("rich%d", i)
		p.Add(&auth.Auth{UID: uid})
		p.SetCredits(uid, 1_000_000, 1_000_000)
	}
	// 一个只剩快过期积分的号，先让它「刚被用过」→ 闲置补偿为 0，权重最低。
	p.Add(&auth.Auth{UID: "expiring"})
	p.SetCreditsDetailed("expiring", 196, 196, 196, time.Now().Add(6*time.Hour), 196)
	if a := p.PickByUID("expiring"); a == nil {
		t.Fatal("PickByUID(expiring)=nil，无法构造「刚被用过」状态")
	}

	// 6 个 rich 的权重（11 + 闲置满分）远高于 expiring（≈1×3），无硬键时 expiring
	// 排第 7、必被截断；有硬键时它是短名单第 1 个，r=0 必然抽中它。
	if a := p.PickExcludingForRealm(nil, "", ""); a == nil || a.UID != "expiring" {
		got := "<nil>"
		if a != nil {
			got = a.UID
		}
		t.Fatalf("选中 %s，want expiring（快过期积分必须进入短名单）", got)
	}
}

// TestExpiringPriorityHonorsSwitch prefer_expiring=false 时上述硬优先键必须完全
// 失效（等于该特性不存在），否则关不掉的特性就是 bug。
func TestExpiringPriorityHonorsSwitch(t *testing.T) {
	withNoPickGap(t)
	p := New("")
	p.SetCostExploreInterval(0)
	p.SetRandomSource(func(n int64) int64 { return 0 })
	p.SetPreferExpiring(false)

	for i := 0; i < 6; i++ {
		uid := fmt.Sprintf("rich%d", i)
		p.Add(&auth.Auth{UID: uid})
		p.SetCredits(uid, 1_000_000, 1_000_000)
	}
	p.Add(&auth.Auth{UID: "expiring"})
	p.SetCreditsDetailed("expiring", 196, 196, 196, time.Now().Add(6*time.Hour), 196)
	if a := p.PickByUID("expiring"); a == nil {
		t.Fatal("PickByUID(expiring)=nil，无法构造「刚被用过」状态")
	}

	a := p.PickExcludingForRealm(nil, "", "")
	if a == nil {
		t.Fatal("选号返回 nil")
	}
	if a.UID == "expiring" {
		t.Fatal("prefer_expiring=false 时快过期号仍被优先，开关失效")
	}
}
