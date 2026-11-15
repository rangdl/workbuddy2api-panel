package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// TestEnsureAllConfigs 副配置缺失时必须生成可解析的默认文件。
func TestEnsureAllConfigs(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config", "config.json")

	ensureAllConfigs(cfgPath)

	base := filepath.Join(dir, "config")
	p := filepath.Join(base, "trae.json")
	raw, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("trae.json 未生成: %v", err)
	}
	var v map[string]any
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Errorf("trae.json 不是合法 JSON: %v\n%s", err, raw)
	}
	// 默认内容刻意是**空对象**：trae.json 只放上游协议常量与凭证策略，
	// 字段由代码兜底成内置默认值。绝不能写 enabled:false——
	// 上游平权后没有"启用某个上游"这回事（加了账号即启用），
	// 留一个 false 开关只会让人以为需要手动打开。
	if _, ok := v["enabled"]; ok {
		t.Error("trae.json 不应包含 enabled 字段（上游平权，无启用开关）")
	}
	if _, ok := v["chat_base"]; ok {
		t.Error("trae.json 不应包含 chat_base（上游路径不是配置项）")
	}
	if _, ok := v["schedule"]; ok {
		t.Error("trae.json 不应包含 schedule（签到排程统一在 config.json）")
	}
}

// TestEnsureAllConfigsDoesNotOverwrite 已有配置绝不能被默认值覆盖。
func TestEnsureAllConfigsDoesNotOverwrite(t *testing.T) {
	dir := t.TempDir()
	base := filepath.Join(dir, "config")
	if err := os.MkdirAll(base, 0o755); err != nil {
		t.Fatal(err)
	}
	const mine = `{"enabled":true,"accounts":[]}`
	p := filepath.Join(base, "trae.json")
	if err := os.WriteFile(p, []byte(mine), 0o600); err != nil {
		t.Fatal(err)
	}

	ensureAllConfigs(filepath.Join(base, "config.json"))

	got, _ := os.ReadFile(p)
	if string(got) != mine {
		t.Errorf("已有配置被覆盖:\n got %s\nwant %s", got, mine)
	}
}

// TestEnsureAllConfigsDisabledByDefault 自动生成的接入配置必须是关闭状态：
// 写默认值不能顺带开启用户没配过的上游。
func TestEnsureAllConfigsDisabledByDefault(t *testing.T) {
	dir := t.TempDir()
	base := filepath.Join(dir, "config")
	ensureAllConfigs(filepath.Join(base, "config.json"))

	raw, err := os.ReadFile(filepath.Join(base, "trae.json"))
	if err != nil {
		t.Fatal(err)
	}
	var v struct {
		Enabled bool `json:"enabled"`
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	if v.Enabled {
		t.Error("trae.json 默认 enabled=true，应默认关闭")
	}
}

// TestEnsureDefaultFileIdempotent 重复调用只写一次，第二次不改文件。
func TestEnsureDefaultFileIdempotent(t *testing.T) {
	p := filepath.Join(t.TempDir(), "x.json")
	if !ensureDefaultFile(p, func() any { return map[string]any{"a": 1} }) {
		t.Fatal("首次应写入")
	}
	if err := os.WriteFile(p, []byte(`{"custom":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if ensureDefaultFile(p, func() any { return map[string]any{"a": 1} }) {
		t.Error("第二次不该写入")
	}
	got, _ := os.ReadFile(p)
	if string(got) != `{"custom":true}` {
		t.Errorf("文件被改写: %s", got)
	}
}
