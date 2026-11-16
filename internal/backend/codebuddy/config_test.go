package codebuddy

import (
	"os"
	"path/filepath"
	"testing"
)

// TestUserAgentFromFile UA 从文件读取；缺省为空（= 用内置三段式默认）。
func TestUserAgentFromFile(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "codebuddy.json")
	if err := os.WriteFile(fp, []byte(`{"user_agent":"WorkBuddy/1.2.3"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(fp)
	if err != nil {
		t.Fatal(err)
	}
	if c.UserAgent != "WorkBuddy/1.2.3" {
		t.Errorf("user_agent=%q want WorkBuddy/1.2.3", c.UserAgent)
	}
	// 缺省为空：空串是「用内置默认」，不是「空 UA」。
	d := Default()
	if d.UserAgent != "" {
		t.Errorf("default user_agent=%q want empty", d.UserAgent)
	}
}

// TestMissingFileReturnsDefault 文件不存在时返回内置默认而不是 nil。
//
// 为什么这条重要：主配置缺失会自动生成，上游配置同理。用户从只有一个 trae.json
// 的版本升级上来时没有 codebuddy.json，不该因此判定「没有 CodeBuddy 上游」——
// 它是默认链路，永远可用。
func TestMissingFileReturnsDefault(t *testing.T) {
	c, err := Load(filepath.Join(t.TempDir(), "nope.json"))
	if err != nil {
		t.Fatalf("缺失文件不应报错: %v", err)
	}
	if c == nil {
		t.Fatal("缺失文件应返回默认配置，得到 nil")
	}
	if c.ChatBaseFor("cn") != DefaultChatBaseCN {
		t.Errorf("missing file 应回落内置 CN base，得到 %q", c.ChatBaseFor("cn"))
	}
	if !c.SanitizeBlacklistFingerprints {
		t.Error("脱敏开关缺省应为 true（安全默认优于放行）")
	}
}

// TestRealmIsolation 两域 base 互不影响 —— 域隔离的核心断言。
//
// 历史事故：域隔离失效会让 global 账号打到 CN 端点，表现为「国际版账号
// 全部报错但凭据正常」。
func TestRealmIsolation(t *testing.T) {
	c := Default()
	c.CN.ChatBase = "https://cn.example"
	c.Global.ChatBase = "https://global.example"
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	if got := c.ChatBaseFor("cn"); got != "https://cn.example" {
		t.Errorf("cn=%q want https://cn.example", got)
	}
	if got := c.ChatBaseFor("global"); got != "https://global.example" {
		t.Errorf("global=%q want https://global.example", got)
	}
	// 只配 cn 时 global 仍用内置默认，不被 cn 污染。
	c2 := Default()
	c2.CN.ChatBase = "https://cn.example"
	if got := c2.ChatBaseFor("global"); got != DefaultChatBaseGlobal {
		t.Errorf("只配 cn 时 global=%q want builtin %q", got, DefaultChatBaseGlobal)
	}
}

// TestBillingFallsBackToChatBase 计费 base 未配时回落同域 chat base。
func TestBillingFallsBackToChatBase(t *testing.T) {
	c := Default()
	c.CN.ChatBase = "https://cn.example"
	if got := c.BillingBaseFor("cn"); got != "https://cn.example" {
		t.Errorf("billing 未配应回落 chat base，得到 %q", got)
	}
	c.CN.BillingBase = "https://billing.example"
	if got := c.BillingBaseFor("cn"); got != "https://billing.example" {
		t.Errorf("显式 billing 应生效，得到 %q", got)
	}
}

// TestValidateRejectsBadBase 非法 base 必须报错，不能静默接受。
func TestValidateRejectsBadBase(t *testing.T) {
	c := Default()
	c.CN.ChatBase = "no-scheme"
	if err := c.Validate(); err == nil {
		t.Fatal("缺少 scheme 的 chat_base 应校验失败")
	}
}

// TestSaveLoadRoundTrip 写盘再读回内容一致（含域维度不丢）。
func TestSaveLoadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "codebuddy.json")
	c := Default()
	c.CN.ChatBase = "https://cn.example"
	c.Global.ChatBase = "https://global.example"
	c.Global.Enabled = false
	c.UserAgent = "UA/1"
	c.SanitizeBlacklistFingerprints = false
	if err := Save(fp, c); err != nil {
		t.Fatal(err)
	}
	got, err := Load(fp)
	if err != nil {
		t.Fatal(err)
	}
	if got.ChatBaseFor("cn") != "https://cn.example" || got.ChatBaseFor("global") != "https://global.example" {
		t.Errorf("域 base 未往返: cn=%q global=%q", got.ChatBaseFor("cn"), got.ChatBaseFor("global"))
	}
	if got.Global.Enabled {
		t.Error("global.enabled=false 未往返")
	}
	if got.UserAgent != "UA/1" {
		t.Errorf("user_agent=%q want UA/1", got.UserAgent)
	}
	if got.SanitizeBlacklistFingerprints {
		t.Error("sanitize=false 未往返")
	}
}
