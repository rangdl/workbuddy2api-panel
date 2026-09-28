package main

import (
	"log"

	"github.com/linguo2625469/workbuddy2api-panel/internal/thirdparty"
)

// loadThirdPartyConfig 从 third_party.json（与 config.json 同目录）加载第三方上游配置。
//
// 返回 nil 表示「未启用 /tp/v1/*」：文件不存在（常见：绝大多数部署只用 CodeBuddy）
// 或配置损坏。配置损坏额外记一条 WARN——不阻断启动（CodeBuddy 主链路不受影响），
// 但必须让运维看见，否则表现为「配了却没生效」的静默失败。
//
// 注意返回的是配置对象本身（即使 Enabled=false 也返回）：handler 侧据此区分
// 「未配置」（nil）与「配置了但关闭」（Enabled=false），两者都返回 404 但日志可辨。
// 转发客户端不在此构造——面板热重载只换配置，客户端（连接池）进程内复用。
func loadThirdPartyConfig(configPath string) *thirdparty.Config {
	path := thirdparty.ConfigPath(configPath)
	cfg, err := thirdparty.Load(path)
	if err != nil {
		log.Printf("WARN: [thirdparty] 配置加载失败，/tp/v1/* 不启用: %v", err)
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
