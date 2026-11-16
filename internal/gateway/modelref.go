package gateway

import "strings"

// modelref.go 模型名前缀协议。
//
// 规则只有一条：**显式 = 锁定，缺省 = 自由**。
//
//	trae:cn:glm-5.3   backend 锁定 trae,  realm 锁定 cn    → 精确指定
//	trae:glm-5.3      backend 锁定 trae,  realm 自由       → 可跨域
//	cn:glm-5.3        backend 自由,       realm 锁定 cn    → 可跨上游
//	glm-5.3           backend 自由,       realm 自由       → 完全自由（统一调度）
//
// 前缀的唯一用途是「方便地钉住某个上游某个域」，不带前缀就是最自由的调度。
// 前端列表会同时输出显式形态（codebuddy:cn:x / trae:cn:x）与自由形态（cn:x）。

// ModelRef 解析后的模型引用。
type ModelRef struct {
	// Backend 指定的上游标识；空 = 不限（参与跨上游调度）。
	Backend string
	// Realm 账号域，恒非空（缺省 "cn"）。
	//
	// 与 Backend 不同，这个字段**总是有值**：域的过滤是默认行为，而不是
	// 可选项。是否需要跨域回退只看 RealmExplicit。
	Realm string
	// Bare 剥离全部前缀后的模型名（上游 wire 名，未经 backend 的模型映射）。
	Bare string

	// BackendExplicit / RealmExplicit 该维度是否由调用方显式写出。
	//
	// 显式写出的维度在调度中**永不回退**：用户明确指定了 trae，
	// 就不该在 Trae 无号时悄悄换成 CodeBuddy——那会让「指定上游」
	// 这个动作失去意义，且污染计费口径。
	BackendExplicit bool
	RealmExplicit   bool
}

// DefaultRealm 缺省域。
const DefaultRealm = "cn"

// ParseModelRef 解析模型名，backends 为已注册的上游标识（含别名）。
//
// 解析顺序：从左到右最多剥两段前缀，backend 与 realm 的先后不限
// （"trae:cn:x" 与 "cn:trae:x" 等价）。任一段不匹配已知枚举即停止，
// 剩余部分原样作为 Bare——所以 "claude-3:5" 这类含冒号的裸模型名不会被误解析
// （"claude-3" 不是已知 backend）。
func ParseModelRef(model string, backends []string) ModelRef {
	ref := ModelRef{Realm: DefaultRealm}
	rest := model
	for i := 0; i < 2; i++ {
		idx := strings.IndexByte(rest, ':')
		if idx < 0 {
			break
		}
		prefix := rest[:idx]
		if !ref.BackendExplicit && containsFold(backends, prefix) {
			ref.Backend = strings.ToLower(prefix)
			ref.BackendExplicit = true
			rest = rest[idx+1:]
			continue
		}
		if !ref.RealmExplicit && (prefix == "cn" || prefix == "global") {
			ref.Realm = prefix
			ref.RealmExplicit = true
			rest = rest[idx+1:]
			continue
		}
		break
	}
	ref.Bare = rest
	return ref
}

// String 还原为规范形态（日志/面板回显用）。
func (r ModelRef) String() string {
	var b strings.Builder
	if r.BackendExplicit {
		b.WriteString(r.Backend)
		b.WriteByte(':')
	}
	if r.RealmExplicit {
		b.WriteString(r.Realm)
		b.WriteByte(':')
	}
	b.WriteString(r.Bare)
	return b.String()
}

// containsFold 大小写不敏感匹配。上游标识习惯小写，但客户端可能写 "Trae:"。
func containsFold(list []string, s string) bool {
	for _, v := range list {
		if strings.EqualFold(v, s) {
			return true
		}
	}
	return false
}
