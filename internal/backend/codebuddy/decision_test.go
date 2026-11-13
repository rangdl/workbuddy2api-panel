package codebuddy

import (
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/gateway"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// TestDecideCodeBuddy CodeBuddy 错误码 → 统一处置动作。
func TestDecideCodeBuddy(t *testing.T) {
	cases := []struct {
		kind       upstream.ErrKind
		wantAction gateway.Action
		wantRotate bool
	}{
		// 账号级：换号有用
		{upstream.ErrHardCredit, gateway.ActionCoolLong, true},
		{upstream.ErrAccountFault, gateway.ActionCoolLong, true},
		{upstream.ErrSessionDead, gateway.ActionDisable, true},
		{upstream.ErrSoftRate, gateway.ActionCoolShort, true},
		{upstream.ErrServer, gateway.ActionCoolMedium, true},
		{upstream.ErrWafBlock, gateway.ActionCoolShort, true},
		{upstream.ErrNotFound, gateway.ActionCoolShort, true},
		// 模型级：不换号
		{upstream.ErrModelBlocked, gateway.ActionModelCooldown, false},
		// 请求级：不罚不换
		{upstream.ErrContentBlocked, gateway.ActionNone, false},
		{upstream.ErrBadParams, gateway.ActionNone, false},
		{upstream.ErrPromptTooLong, gateway.ActionNone, false},
		{upstream.ErrImageInvalid, gateway.ActionNone, false},
		{upstream.ErrUpstreamBusy, gateway.ActionNone, false},
		{upstream.ErrChannelRefused, gateway.ActionNone, false},
		{upstream.ErrClient, gateway.ActionNone, false},
		{upstream.ErrNone, gateway.ActionNone, false},
	}
	for _, c := range cases {
		d := Decide(c.kind, 400, "msg", 0)
		if d.Action != c.wantAction {
			t.Errorf("%s: Action = %v, want %v", c.kind, d.Action, c.wantAction)
		}
		if d.Rotate != c.wantRotate {
			t.Errorf("%s: Rotate = %v, want %v", c.kind, d.Rotate, c.wantRotate)
		}
	}
}

// TestDecideCarriesRetryAfter 上游明示的 Retry-After 要带进 Decision。
func TestDecideCarriesRetryAfter(t *testing.T) {
	d := Decide(upstream.ErrSoftRate, 429, "rate limited", 90e9)
	if d.Cooldown != 90e9 {
		t.Errorf("Cooldown = %v, want 90s", d.Cooldown)
	}
}
