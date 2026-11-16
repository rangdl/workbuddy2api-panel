// add_account_test.go 钉住「添加账号」弹窗支持两种上游。
//
// 背景：Trae 账号早已在同一个池里，但新增入口只在 Trae 独立页里，右上角
// 「添加账号」只能加 CodeBuddy。这里锁住两条链路（上游选择 + 回调兑换）不被
// 后续改动悄悄丢掉——纯静态资源，用文本断言即可。
package panel

import (
	"os/exec"
	"strings"
	"testing"
)

func TestAddAccountDialogOffersBothUpstreams(t *testing.T) {
	body := string(mustReadPanelFile(t, "index.html"))

	for _, want := range []string{
		`name="addUpstream"`,
		`value="codebuddy"`,
		`value="trae"`,
		`id="addTraeFinish"`,
		`id="addTraeCallback"`,
		`id="btnAddTraeFinish"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("index.html 缺少 %q（添加账号应支持两种上游）", want)
		}
	}
}

func TestAddAccountJsRoutesByUpstream(t *testing.T) {
	js := string(mustReadPanelFile(t, "js/accounts.js"))

	for _, want := range []string{
		"function addUpstream(",
		"function syncAddUpstreamUi(",
		"function startAddTraeLogin(",
		"trae/login/start",
		"trae/login/finish",
	} {
		if !strings.Contains(js, want) {
			t.Errorf("accounts.js 缺少 %q", want)
		}
	}
	// CodeBuddy 的轮询登录必须保留（不能因为合入 Trae 把它弄丢）。
	if !strings.Contains(js, "login/start") || !strings.Contains(js, "pollLogin") {
		t.Error("accounts.js 应保留 CodeBuddy 的轮询登录链路")
	}
}

// TestAddAccountJsSyntax node --check 通过（防止模板字符串/括号写坏）。
func TestAddAccountJsSyntax(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed")
	}
	for _, f := range []string{"js/accounts.js", "js/config.js", "js/core.js"} {
		out, err := exec.Command(node, "--check", f).CombinedOutput()
		if err != nil {
			t.Errorf("%s 语法错误: %v\n%s", f, err, out)
		}
	}
}
