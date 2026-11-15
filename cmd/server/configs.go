// configs.go 副配置文件（trae / model）的「不存在则写默认」。
//
// 为什么需要：主配置早就支持首次运行自动生成（config.go 的 WriteDefault），但副配置
// 各自「不存在 → 静默禁用 / 返回 nil」。用户第一次进面板的「上游接入」页只看到空白，
// 必须自己猜到文件名和字段才能建文件。这里统一补齐。
//
// 默认值的选择原则：
//   - 写「能直接看懂结构」的最小样例，而不是全量字段——注释和默认值由代码兜底，
//     文件里留空字符串/缺省键反而更清楚（面板保存时会补齐）；
//   - 生成的 trae.json 一律 enabled=false：写默认值不能顺带开启一个用户没配过的上游。
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
	// trae.json 只放上游协议常量与凭证策略；**没有 enabled 开关**（上游平权，
	// 加了账号即启用），也没有 schedule（已统一到 config.json）。这里写空对象，
	// 字段由代码兜底成内置默认值。
	ensureDefaultFile(filepath.Join(dir, "trae.json"), func() any {
		return map[string]any{}
	})
	// codebuddy.json 同理：只写「用户可能想改」的少量字段，其余由代码兜底。
	//
	// 刻意**不**把全部字段铺出来：自动生成的配置若列着一排 `"user_agent": ""`，
	// 用户会以为那些是"待填项"而实际含义是"用内置默认"，反而制造困惑。
	// 这里只写结构骨架（域维度）与最常调的超时。
	ensureDefaultFile(filepath.Join(dir, "codebuddy.json"), func() any {
		return map[string]any{
			"cn":     map[string]any{},
			"global": map[string]any{"enabled": true},
		}
	})
}
