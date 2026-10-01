package main

import (
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"strconv"

	"github.com/linguo2625469/workbuddy2api-panel/internal/server"
)

// anthropicFileName Anthropic Messages 端点配置文件名（与 config.json 同目录）。
const anthropicFileName = "anthropic.json"

// anthropicFileConfig anthropic.json 的结构。
type anthropicFileConfig struct {
	Enabled      *bool             `json:"enabled"`
	ModelMap     map[string]string `json:"model_map"`
	DefaultModel string            `json:"default_model"`
}

// anthropicConfigPath 返回 anthropic.json 的完整路径（与 config.json 同目录）。
func anthropicConfigPath(configPath string) string {
	return filepath.Join(filepath.Dir(configPath), anthropicFileName)
}

// loadAnthropicConfig 从 anthropic.json + 环境变量构造 Anthropic 配置。
// 文件不存在时默认禁用（与 responses.json 的「默认启用」相反：CC 接入是新增
// 能力，不改变既有部署的行为面；要用就显式建文件或设环境变量）。
//
// 环境变量（优先级高于文件）：
//
//	WB2A_ANTHROPIC_ENABLED      true/false
//	WB2A_ANTHROPIC_MODEL_MAP    JSON，如 {"claude-sonnet-4-5":"glm-5.2"}
//	WB2A_ANTHROPIC_DEFAULT_MODEL
func loadAnthropicConfig(configPath string) *server.AnthropicConfig {
	fc := anthropicFileConfig{}
	path := anthropicConfigPath(configPath)
	if raw, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(raw, &fc); err != nil {
			log.Printf("WARN: [anthropic] parse %s: %v", path, err)
		}
	} else if !os.IsNotExist(err) {
		log.Printf("WARN: [anthropic] read %s: %v", path, err)
	}

	enabled := false
	if fc.Enabled != nil {
		enabled = *fc.Enabled
	}
	if v := os.Getenv("WB2A_ANTHROPIC_ENABLED"); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			enabled = b
		}
	}

	cfg := &server.AnthropicConfig{
		Enabled:      enabled,
		ModelMap:     fc.ModelMap,
		DefaultModel: fc.DefaultModel,
	}
	if m := os.Getenv("WB2A_ANTHROPIC_MODEL_MAP"); m != "" {
		parsed := map[string]string{}
		if json.Unmarshal([]byte(m), &parsed) == nil && len(parsed) > 0 {
			cfg.ModelMap = parsed
		}
	}
	if v := os.Getenv("WB2A_ANTHROPIC_DEFAULT_MODEL"); v != "" {
		cfg.DefaultModel = v
	}

	if cfg.Enabled {
		if path != "" {
			log.Printf("[anthropic] /v1/messages 已启用 (config=%s, model_map=%d 项, default=%q)",
				path, len(cfg.ModelMap), cfg.DefaultModel)
		} else {
			log.Printf("[anthropic] /v1/messages 已启用 (env 配置)")
		}
	}
	return cfg
}
