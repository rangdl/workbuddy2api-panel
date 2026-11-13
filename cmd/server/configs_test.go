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
	for _, name := range []string{responsesFileName, anthropicFileName, "third_party.json", "trae.json"} {
		p := filepath.Join(base, name)
		raw, err := os.ReadFile(p)
		if err != nil {
			t.Fatalf("%s 未生成: %v", name, err)
		}
		var v map[string]any
		if err := json.Unmarshal(raw, &v); err != nil {
			t.Errorf("%s 不是合法 JSON: %v\n%s", name, err, raw)
		}
		if _, ok := v["enabled"]; !ok {
			t.Errorf("%s 缺少 enabled 字段", name)
		}
	}
}

// TestEnsureAllConfigsDoesNotOverwrite 已有配置绝不能被默认值覆盖。
func TestEnsureAllConfigsDoesNotOverwrite(t *testing.T) {
	dir := t.TempDir()
	base := filepath.Join(dir, "config")
	if err := os.MkdirAll(base, 0o755); err != nil {
		t.Fatal(err)
	}
	const mine = `{"enabled":true,"model_map":{"a":"b"}}`
	p := filepath.Join(base, responsesFileName)
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

	for _, name := range []string{anthropicFileName, "third_party.json", "trae.json"} {
		raw, err := os.ReadFile(filepath.Join(base, name))
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
			t.Errorf("%s 默认 enabled=true，应默认关闭", name)
		}
	}
	// responses 是既有行为：默认启用，不受影响
	raw, _ := os.ReadFile(filepath.Join(base, responsesFileName))
	var r struct {
		Enabled bool `json:"enabled"`
	}
	json.Unmarshal(raw, &r)
	if !r.Enabled {
		t.Error("responses.json 默认应为 enabled=true（保持既有行为）")
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
