// configs.go 副配置文件（responses / anthropic / third_party / trae / model）的
// 「不存在则写默认」。
//
// 为什么需要：主配置早就支持首次运行自动生成（config.go 的 WriteDefault），但四个
// 副配置各自「不存在 → 静默禁用 / 返回 nil」。用户第一次进面板的「协议接入」「上游接入」
// 页只看到空白，必须自己猜到文件名和字段才能建文件。这里统一补齐。
//
// 默认值的选择原则：
//   - 写「能直接看懂结构」的最小样例，而不是全量字段——注释和默认值由代码兜底，
//     文件里留空字符串/缺省键反而更清楚（面板保存时会补齐）；
//   - 生成的 anthropic.json / third_party.json / trae.json 一律 enabled=false：
//     写默认值不能顺带开启一个用户没配过的上游；
//   - responses.json 是例外（enabled=true），与既有行为一致（它就是默认启用的）。
package main

import (
	"encoding/json"
	"log"
	"os"
	"path/filepath"
)

// defaultFile 一份副配置的默认内容。
type defaultFile struct {
	Name string
	// Build 返回落盘用的对象（经 json.MarshalIndent 序列化）。
	Build func() any
}

// ensureDefaultFile 在文件不存在时写入默认内容（O_EXCL，绝不覆盖已有文件）。
// 返回是否实际写入。
func ensureDefaultFile(path string, build func() any) bool {
	if _, err := os.Stat(path); err == nil {
		return false // 已存在，不动
	}
	out, err := json.MarshalIndent(build(), "", "  ")
	if err != nil {
		log.Printf("WARN: [config] 生成 %s 默认内容失败: %v", path, err)
		return false
	}
	out = append(out, '\n')
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		log.Printf("WARN: [config] 创建目录失败 %s: %v", filepath.Dir(path), err)
		return false
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		if !os.IsExist(err) {
			log.Printf("WARN: [config] 写入 %s 失败: %v", path, err)
		}
		return false
	}
	defer f.Close()
	if _, err := f.Write(out); err != nil {
		log.Printf("WARN: [config] 写入 %s 失败: %v", path, err)
		return false
	}
	log.Printf("[config] %s 不存在，已生成默认配置", path)
	return true
}

// ensureAllConfigs 为全部副配置补齐默认文件。
// 必须在迁移之后、各 load*Config 之前调用。
func ensureAllConfigs(configPath string) {
	dir := filepath.Dir(configPath)
	ensureDefaultFile(filepath.Join(dir, responsesFileName), func() any {
		return map[string]any{
			"enabled": true,
			"model_map": map[string]string{
				"gpt-5-codex": "glm-5.2",
			},
			"default_model":        "",
			"max_cached_responses": 512,
		}
	})
	ensureDefaultFile(filepath.Join(dir, anthropicFileName), func() any {
		return map[string]any{
			"enabled":       false,
			"model_map":     map[string]string{},
			"default_model": "",
		}
	})
	ensureDefaultFile(filepath.Join(dir, "third_party.json"), func() any {
		// 带一条注释性样例（enabled=false，不会真的启用），用户改成自己的地址即可。
		return map[string]any{
			"enabled": false,
			"providers": []any{
				map[string]any{
					"name":     "example",
					"protocol": "openai",
					"base_url": "https://api.example.com/v1",
					"api_key":  "",
					"models":   []string{},
				},
			},
		}
	})
	ensureDefaultFile(filepath.Join(dir, "trae.json"), func() any {
		return map[string]any{
			"enabled":  false,
			"accounts": []any{},
		}
	})
}
