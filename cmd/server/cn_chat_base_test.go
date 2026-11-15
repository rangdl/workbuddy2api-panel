package main

import (
	"testing"

	backendcb "github.com/linguo2625469/workbuddy2api-panel/internal/backend/codebuddy"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// CodeBuddy 的域 base 已搬进 codebuddy.json（上游配置独立成文件）。
// 这几条测试锁的是同一件事：**空值回落内置默认、有值才覆盖、两域互不影响**。
// 搬迁的同时把断言从 config.CN 改到 codebuddy.Config 的域方法上——
// 行为不变，只是事实来源换了文件。

// 空配置（无 cn 段）必须保留内置 CN base，零回归。
func TestCNChatBaseDefaultKeepsBuiltin(t *testing.T) {
	cfg := backendcb.Default()
	if got := cfg.ChatBaseFor("cn"); got != backendcb.DefaultChatBaseCN {
		t.Fatalf("ChatBaseFor(cn)=%q want builtin %q", got, backendcb.DefaultChatBaseCN)
	}
}

func TestCNChatBaseOverride(t *testing.T) {
	cfg := backendcb.Default()
	cfg.CN.ChatBase = "https://www.workbuddy.cn"
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	if got := cfg.ChatBaseFor("cn"); got != "https://www.workbuddy.cn" {
		t.Fatalf("ChatBaseFor(cn)=%q want override", got)
	}
	// global 不受 cn 覆盖影响：这是域隔离的核心断言。
	if got := cfg.ChatBaseFor("global"); got != backendcb.DefaultChatBaseGlobal {
		t.Fatalf("ChatBaseFor(global)=%q want untouched builtin", got)
	}
}

// 装配后 up.ChatBaseCN 必须拿到域 base；空值时不覆盖 client 的内置默认。
func TestCNChatBaseAppliedToClient(t *testing.T) {
	up := upstream.New()
	builtin := up.ChatBaseCN

	empty := backendcb.Default()
	if b := empty.ChatBaseFor("cn"); b != builtin {
		up.ChatBaseCN = b
	}
	if up.ChatBaseCN != builtin {
		t.Fatalf("ChatBaseCN=%q want builtin %q（空值不应覆盖）", up.ChatBaseCN, builtin)
	}

	over := backendcb.Default()
	over.CN.ChatBase = "https://www.workbuddy.cn"
	if b := over.ChatBaseFor("cn"); b != backendcb.DefaultChatBaseCN {
		up.ChatBaseCN = b
	}
	if up.ChatBaseCN != "https://www.workbuddy.cn" {
		t.Fatalf("ChatBaseCN=%q want override", up.ChatBaseCN)
	}
}

// cn.chat_base 在装配期写进 client，之后不再读取——保存后须提示重启。
//
// 该字段已迁到 codebuddy.json，重启提示由 /panel/api/codebuddy_config 返回
// （见 internal/panel/codebuddy_config.go）。主配置的重启清单**不该**再列它：
// 列一个 config.json 里不存在的字段会让用户去改错地方。
func TestCNChatBaseNotInMainRestartList(t *testing.T) {
	for _, f := range restartRequiredFields(Default()) {
		if f == "cn.chat_base" || f == "upstream.user_agent" {
			t.Errorf("%q 已迁到 codebuddy.json，不应出现在主配置的重启清单里", f)
		}
	}
}
