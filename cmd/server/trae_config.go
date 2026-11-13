package main

import (
	"encoding/json"
	"fmt"
	"log"
	"os"

	"github.com/linguo2625469/workbuddy2api-panel/internal/trae"
)

// loadTraeConfig 从 trae.json（与 config.json 同目录）加载 Trae 上游配置。
//
// 返回 nil 表示「未启用 /trae/v1/*」：文件不存在（绝大多数部署只用 CodeBuddy）
// 或配置损坏。配置损坏额外记一条 WARN——不阻断启动，但必须让运维看见，
// 否则表现为「配了却没生效」的静默失败。
//
// 与第三方上游同策略：即使 Enabled=false 也返回配置对象，便于面板热启用。
func loadTraeConfig(configPath string) *trae.Config {
	path := trae.ConfigPath(configPath)
	cfg, err := trae.Load(path)
	if err != nil {
		log.Printf("WARN: [trae] 配置加载失败，/trae/v1/* 不启用: %v", err)
		return nil
	}
	if cfg == nil {
		return nil
	}
	cfg.SaveHook = saveTraeConfigFunc(path)
	if cfg.Enabled {
		cfg.LogSummary()
	}
	return cfg
}

// saveTraeConfigFunc 返回把配置原子写回 trae.json 的钩子。
//
// 用途只有一个：token 续期后 refreshToken 会轮换，必须立刻落盘，
// 否则进程重启后拿旧值续期必然失败（参考实现都强调这一点）。
// 权限与面板写配置一致（0600，文件含凭证明文）。
func saveTraeConfigFunc(path string) func(*trae.Config) error {
	return func(cfg *trae.Config) error {
		b, err := json.MarshalIndent(cfg, "", "  ")
		if err != nil {
			return fmt.Errorf("序列化失败: %w", err)
		}
		b = append(b, '\n')
		tmp := path + ".tmp"
		if err := os.WriteFile(tmp, b, 0o600); err != nil {
			return fmt.Errorf("写入失败: %w", err)
		}
		if err := os.Rename(tmp, path); err != nil {
			return fmt.Errorf("原子替换失败: %w", err)
		}
		return nil
	}
}
