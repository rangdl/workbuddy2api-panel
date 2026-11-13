package codebuddy

import (
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/gateway"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// Decide 把 CodeBuddy 的错误分类翻译成跨上游统一的处置决策。
//
// 映射原则是「对账号做什么」，不是「错误叫什么」：
//   - 余额不足   → 长冷却 + 换号（换个号能过）
//   - 会话失效   → 禁用 + 换号（冷却不会自愈，需重新登录）
//   - 软限流     → 短冷却 + 换号
//   - 上游 5xx   → 中冷却 + 换号
//   - 模型不可用 → 仅该 (账号,模型) 避让，**不换号**（换号还是同一个模型）
//   - 请求级错误 → 不罚号、不换号（换谁都是同一个错，白烧额度）
func Decide(kind upstream.ErrKind, status int, msg string, retryAfter time.Duration) gateway.Decision {
	d := gateway.Decision{
		Kind:    kind.String(),
		Code:    statusCode(status),
		Message: msg,
	}
	switch kind {
	case upstream.ErrHardCredit:
		d.Action, d.Rotate = gateway.ActionCoolLong, true
	case upstream.ErrAccountFault:
		// 11140 request illegal / 14017 trial not activated：账号级授权/配额故障。
		d.Action, d.Rotate = gateway.ActionCoolLong, true
	case upstream.ErrSessionDead:
		d.Action, d.Rotate = gateway.ActionDisable, true
	case upstream.ErrSoftRate:
		d.Action, d.Rotate = gateway.ActionCoolShort, true
	case upstream.ErrServer:
		d.Action, d.Rotate = gateway.ActionCoolMedium, true
	case upstream.ErrWafBlock:
		// WAF 拦截页：账号软冷却 + 抖动退避（与 ErrSoftRate 同档）。
		d.Action, d.Rotate = gateway.ActionCoolShort, true
	case upstream.ErrNotFound:
		// 上游偶发 404：短冷却，不累计错误计数（防雪崩）。
		d.Action, d.Rotate = gateway.ActionCoolShort, true
	case upstream.ErrModelBlocked:
		// 11102「该后端无此模型」：(账号,模型) 负缓存，账号对其余模型仍健康。
		d.Action, d.Rotate = gateway.ActionModelCooldown, false
	case upstream.ErrContentBlocked, upstream.ErrBadParams, upstream.ErrPromptTooLong,
		upstream.ErrImageInvalid, upstream.ErrUpstreamBusy, upstream.ErrChannelRefused:
		// 全部是请求级问题：换号无用。
		d.Action, d.Rotate = gateway.ActionNone, false
	default:
		// ErrClient（未知 4xx）/ ErrNone：保守——不罚号也不换号。
		//
		// 取舍说明：跨上游回退场景下，ErrClient 换到另一个上游**可能**能过；
		// 但误判的代价是白烧一个健康号的风控额度，收益不确定。故保持保守，
		// 仅当分类明确指向账号问题时才换号。
		d.Action, d.Rotate = gateway.ActionNone, false
	}
	d.Cooldown = retryAfter
	return d
}

// statusCode 把 HTTP 状态转成字符串码（空状态返回空串）。
func statusCode(status int) string {
	switch {
	case status <= 0:
		return ""
	case status < 100 || status > 599:
		return ""
	}
	return itoa(status)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [8]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}
