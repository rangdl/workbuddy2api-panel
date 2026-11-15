// account_pool.go 账号入池的唯一入口。
//
// 为什么需要这一层：账号对象要自带上游能力（Credits/Checkin/Keepalive），
// 而能力的依赖（HTTP client、快过期窗口）只有装配层知道。把包装收在一处，
// 新增入池路径时不会漏掉依赖注入——漏了的表现是「账号在池里但后台刷新
// 永远是 0」，非常难查。
//
// 为什么要装配期注入而不是本包直接调 backend：包直接 import
// backend/codebuddy 会让 panel 依赖具体上游的构造细节（Client、
// ExpiringSoon 闭包），新增一个可登录的上游就要改本包。现在包装方式由
// cmd/server 提供（Config.WrapAccount），panel 只负责「调它 + 入池」。
package panel

import (
	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/gateway"
)

// addCodeBuddyAccount 把 CodeBuddy 账号包装后加入池。
//
// 所有新增 CodeBuddy 账号的路径（登录、导入）都必须走这里，不要直接
// pool.WrapAuth——那样包出来的账号没有 client，全部能力查询都会返回
// ErrUnsupported，表现为「签到/余额刷新对这些账号静默失效」。
//
// 未注入 WrapAccount 时（测试裸用）退化为不入池：静默丢账号比包出没有
// 能力的账号更难查，所以这里什么都不做，由调用方自己 pool.Add。
func (p *Panel) addCodeBuddyAccount(a *auth.Auth) {
	if p.cfg.Pool == nil || a == nil || p.cfg.WrapAccount == nil {
		return
	}
	for _, acc := range p.cfg.WrapAccount(a) {
		if acc != nil {
			p.cfg.Pool.Add(acc)
		}
	}
}

// codeBuddyCredential 取 CodeBuddy 账号凭证（装配期注入）。
//
// 面板的成长任务/余额/积分包接口全是 CodeBuddy 专有能力，本来就需要
// 原始 *auth.Auth。本包不 import 具体 backend，由 cmd/server 提供闭包。
//
// 未注入时返回 nil：调用方按「账号不存在」处理（与 pool.AuthByUID 对
// 非 CodeBuddy 账号返回 nil 的既有语义一致）。
func (p *Panel) codeBuddyCredential(uid string) *auth.Auth {
	if p.cfg.CredentialFor == nil {
		return nil
	}
	return p.cfg.CredentialFor(uid)
}

// traeCredential 取 Trae 账号凭证（装配期注入；nil = 该账号不是 Trae 或未注入）。
func (p *Panel) traeCredential(acc gateway.Account) any {
	if p.cfg.TraeCredential == nil {
		return nil
	}
	return p.cfg.TraeCredential(acc)
}
