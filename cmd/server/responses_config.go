package main

import (
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"strconv"

	"github.com/linguo2625469/workbuddy2api-panel/internal/responsesstore"
	"github.com/linguo2625469/workbuddy2api-panel/internal/server"
)

// responsesFileName 与 config.json 同目录的可选配置文件（缺省不存在时用默认值）。
//
// 刻意不并入主 config.json：Responses 支持是本 fork 的增量能力，独立文件可
// 降低与上游 config.go 的合并冲突。
const responsesFileName = "responses.json"

// responsesFileConfig responses.json 的结构。
type responsesFileConfig struct {
	Enabled            *bool             `json:"enabled"`              // 缺省 true
	ModelMap           map[string]string `json:"model_map"`            // Responses 模型名 → 上游模型名
	DefaultModel       string            `json:"default_model"`        // 未命中映射时的回落模型
	MaxCachedResponses int               `json:"max_cached_responses"` // 增量缓存条数，缺省 512
}

// responsesConfigPath 返回 responses.json 的完整路径（与 config.json 同目录）。
func responsesConfigPath(configPath string) string {
	return filepath.Join(filepath.Dir(configPath), responsesFileName)
}

// loadResponsesConfig 从 responses.json + 环境变量构造 Responses 配置。
// 默认启用；enabled=false 时返回 nil（不注册 /v1/responses 路由）。
//
// 环境变量（优先级高于文件）：
//
//	WB2A_RESPONSES_ENABLED      true/false
//	WB2A_RESPONSES_MODEL_MAP    JSON，如 {"gpt-5-codex":"glm-5.2"}
//	WB2A_RESPONSES_DEFAULT_MODEL
//	WB2A_RESPONSES_MAX_CACHE
func loadResponsesConfig(configPath string) *server.ResponsesConfig {
	fc := responsesFileConfig{}
	path := responsesConfigPath(configPath)
	if raw, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(raw, &fc); err != nil {
			log.Printf("WARN: [responses] parse %s: %v", path, err)
		}
	} else if !os.IsNotExist(err) {
		log.Printf("WARN: [responses] read %s: %v", path, err)
	}

	enabled := true
	if fc.Enabled != nil {
		enabled = *fc.Enabled
	}
	if v := os.Getenv("WB2A_RESPONSES_ENABLED"); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			enabled = b
		}
	}
	if !enabled {
		// 仍返回非 nil（Enabled=false）：路由照常注册，handler 运行期返回 404，
		// 这样面板可以热切换启用/禁用，无需重启。
		return &server.ResponsesConfig{Enabled: false}
	}

	modelMap := fc.ModelMap
	if v := os.Getenv("WB2A_RESPONSES_MODEL_MAP"); v != "" {
		m := map[string]string{}
		if err := json.Unmarshal([]byte(v), &m); err != nil {
			log.Printf("WARN: [responses] WB2A_RESPONSES_MODEL_MAP invalid: %v", err)
		} else {
			modelMap = m
		}
	}
	defaultModel := fc.DefaultModel
	if v := os.Getenv("WB2A_RESPONSES_DEFAULT_MODEL"); v != "" {
		defaultModel = v
	}
	maxCache := fc.MaxCachedResponses
	if v := os.Getenv("WB2A_RESPONSES_MAX_CACHE"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			maxCache = n
		}
	}

	return &server.ResponsesConfig{
		Enabled:      true,
		Store:        responsesstore.NewMemoryStore(maxCache),
		ModelMap:     modelMap,
		DefaultModel: defaultModel,
	}
}
