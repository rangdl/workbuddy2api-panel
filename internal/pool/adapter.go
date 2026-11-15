package pool

// adapter.go 原先是 *auth.Auth ⇄ gateway.Account 的适配器，里面直接持有
// *auth.Auth——那让 internal/pool 事实上 import 了 internal/auth。
//
// 现已拆分：
//   - 上游无关的占位账号与它所属的兼容常量 → bare.go
//   - CodeBuddy 的适配器与 UnwrapAuth / WrapAuth → backend/codebuddy/placeholder.go
//
// 本文件保留为空壳只为避免历史引用失效，可以安全删除。
