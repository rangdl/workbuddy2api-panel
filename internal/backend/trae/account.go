// Package trae 把 internal/trae（Trae 上游）适配成 gateway.Backend。
package trae

import (
	"strings"

	"github.com/linguo2625469/workbuddy2api-panel/internal/gateway"
	traepkg "github.com/linguo2625469/workbuddy2api-panel/internal/trae"
)

// Name 上游标识。
const Name = "trae"

// Account 把 *trae.Account 适配成 gateway.Account。
type Account struct {
	A *traepkg.Account
	// realm 由账号所属的 base 域名决定（见 realmOfBase）。
	// 未导出：Realm() 方法同名会冲突（Go 不允许字段与方法同名）。
	realm string
}

func (x Account) UID() string      { return x.A.UID }
func (x Account) Nickname() string { return x.A.Name }
func (x Account) Realm() string    { return x.realm }
func (x Account) Backend() string  { return Name }

// Enterprise Trae 没有企业版概念。
func (x Account) Enterprise() bool { return false }

// Unwrap 取出原始账号。
func Unwrap(acc gateway.Account) *traepkg.Account {
	if a, ok := acc.(Account); ok {
		return a.A
	}
	if a, ok := acc.(*Account); ok {
		return a.A
	}
	return nil
}

// realmOfBase 按上游域名判定账号域。
//
// Trae 与 CodeBuddy 一样有 cn / global 两个域，只是判定依据不同：
//   - CodeBuddy 看 auth 文件里的 realm 字段或 domain 后缀；
//   - Trae 看对话/接口 base 的域名——本仓库目前只实现了 CN（mchost.guru /
//     trae.cn / trae.com.cn），其余域名一律视为 global。
//
// 为什么必须返回非空：空串在 gateSpec 里表示「不过滤域」，静默返回空会让
// 域隔离失效，cn 请求可能落到 global 账号上。
func realmOfBase(chatBase, apiBase string) string {
	for _, u := range []string{chatBase, apiBase} {
		u = strings.ToLower(strings.TrimSpace(u))
		if u == "" {
			continue
		}
		// 判定顺序敏感：trae.com.cn 是 CN 域，但它是 trae.com 的超串，
		// 所以必须先判 .com.cn 再判国际版后缀，否则 CN 会被误判成 global。
		switch {
		case strings.Contains(u, "trae.com.cn"),
			strings.Contains(u, "trae.cn"),
			strings.Contains(u, "trae-api-cn"),
			strings.Contains(u, "mchost.guru"):
			return "cn"
		case strings.Contains(u, "trae.ai"), strings.Contains(u, "trae.com"):
			return "global"
		}
	}
	// 无法判定时按 CN 处理（与仓库当前的唯一实现一致）。
	return gateway.DefaultRealm
}

// Wrap 把配置里的账号包装成 gateway.Account。
func Wrap(cfg *traepkg.Config) []gateway.Account {
	if cfg == nil {
		return nil
	}
	realm := realmOfBase(cfg.ChatBase, cfg.APITraeBase)
	out := make([]gateway.Account, 0, len(cfg.Accounts))
	for i := range cfg.Accounts {
		out = append(out, Account{A: &cfg.Accounts[i], realm: realm})
	}
	return out
}
