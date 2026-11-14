// account_pool.go 账号入池的唯一入口。
//
// 为什么需要这一层：账号对象要自带上游能力（Credits/Checkin/Keepalive），
// 而能力的依赖（HTTP client、快过期窗口）只有面板这一层知道。把包装收在
// 一处，新增入池路径时不会漏掉依赖注入——漏了的表现是「账号在池里但后台
// 刷新永远是 0」，非常难查。
package panel

import (
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	backendcb "github.com/linguo2625469/workbuddy2api-panel/internal/backend/codebuddy"
)

// addCodeBuddyAccount 把 CodeBuddy 账号包装后加入池。
//
// 所有新增 CodeBuddy 账号的路径（登录、导入）都必须走这里，不要直接
// pool.WrapAuth——那样包出来的账号没有 client，全部能力查询都会返回
// ErrUnsupported，表现为「签到/余额刷新对这些账号静默失效」。
func (p *Panel) addCodeBuddyAccount(a *auth.Auth) {
	if p.cfg.Pool == nil || a == nil {
		return
	}
	accs := backendcb.WrapWithClient([]*auth.Auth{a}, p.cfg.Upstream, func() time.Duration {
		return p.expiringSoonWindow()
	})
	for _, acc := range accs {
		p.cfg.Pool.Add(acc)
	}
}
