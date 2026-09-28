package main

import (
	"log"

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
	if cfg.Enabled {
		cfg.LogSummary()
	}
	return cfg
}
