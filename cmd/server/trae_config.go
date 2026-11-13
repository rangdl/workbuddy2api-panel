package main

import (
	"log"

	"github.com/linguo2625469/workbuddy2api-panel/internal/trae"
)

// loadTraeConfig 从 trae.json（与 config.json 同目录）加载 Trae 上游配置。
//
// authDir 传主配置的 auth_dir（缺省 ./auths）——Trae 账号与 workbuddy 账号
// **必须在同一个目录**，靠 trae- / workbuddy- 前缀区分。不能按 trae.json
// 的位置推导（配置在 config/，账号在根 auths/，两者不同级）。
//
// 返回 nil 表示「未启用 /trae/v1/*」：文件不存在（绝大多数部署只用 CodeBuddy）
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
		log.Printf("WARN: [trae] 配置加载失败，/trae/v1/* 不启用: %v", err)
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
