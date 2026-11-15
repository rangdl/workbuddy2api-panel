package pool

// shim_test.go 把 CodeBuddy 的适配器重导出给池的测试。
//
// 背景：adapter.go 拆分后，WrapAuth / UnwrapAuth 搬去了
// backend/codebuddy/placeholder.go（它们持有 *auth.Auth，留在池里会让
// internal/pool 依赖 internal/auth，见 docs/architecture.md「已知例外①」）。
//
// 但池的**测试**需要构造大量便宜账号来驱动状态机——那些测试测的是冷却、
// 熔断、域过滤、积分保底，与 CodeBuddy 凭证无关。如果每个测试都改成
// `p.Add(backendcb.WrapAuth(...))`，241 处调用点要全改，而语义上这些测试
// 也不该关心上游是哪个。
//
// 所以只在测试构建里重导出：生产代码看不到这两个符号，分层约束保持。
// 这里 import backend/codebuddy 是**测试专属**的，不影响 pool 的依赖图
// （go list -deps ./internal/pool 仍然没有 internal/auth 与 backend/*）。

import (
	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	backendcb "github.com/linguo2625469/workbuddy2api-panel/internal/backend/codebuddy"
	"github.com/linguo2625469/workbuddy2api-panel/internal/gateway"
)

// WrapAuth 测试用：把 *auth.Auth 包成池账号（占位形态，无 client）。
func WrapAuth(a *auth.Auth) gateway.Account { return backendcb.WrapAuth(a) }

// WrapAuths 测试用：批量包装。
func WrapAuths(list []*auth.Auth) []gateway.Account { return backendcb.WrapAuths(list) }

// UnwrapAuth 测试用：取出 *auth.Auth。
func UnwrapAuth(acc gateway.Account) *auth.Auth { return backendcb.UnwrapAuth(acc) }
