package main

import (
	"log"

	backendtrae "github.com/linguo2625469/workbuddy2api-panel/internal/backend/trae"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/trae"
)

// loadTraeConfig 从 trae.json（与 config.json 同目录）加载 Trae 上游配置。
//
// authDir 传主配置的 auth_dir（缺省 ./auths）——Trae 账号与 workbuddy 账号
// **必须在同一个目录**，靠 trae- / workbuddy- 前缀区分。不能按 trae.json
// 的位置推导（配置在 config/，账号在根 auths/，两者不同级）。
//
// 返回 nil 表示「未启用 Trae 上游」：文件不存在（绝大多数部署只用 CodeBuddy）
// 或配置损坏。配置损坏额外记一条 WARN——不阻断启动，但必须让运维看见，
// 否则表现为「配了却没生效」的静默失败。
//
// 与第三方上游同策略：即使 Enabled=false 也返回配置对象，便于面板热启用。
func loadTraeConfig(configPath, authDir string) *trae.Config {
	path := trae.ConfigPath(configPath)
	// 旧版把账号内嵌在 trae.json 里，先拆成一账号一文件（幂等，只在真有时才写盘）。
	if n, err := trae.MigrateLegacyAccounts(path, authDir); err != nil {
		log.Printf("WARN: [trae] 旧版内嵌账号迁移未完全成功: %v", err)
	} else if n > 0 {
		log.Printf("[trae] 已把 %d 个内嵌账号迁移到 %s/trae-*.json", n, authDir)
	}
	cfg, err := trae.Load(path, authDir)
	if err != nil {
		log.Printf("WARN: [trae] 配置加载失败，Trae 上游不参与调度: %v", err)
		return nil
	}
	if cfg == nil {
		return nil
	}
	cfg.SaveAccount = saveTraeAccountFunc(cfg.AuthDir)
	if cfg.Enabled {
		cfg.LogSummary()
	}
	return cfg
}

// saveTraeAccountFunc 返回把单个账号原子写回 auths/trae-<uid>.json 的钩子。
//
// 用途只有一个：token 续期后 refreshToken 会轮换，必须立刻落盘，
// 否则进程重启后拿旧值续期必然失败（参考实现都强调这一点）。
// 一账号一文件，多个账号并发续期互不覆盖。
func saveTraeAccountFunc(dir string) func(*trae.Account) error {
	return func(a *trae.Account) error {
		return trae.SaveAccount(dir, a)
	}
}

// syncTraeAccountsToPool 把 Trae 账号合进同一个调度池。
//
// 池里同时装着 CodeBuddy 与 Trae 账号，选号按 backend 维度区分（gateSpec.backend）。
// 这里是「合池」唯一的装配点：Trae 侧账号变化（登录/删除/禁用）后调用即可。
//
// 只增不删的取舍：池的 SyncToDir 是「本次扫描到的全集」，混两个上游时不能直接
// 用它（会把另一个上游的账号当成"已删除"）。所以这里用 Add 逐个加入——
// 状态保留由 pool.upsertLocked 保证，禁用态由 trae.json 的 Disabled 字段驱动。
func syncTraeAccountsToPool(p *pool.Pool, cfg *trae.Config) int {
	if p == nil || cfg == nil || !cfg.Enabled {
		return 0
	}
	accs := backendtrae.Wrap(cfg)
	for _, a := range accs {
		p.Add(a)
	}
	return len(accs)
}

// removeStaleTraeAccounts 把已从 trae.json 删除的账号摘出池。
func removeStaleTraeAccounts(p *pool.Pool, cfg *trae.Config) {
	if p == nil {
		return
	}
	live := map[string]bool{}
	if cfg != nil && cfg.Enabled {
		for _, a := range backendtrae.Wrap(cfg) {
			live[a.UID()] = true
		}
	}
	for _, st := range p.List() {
		// 只处理 Trae 的账号：CodeBuddy 的由 SyncToDir 管。
		if st.Backend != backendtrae.Name {
			continue
		}
		if !live[st.UID] {
			p.Remove(st.UID)
		}
	}
}
