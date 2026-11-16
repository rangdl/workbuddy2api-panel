package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	backendcb "github.com/linguo2625469/workbuddy2api-panel/internal/backend/codebuddy"
)

// TestMigrateLegacyUpstreamConfig 旧 config.json 的上游字段搬到 codebuddy.json。
//
// 这是「配置单一来源」的关键一步：迁移后旧键必须从 config.json 删除，否则
// 下一个人改旧键却不生效——那是最难排查的一类问题。
func TestMigrateLegacyUpstreamConfig(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")
	cbPath := backendcb.Path(cfgPath)

	legacy := `{
	  "listen": ":8080",
	  "cn": {"chat_base": "https://cn.example"},
	  "global": {"enabled": false, "chat_base": "https://global.example"},
	  "upstream": {"timeout_seconds": 60, "user_agent": "UA/9", "client_version": "1.2.3"},
	  "features": {"sanitize_blacklist_fingerprints": false}
	}`
	if err := os.WriteFile(cfgPath, []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}

	if !migrateLegacyUpstreamConfig(cfgPath, cbPath) {
		t.Fatal("应检测到旧字段并迁移")
	}

	// 新文件内容正确
	cfg, err := backendcb.Load(cbPath)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ChatBaseFor("cn") != "https://cn.example" {
		t.Errorf("cn base=%q want https://cn.example", cfg.ChatBaseFor("cn"))
	}
	if cfg.ChatBaseFor("global") != "https://global.example" {
		t.Errorf("global base=%q want https://global.example", cfg.ChatBaseFor("global"))
	}
	if cfg.Global.Enabled {
		t.Error("global.enabled=false 未迁移")
	}
	if cfg.UserAgent != "UA/9" {
		t.Errorf("user_agent=%q want UA/9", cfg.UserAgent)
	}
	if cfg.ClientVersion != "1.2.3" {
		t.Errorf("client_version=%q want 1.2.3", cfg.ClientVersion)
	}
	if cfg.SanitizeBlacklistFingerprints {
		t.Error("sanitize=false 未迁移")
	}

	// 旧键必须已从 config.json 删除 —— 保证配置单一来源
	raw, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	var root map[string]json.RawMessage
	if err := json.Unmarshal(raw, &root); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"cn", "global", "upstream", "features"} {
		if _, ok := root[k]; ok {
			t.Errorf("config.json 仍残留旧键 %q（违反配置单一来源）", k)
		}
	}
	// 与上游无关的键必须保留
	if _, ok := root["listen"]; !ok {
		t.Error("迁移不应删除无关键 listen")
	}
}

// TestMigrateIsIdempotent 迁移幂等：第二次调用不再动作。
func TestMigrateIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")
	cbPath := backendcb.Path(cfgPath)
	// 用 user_agent 作迁移触发条件：超时已归主配置（不搬），上游专有字段才搬。
	if err := os.WriteFile(cfgPath, []byte(`{"upstream":{"user_agent":"UA/1"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if !migrateLegacyUpstreamConfig(cfgPath, cbPath) {
		t.Fatal("首次应迁移")
	}
	if migrateLegacyUpstreamConfig(cfgPath, cbPath) {
		t.Fatal("第二次不应再迁移（codebuddy.json 已存在）")
	}
}

// TestMigrateSkipsWhenNewFileExists 新文件已存在时不覆盖 —— 它是当前事实来源。
func TestMigrateSkipsWhenNewFileExists(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")
	cbPath := backendcb.Path(cfgPath)
	if err := os.WriteFile(cfgPath, []byte(`{"upstream":{"user_agent":"OLD"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	// 用户已经手工建了 codebuddy.json
	cur := backendcb.Default()
	cur.UserAgent = "NEW"
	if err := backendcb.Save(cbPath, cur); err != nil {
		t.Fatal(err)
	}
	if migrateLegacyUpstreamConfig(cfgPath, cbPath) {
		t.Fatal("新文件已存在时不应迁移（会覆盖用户当前配置）")
	}
	got, err := backendcb.Load(cbPath)
	if err != nil {
		t.Fatal(err)
	}
	if got.UserAgent != "NEW" {
		t.Errorf("user_agent=%q want NEW（不得被旧配置覆盖）", got.UserAgent)
	}
}

// TestCodeBuddyEnvPrefersNewName 新 env 名优先，旧名仍生效。
//
// 旧名（WB2A_USER_AGENT 等）在部署脚本里被广泛使用，直接删掉会让升级后的
// 部署静默失去覆盖——所以保留并告警，给一个迁移窗口。
func TestCodeBuddyEnvPrefersNewName(t *testing.T) {
	t.Setenv("WB2A_USER_AGENT", "OldAgent/1")

	c := backendcb.Default()
	applyCodeBuddyEnv(c)
	if c.UserAgent != "OldAgent/1" {
		t.Errorf("旧 env 名应继续生效，得到 %q", c.UserAgent)
	}

	// 新名覆盖旧名
	t.Setenv("WB2A_CODEBUDDY_USER_AGENT", "NewAgent/2")
	c2 := backendcb.Default()
	applyCodeBuddyEnv(c2)
	if c2.UserAgent != "NewAgent/2" {
		t.Errorf("新 env 名应优先，得到 %q", c2.UserAgent)
	}
}
