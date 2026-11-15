package usage

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// usage_backend_test.go 守「上游与域是两个正交维度」这条约束。
//
// 历史缺陷：Trae 账号通过 gateway.UsageLabel 把 "trae" 写进 bucket.Realm，
// 于是面板「按域」出现 cn / global / trae 三行——上游被当成了与域同级的东西。
// 真实关系是：CodeBuddy 有 cn/global 两个域，Trae 只有 cn 域；同一个 cn 域里
// 可能同时有这两个上游。
//
// 这组测试断言三件事：
//  1. by_realm 只含真实域，绝不出现上游名；
//  2. by_backend 独立成立；
//  3. 旧格式数据（realm 里存的是上游名）加载后被正确迁移，且被污染过的
//     Trae 积分字段被清空。

// TestByRealmNeverContainsBackendName 按域聚合不得出现上游名。
func TestByRealmNeverContainsBackendName(t *testing.T) {
	r := New("")
	now := time.Now()
	// 两个上游、同一个 cn 域 —— 这正是真实部署的形态。
	r.Add(now, "codebuddy", "cn", "cb1", "glm-5.2", Delta{TotalTokens: 100, HasTotal: true}, true)
	r.Add(now, "trae", "cn", "tr1", "deepseek-v4.1-flash", Delta{TotalTokens: 200, HasTotal: true}, true)
	r.Add(now, "codebuddy", "global", "cb2", "claude-4.6", Delta{TotalTokens: 300, HasTotal: true}, true)

	s := r.Snapshot(24, nil)

	realms := map[string]bool{}
	for _, row := range s.ByRealm {
		realms[row.Key] = true
		if row.Key != "cn" && row.Key != "global" {
			t.Errorf("by_realm 出现非域值 %q —— 上游名不能进 realm", row.Key)
		}
	}
	if !realms["cn"] || !realms["global"] {
		t.Fatalf("by_realm 应含 cn 与 global，得到 %v", realms)
	}
	// cn 域必须把两个上游的 token 合并（600 = 100 + 200 + 300 里的前两项）。
	for _, row := range s.ByRealm {
		if row.Key == "cn" && row.TotalTokens != 300 {
			t.Errorf("cn 域 total = %d, want 300（两个上游在同一域应合并）", row.TotalTokens)
		}
	}

	// by_backend 独立：codebuddy 两笔、trae 一笔。
	backends := map[string]int64{}
	for _, row := range s.ByBackend {
		if row.Key != "codebuddy" && row.Key != "trae" {
			t.Errorf("by_backend 出现非上游值 %q", row.Key)
		}
		backends[row.Key] = row.TotalTokens
	}
	if backends["codebuddy"] != 400 || backends["trae"] != 200 {
		t.Errorf("by_backend = %v, want codebuddy=400 trae=200", backends)
	}
}

// TestByAccountCarriesBackendAndRealm 账号行必须同时带上两个维度。
func TestByAccountCarriesBackendAndRealm(t *testing.T) {
	r := New("")
	now := time.Now()
	r.Add(now, "trae", "cn", "tr1", "deepseek-v4.1-flash", Delta{TotalTokens: 10, HasTotal: true}, true)

	s := r.Snapshot(24, nil)
	if len(s.ByAccount) != 1 {
		t.Fatalf("by_account = %d 行, want 1", len(s.ByAccount))
	}
	row := s.ByAccount[0]
	if row.Backend != "trae" || row.Realm != "cn" {
		t.Fatalf("账号行 backend/realm = %q/%q, want trae/cn", row.Backend, row.Realm)
	}
}

// TestLegacyRealmMigration 旧数据把上游名存在 realm 里，加载后要还原成两列。
//
// 同时断言 Trae 的积分字段被清空：那是按「token × 倍率」估算出来的，
// 而 consumption_rate 是相对倍率不是每 token 积分数，估出来虚高约 1500 倍。
func TestLegacyRealmMigration(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "usage.json")

	// 版本 4 的落盘形态：没有 b 字段，realm 直接是 "trae"，cr 是虚高估算值。
	legacy := `{"version":4,"saved":"2026-11-13T18:00:00Z","buckets":[
		{"s":"h:2026-11-13T18","r":"trae","u":"tr1","m":"deepseek-v4.1-flash",
		 "q":8,"p":2186,"c":352,"t":2538,"cr":152.28,"cn":8,"ct":2538},
		{"s":"h:2026-11-14T15","r":"cn","u":"cb1","m":"hy3",
		 "q":4,"p":479,"c":165,"t":644,"cr":3.2,"cn":4,"ct":644}
	]}`
	if err := os.WriteFile(path, []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}

	r := New(path)
	s := r.Snapshot(0, nil) // 全部历史

	// 1) realm 分组只有 cn（旧 Trae 只有 cn 域；旧 CodeBuddy 桶本来就是 cn）。
	for _, row := range s.ByRealm {
		if row.Key != "cn" {
			t.Errorf("迁移后 by_realm 仍出现 %q", row.Key)
		}
	}
	// 2) 上游维度被正确还原。
	backends := map[string]bool{}
	for _, row := range s.ByBackend {
		backends[row.Key] = true
	}
	if !backends["trae"] || !backends["codebuddy"] {
		t.Errorf("迁移后 by_backend = %v, want 含 trae 与 codebuddy", backends)
	}
	// 3) Trae 账号行的 backend/realm 两列都对。
	for _, row := range s.ByAccount {
		if row.Key == "tr1" {
			if row.Backend != "trae" || row.Realm != "cn" {
				t.Errorf("tr1 行 = backend %q realm %q, want trae/cn", row.Backend, row.Realm)
			}
		}
	}
	// 4) Trae 的虚高积分被作废，CodeBuddy 的实测积分保留。
	if len(s.CreditByAccount) != 1 {
		t.Fatalf("credit_by_account = %d 行, want 1（Trae 的估算应被清掉）", len(s.CreditByAccount))
	}
	if got := s.CreditByAccount[0]; got.Key != "cb1" || got.Credits != 3.2 {
		t.Errorf("保留的积分行 = %+v, want cb1/3.2（CodeBuddy 实测值不受影响）", got)
	}
	// 5) token 是真实观测，必须完整保留。
	if s.Totals.TotalTokens != 2538+644 {
		t.Errorf("total tokens = %d, want %d（token 计数不能被迁移影响）",
			s.Totals.TotalTokens, 2538+644)
	}
}

// TestMigrationIsIdempotent 迁移写回后再次加载不得改变结果。
func TestMigrationIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "usage.json")
	legacy := `{"version":4,"buckets":[
		{"s":"h:2026-11-13T18","r":"trae","u":"tr1","m":"m","t":100,"cr":99.9,"cn":1,"ct":100}
	]}`
	if err := os.WriteFile(path, []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}

	r1 := New(path)
	r1.flush(true) // 迁移结果落盘
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var f file
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatal(err)
	}
	if len(f.Buckets) != 1 {
		t.Fatalf("落盘桶数 = %d, want 1", len(f.Buckets))
	}
	if f.Buckets[0].Backend != "trae" || f.Buckets[0].Realm != "cn" {
		t.Fatalf("落盘后 backend/realm = %q/%q, want trae/cn", f.Buckets[0].Backend, f.Buckets[0].Realm)
	}
	if f.Buckets[0].CR != 0 {
		t.Fatalf("落盘后 cr = %v, want 0（虚高估算已作废）", f.Buckets[0].CR)
	}

	// 再加载一次：数值不得变化（不重复加总、不复活积分）。
	r2 := New(path)
	s2 := r2.Snapshot(0, nil)
	if s2.Totals.TotalTokens != 100 {
		t.Errorf("二次加载 total = %d, want 100", s2.Totals.TotalTokens)
	}
	if s2.Totals.Credits != 0 {
		t.Errorf("二次加载 credits = %v, want 0", s2.Totals.Credits)
	}
}
