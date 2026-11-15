package main

import (
	"encoding/json"
	"log"
	"os"
	"strings"

	backendcb "github.com/linguo2625469/workbuddy2api-panel/internal/backend/codebuddy"
)

// codebuddy_config.go 加载 CodeBuddy 上游配置（codebuddy.json）。
//
// 为什么上游配置要独立成文件：见 internal/backend/codebuddy/config.go 的包注释。
// 简言之——主配置只放与上游无关的东西，加/删上游只动一个文件，不留死字段。

// loadCodeBuddyConfig 读取 codebuddy.json，缺失时用内置默认。
//
// 返回**永远非 nil**：上游平权后不存在「没有 CodeBuddy 上游」（它是默认链路），
// 配置缺失只意味着"全用内置默认"。
func loadCodeBuddyConfig(configPath string) *backendcb.Config {
	path := backendcb.Path(configPath)

	// 迁移已由 main 在 ensureAllConfigs 之前完成（必须在默认文件生成前跑，
	// 否则目标文件已存在，迁移会被跳过而导致旧配置静默丢失）。这里只读。

	cfg, err := backendcb.Load(path)
	if err != nil {
		log.Printf("WARN: [codebuddy] 配置加载失败，使用内置默认: %v", err)
		cfg = backendcb.Default()
	}
	applyCodeBuddyEnv(cfg)
	return cfg
}

// applyCodeBuddyEnv 用环境变量覆盖 CodeBuddy 上游配置。
//
// 命名：WB2A_CODEBUDDY_<FIELD>。前缀是「项目名_上游名」，与主配置的
// WB2A_<FIELD> 形成层级——加一个新上游就是 WB2A_TRAE_*，不会与主配置抢名字，
// 也不会和上一个上游的变量混淆。
//
// 旧名（WB2A_USER_AGENT 等）**继续生效**并记一条 WARN：这些变量在部署脚本和
// docker-compose 里被广泛使用，直接删掉会让升级后的部署静默失去覆盖——那是
// 比"名字不好看"严重得多的问题。给一个迁移窗口，后续版本再移除。
func applyCodeBuddyEnv(c *backendcb.Config) {
	// 新名优先；未设置时回落旧名（并提示改名）。
	str := func(newName, oldName string) string {
		if v := os.Getenv(newName); v != "" {
			return v
		}
		if oldName == "" {
			return ""
		}
		v := os.Getenv(oldName)
		if v != "" {
			log.Printf("WARN: [codebuddy] 环境变量 %s 已更名为 %s，旧名将在后续版本移除", oldName, newName)
		}
		return v
	}
	if v := str("WB2A_CODEBUDDY_USER_AGENT", "WB2A_USER_AGENT"); v != "" {
		c.UserAgent = v
	}
	if v := str("WB2A_CODEBUDDY_CLIENT_VERSION", "WB2A_CLIENT_VERSION"); v != "" {
		c.ClientVersion = v
	}
	if v := str("WB2A_CODEBUDDY_CLI_VERSION", "WB2A_CLI_VERSION"); v != "" {
		c.CliVersion = v
	}
	if v := str("WB2A_CODEBUDDY_CLIENT_NAME", "WB2A_CLIENT_NAME"); v != "" {
		c.ClientName = v
	}
	if v := str("WB2A_CODEBUDDY_DEVICE_TOKEN", "WB2A_DEVICE_TOKEN"); v != "" {
		c.DeviceToken = v
	}
	if v := str("WB2A_CODEBUDDY_DEVICE_TOKEN_FILE", "WB2A_DEVICE_TOKEN_FILE"); v != "" {
		c.DeviceTokenFile = v
	}
	if v := str("WB2A_CODEBUDDY_PASSTHROUGH_IP", "WB2A_PASSTHROUGH_IP"); v != "" {
		c.PassthroughIP = v == "1" || strings.EqualFold(v, "true")
	}
	if v := str("WB2A_CODEBUDDY_SANITIZE_FINGERPRINTS", "WB2A_SANITIZE_FINGERPRINTS"); v != "" {
		c.SanitizeBlacklistFingerprints = !(v == "0" || strings.EqualFold(v, "false"))
	}
	if v := str("WB2A_CODEBUDDY_GLOBAL_ENABLED", "WB2A_GLOBAL_ENABLED"); v != "" {
		c.Global.Enabled = !(v == "0" || strings.EqualFold(v, "false"))
	}
	c.Normalize()
}

// migrateLegacyUpstreamConfig 把旧 config.json 里的上游字段搬到 codebuddy.json。
//
// 只在 codebuddy.json **不存在**时执行：已存在的文件是用户当前的事实来源，
// 不能再拿旧键覆盖它。迁移成功后从 config.json 删掉这些键——留着会让
// 「配置单一来源」失效，下一个人改旧键却不生效，是最难排查的一类问题。
//
// 幂等：迁移完成后 codebuddy.json 存在，本函数直接返回 false。
func migrateLegacyUpstreamConfig(configPath, cbPath string) bool {
	if _, err := os.Stat(cbPath); err == nil {
		return false // 已迁移过（或用户手工建了），不动
	}
	raw, err := os.ReadFile(configPath)
	if err != nil {
		return false
	}
	var root map[string]json.RawMessage
	if err := json.Unmarshal(raw, &root); err != nil {
		return false
	}

	// 旧结构 → 新结构。键名保持一致，避免迁移时引入映射错误。
	cfg := backendcb.Default()
	moved := false

	if section, ok := root["cn"]; ok {
		var cn struct {
			ChatBase string `json:"chat_base"`
		}
		if json.Unmarshal(section, &cn) == nil && cn.ChatBase != "" {
			cfg.CN.ChatBase = cn.ChatBase
			moved = true
		}
	}
	if section, ok := root["global"]; ok {
		var gl struct {
			Enabled     *bool  `json:"enabled"`
			ChatBase    string `json:"chat_base"`
			BillingBase string `json:"billing_base"`
		}
		if json.Unmarshal(section, &gl) == nil {
			if gl.Enabled != nil && !*gl.Enabled {
				cfg.Global.Enabled = false
				moved = true
			}
			if gl.ChatBase != "" {
				cfg.Global.ChatBase = gl.ChatBase
				moved = true
			}
			if gl.BillingBase != "" {
				cfg.Global.BillingBase = gl.BillingBase
				moved = true
			}
		}
	}
	if section, ok := root["upstream"]; ok {
		var up struct {
			UserAgent       string `json:"user_agent"`
			ClientVersion   string `json:"client_version"`
			CliVersion      string `json:"cli_version"`
			ClientName      string `json:"client_name"`
			DeviceToken     string `json:"device_token"`
			DeviceTokenFile string `json:"device_token_file"`
			PassthroughIP   bool   `json:"passthrough_ip"`
		}
		if json.Unmarshal(section, &up) == nil {
			for _, f := range []struct {
				dst *string
				src string
			}{
				{&cfg.UserAgent, up.UserAgent},
				{&cfg.ClientVersion, up.ClientVersion},
				{&cfg.CliVersion, up.CliVersion},
				{&cfg.ClientName, up.ClientName},
				{&cfg.DeviceToken, up.DeviceToken},
				{&cfg.DeviceTokenFile, up.DeviceTokenFile},
			} {
				if f.src != "" {
					*f.dst = f.src
					moved = true
				}
			}
			if up.PassthroughIP {
				cfg.PassthroughIP = true
				moved = true
			}
		}
	}
	if section, ok := root["features"]; ok {
		var feat struct {
			Sanitize *bool `json:"sanitize_blacklist_fingerprints"`
		}
		if json.Unmarshal(section, &feat) == nil && feat.Sanitize != nil {
			cfg.SanitizeBlacklistFingerprints = *feat.Sanitize
			moved = true
		}
	}
	if !moved {
		return false // 旧配置里没有上游字段，无需迁移
	}

	if err := backendcb.Save(cbPath, cfg); err != nil {
		log.Printf("WARN: [codebuddy] 迁移写盘失败（保持旧配置可用）: %v", err)
		return false
	}
	// 写成功后才删旧键：万一 Save 失败，config.json 仍是完整可用的。
	for _, k := range []string{"cn", "global", "upstream", "features"} {
		delete(root, k)
	}
	out, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		return true // 已经迁移成功，回写失败只影响"删旧键"，不影响可用性
	}
	if err := os.WriteFile(configPath, append(out, '\n'), 0o600); err != nil {
		log.Printf("WARN: [codebuddy] 旧键删除失败（配置仍可用，但存在两处来源）: %v", err)
	}
	return true
}
