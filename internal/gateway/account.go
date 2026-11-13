// Package gateway 定义多上游账号调度的**公共抽象**。
//
// 设计目标：让 internal/pool 的调度状态机（选号权重、冷却、熔断、降权、模型级
// 避让、在途租约）与具体上游协议解耦。pool 只认这里的接口，不认识
// CodeBuddy 的 auth.Auth，也不认识 Trae 的 trae.Account。
//
// 依赖方向（单向，本包不 import 任何 internal 包）：
//
//	pool  ──依赖──▶  gateway  ◀──依赖──  backend/codebuddy
//	                                      backend/trae
//
// 为什么本包不带凭证：pool 从来不需要 token——它只读 UID/昵称/域。把凭证挡在
// 接口之外，是「两种形态完全不同的凭证能进同一个池」的前提条件。
package gateway

// Account 调度器眼里的账号：只有身份与归属，没有凭证。
//
// 实现方（backend/*/account.go）通常是对自家账号结构的轻量包装，
// 且必须保证这些方法都是**无锁只读**——它们会在 pick 的热路径上被调用。
type Account interface {
	// UID 账号在上游内的唯一标识。
	//
	// 注意：不同 backend 的 UID **可能形态相同**（都是字符串），因此跨 backend
	// 的比较一律使用 QualifiedUID，不要直接比 UID。
	UID() string
	// Nickname 展示名（可能为空）。
	Nickname() string
	// Realm 账号域（"cn" / "global"）。
	//
	// 两套体系都有这个维度，只是判定依据不同：
	//   CodeBuddy: 显式 realm 字段，否则按 Domain 后缀判定
	//   Trae:      按 ChatBase / APITraeBase 的域名判定
	//
	// 契约：**必须返回非空值**（缺省 "cn"）。空串在 gateSpec 里表示「不过滤域」，
	// 静默返回空会让域隔离失效。
	Realm() string
	// Backend 账号所属上游的稳定标识（"codebuddy" / "trae"）。
	Backend() string
	// Enterprise 是否企业版账号。
	//
	// 企业版没有个人成长体系（签到/任务），面板据此隐藏按钮；
	// 不支持该概念的 backend 返回 false。
	Enterprise() bool
}

// QualifiedUID 跨 backend 唯一的账号标识（"backend:uid"）。
//
// 用途：会话粘性绑定值、跨 backend 的去重集合、日志。单 backend 场景下
// 与裸 uid 等价，但**不要**用它做 map 键后再拆开当 uid 用。
func QualifiedUID(a Account) string {
	return a.Backend() + ":" + a.UID()
}
