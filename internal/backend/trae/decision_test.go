package trae

import (
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/gateway"
	traepkg "github.com/linguo2625469/workbuddy2api-panel/internal/trae"
)

// TestDecideTrae Trae 错误码 → 统一处置动作。
func TestDecideTrae(t *testing.T) {
	cases := []struct {
		kind       traepkg.ErrKind
		wantAction gateway.Action
		wantRotate bool
	}{
		// 账号级问题：换号有用。
		{traepkg.ErrPlanLimit, gateway.ActionCoolLong, true},
		{traepkg.ErrSessionDead, gateway.ActionDisable, true},
		{traepkg.ErrDeviceProof, gateway.ActionDisable, true},
		{traepkg.ErrRiskControl, gateway.ActionCoolLong, true},
		{traepkg.ErrSoftRate, gateway.ActionCoolShort, true},
		{traepkg.ErrServer, gateway.ActionCoolMedium, true},
		// 模型级：只避让该模型，换号无用。
		{traepkg.ErrModelUnknown, gateway.ActionModelCooldown, false},
		// 请求级：不罚号不换号。
		{traepkg.ErrClient, gateway.ActionNone, false},
		{traepkg.ErrNone, gateway.ActionNone, false},
	}
	for _, c := range cases {
		d := Decide(traepkg.Classification{Kind: c.kind})
		if d.Action != c.wantAction {
			t.Errorf("%s: Action = %v, want %v", c.kind, d.Action, c.wantAction)
		}
		if d.Rotate != c.wantRotate {
			t.Errorf("%s: Rotate = %v, want %v", c.kind, d.Rotate, c.wantRotate)
		}
	}
}

// TestDecideTraeCarriesFields 原始码与原因要透传（日志/面板用）。
func TestDecideTraeCarriesFields(t *testing.T) {
	d := Decide(traepkg.Classification{
		Kind: traepkg.ErrPlanLimit, Code: "1005", Message: "quota exhausted",
	})
	if d.Code != "1005" || d.Message != "quota exhausted" || d.Kind != "plan_limit" {
		t.Errorf("字段未透传: %+v", d)
	}
}
