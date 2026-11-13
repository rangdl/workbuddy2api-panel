package trae

import (
	"github.com/linguo2625469/workbuddy2api-panel/internal/gateway"
	traepkg "github.com/linguo2625469/workbuddy2api-panel/internal/trae"
)

// Decide 把 Trae 的错误分类翻译成跨上游统一的处置决策。
//
// 与 CodeBuddy 的映射表对照着看：错误码完全不同（1005 vs ErrHardCredit），
// 但处置动作落在同一组 Action 上——这正是 gateway.Decision 存在的意义。
func Decide(c traepkg.Classification) gateway.Decision {
	d := gateway.Decision{
		Kind:    string(c.Kind),
		Code:    c.Code,
		Message: c.Message,
	}
	switch c.Kind {
	case traepkg.ErrPlanLimit:
		// 1005 套餐/积分额度用尽：长冷却，等次日或充值。
		d.Action, d.Rotate = gateway.ActionCoolLong, true
	case traepkg.ErrSessionDead:
		// HTTP 401：需重新登录，冷却不会自愈。
		d.Action, d.Rotate = gateway.ActionDisable, true
	case traepkg.ErrDeviceProof:
		// 20403/20405 设备不匹配：需重新导入设备凭证，同样不会自愈。
		d.Action, d.Rotate = gateway.ActionDisable, true
	case traepkg.ErrRiskControl:
		// 4017 触发风控：冷却无效，直接摘出轮转（用长冷却表达）。
		d.Action, d.Rotate = gateway.ActionCoolLong, true
	case traepkg.ErrSoftRate:
		d.Action, d.Rotate = gateway.ActionCoolShort, true
	case traepkg.ErrServer:
		d.Action, d.Rotate = gateway.ActionCoolMedium, true
	case traepkg.ErrModelUnknown:
		// 4001/4023：只避让该模型，不换号（换号还是同一个模型）。
		d.Action, d.Rotate = gateway.ActionModelCooldown, false
	case traepkg.ErrCheckinAlready, traepkg.ErrCheckinBusy:
		// 签到专用语义，不该出现在 chat 链路；出现则按请求级处理。
		d.Action, d.Rotate = gateway.ActionNone, false
	default:
		// ErrClient（其他 4xx）/ ErrNone：请求本身有问题，换号无用。
		d.Action, d.Rotate = gateway.ActionNone, false
	}
	if c.Cooldown > 0 {
		d.Cooldown = c.Cooldown
	}
	return d
}
