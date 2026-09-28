package trae

import (
	"net/http"
	"testing"
	"time"
)

// TestClassifyBizCodes 业务码分类（Trae 的码表与 CodeBuddy 完全不同）。
func TestClassifyBizCodes(t *testing.T) {
	cases := []struct {
		name       string
		status     int
		body       string
		streamCode int64
		wantKind   ErrKind
		wantCool   time.Duration
		wantDis    bool
	}{
		{"1005 套餐额度用尽（响应体）", http.StatusOK, `{"code":1005,"message":"套餐额度用尽"}`, 0,
			ErrPlanLimit, planLimitCooldown, false},
		{"1005 流内错误", 0, "", 1005, ErrPlanLimit, planLimitCooldown, false},
		{"20403 设备不匹配", http.StatusBadRequest, `{"code":20403,"message":"Device not match"}`, 0,
			ErrDeviceProof, 0, true},
		{"20405 缺少设备签名", http.StatusBadRequest, `{"code":20405,"message":"Device proof required"}`, 0,
			ErrDeviceProof, 0, true},
		{"4023 模型未知", http.StatusOK, `{"code":4023,"message":"model unknown"}`, 0,
			ErrModelUnknown, 0, false},
		{"4001 模型不可用", http.StatusBadRequest, `{"code":4001,"message":"not available"}`, 0,
			ErrModelUnknown, 0, false},
		{"401 凭证失效（HTTP）", http.StatusUnauthorized, ``, 0, ErrSessionDead, 0, true},
		{"429 限流", http.StatusTooManyRequests, ``, 0, ErrSoftRate, softRateCooldown, false},
		{"500 上游异常", http.StatusInternalServerError, ``, 0, ErrServer, serverCooldown, false},
		{"400 请求问题（不冷却）", http.StatusBadRequest, `bad request`, 0, ErrClient, 0, false},
		{"404 请求问题", http.StatusNotFound, ``, 0, ErrClient, 0, false},
		{"无错误", http.StatusOK, ``, 0, ErrNone, 0, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := Classify(c.status, []byte(c.body), c.streamCode, "")
			if got.Kind != c.wantKind {
				t.Fatalf("Kind = %q, want %q（%+v）", got.Kind, c.wantKind, got)
			}
			if got.Cooldown != c.wantCool {
				t.Errorf("Cooldown = %v, want %v", got.Cooldown, c.wantCool)
			}
			if got.Disable != c.wantDis {
				t.Errorf("Disable = %v, want %v", got.Disable, c.wantDis)
			}
			// 无错误时不要求 message；有分类时必须有可读文案（兜底也算）。
			if c.wantKind != ErrNone && got.Message == "" {
				t.Error("Message 不应为空（兜底文案也必须可读）")
			}
		})
	}
}

// TestClassifyErrorObjectForm 兼容 {error:{code,message}} 形态。
func TestClassifyErrorObjectForm(t *testing.T) {
	got := Classify(http.StatusOK, []byte(`{"error":{"code":1005,"message":"quota"}}`), 0, "")
	if got.Kind != ErrPlanLimit {
		t.Fatalf("Kind = %q, want %q", got.Kind, ErrPlanLimit)
	}
}

// TestClassifyStreamMessagePriority 流内错误优先用上游原文作为 message。
func TestClassifyStreamMessagePriority(t *testing.T) {
	got := Classify(0, nil, 1005, "套餐额度用尽（原文）")
	if got.Message != "套餐额度用尽（原文）" {
		t.Errorf("Message = %q，应优先上游原文", got.Message)
	}
	if got.Code != "1005" {
		t.Errorf("Code = %q", got.Code)
	}
}

// TestClassifyUnknownCodeFallsBackToStatus 未知业务码 + HTTP 4xx → 按状态码分类。
func TestClassifyUnknownCodeFallsBackToStatus(t *testing.T) {
	got := Classify(http.StatusTooManyRequests, []byte(`{"code":99999,"message":"x"}`), 0, "")
	if got.Kind != ErrSoftRate {
		t.Fatalf("Kind = %q, want %q", got.Kind, ErrSoftRate)
	}
}

// TestCooldownsLifecycle 冷却表的标记 / 判定 / 清除 / 快照。
func TestCooldownsLifecycle(t *testing.T) {
	c := NewCooldowns()
	if !c.Available("u1") {
		t.Fatal("未标记的账号应可用")
	}
	c.Mark("u1", time.Hour, ErrPlanLimit, "额度用尽")
	if c.Available("u1") {
		t.Fatal("标记后应不可用")
	}
	snap := c.Snapshot()
	if len(snap) != 1 || snap[0].UID != "u1" || snap[0].Kind != string(ErrPlanLimit) {
		t.Fatalf("快照不符: %+v", snap)
	}
	if snap[0].RemainingSec <= 0 || snap[0].RemainingSec > 3600 {
		t.Errorf("RemainingSec = %d", snap[0].RemainingSec)
	}
	c.Clear("u1")
	if !c.Available("u1") {
		t.Fatal("清除后应可用")
	}
	if c.Count() != 0 {
		t.Errorf("清除后 Count = %d", c.Count())
	}
}

// TestCooldownsExpiry 过期条目惰性清理（不需要后台 GC）。
func TestCooldownsExpiry(t *testing.T) {
	c := NewCooldowns()
	c.Mark("u1", time.Millisecond, ErrSoftRate, "限流")
	time.Sleep(5 * time.Millisecond)
	if !c.Available("u1") {
		t.Fatal("过期后应可用")
	}
	if n := len(c.Snapshot()); n != 0 {
		t.Errorf("过期条目应被清理，剩 %d", n)
	}
}

// TestCooldownsNoopSafety 零值/空 uid 不应 panic，也不应产生条目。
func TestCooldownsNoopSafety(t *testing.T) {
	var nilC *Cooldowns
	if !nilC.Available("u1") {
		t.Error("nil 冷却表应视为全可用")
	}
	nilC.Mark("u1", time.Hour, ErrServer, "x") // 不应 panic
	nilC.Clear("u1")
	if nilC.Snapshot() != nil {
		t.Error("nil 冷却表快照应为 nil")
	}

	c := NewCooldowns()
	c.Mark("", time.Hour, ErrServer, "x") // 空 uid
	c.Mark("u2", 0, ErrServer, "x")       // 零时长
	if c.Count() != 0 {
		t.Errorf("空 uid / 零时长不应产生条目，得到 %d", c.Count())
	}
}

// TestCooldownsSnapshotOrder 快照按剩余时间降序（面板展示稳定）。
func TestCooldownsSnapshotOrder(t *testing.T) {
	c := NewCooldowns()
	c.Mark("short", time.Minute, ErrSoftRate, "a")
	c.Mark("long", time.Hour, ErrPlanLimit, "b")
	snap := c.Snapshot()
	if len(snap) != 2 {
		t.Fatalf("快照数 = %d", len(snap))
	}
	if snap[0].UID != "long" {
		t.Errorf("应按剩余时间降序，首条 = %s", snap[0].UID)
	}
}
