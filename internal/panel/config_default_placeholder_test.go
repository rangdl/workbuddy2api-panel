package panel

import (
	"os"
	"strings"
	"testing"

	cbbackend "github.com/linguo2625469/workbuddy2api-panel/internal/backend/codebuddy"
	"github.com/linguo2625469/workbuddy2api-panel/internal/trae"
)

// config_default_placeholder_test.go 守「配置页输入框的 placeholder 显示真实默认值」。
//
// 原先这些框一律写「留空 = 内置默认」，用户想知道实际会发什么出去必须翻源码。
// 现在 placeholder 直接给默认值，于是**它必须与后端常量同步**——常量改了而
// placeholder 没改，就成了比「留空 = 内置默认」更糟的谎：它看起来像确切的答案。
//
// 这里不做字符串对源码的弱断言，而是直接引用默认常量来比对，改常量时测试会
// 立刻失败并指出哪一处文案过期了。

// readIndexHTML 读面板页面。
func readIndexHTML(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile("index.html")
	if err != nil {
		t.Fatalf("读 index.html 失败: %v", err)
	}
	return string(b)
}

// placeholderOf 取某个 id 输入框的 placeholder 原值。
func placeholderOf(t *testing.T, html, id string) string {
	t.Helper()
	// 定位 id="X" 之后最近的 placeholder="..."（属性顺序不固定，逐个找）。
	i := strings.Index(html, `id="`+id+`"`)
	if i < 0 {
		t.Fatalf("index.html 里找不到 id=%q 的输入框", id)
	}
	rest := html[i:]
	end := strings.Index(rest, ">")
	if end < 0 {
		t.Fatalf("id=%q 的标签没有闭合", id)
	}
	tag := rest[:end]
	p := strings.Index(tag, `placeholder="`)
	if p < 0 {
		return ""
	}
	tag = tag[p+len(`placeholder="`):]
	q := strings.Index(tag, `"`)
	if q < 0 {
		return ""
	}
	return tag[:q]
}

// TestCodeBuddyPlaceholdersShowDefaults CodeBuddy 四个可覆盖项的 placeholder
// 必须等于后端实际会用的默认值。
func TestCodeBuddyPlaceholdersShowDefaults(t *testing.T) {
	html := readIndexHTML(t)
	cases := []struct {
		id   string
		want string
		why  string
	}{
		{"cbClientVersion", cbbackend.DefaultClientVersion, "出站 UA 的 WorkBuddy 段"},
		{"cbCliVersion", cbbackend.DefaultCliVersion, "出站 UA 的 CLI 段"},
		{"cbChatBase", cbbackend.DefaultChatBaseCN, "国内版对话 base"},
		{"cbGlobalChatBase", cbbackend.DefaultChatBaseGlobal, "国际版对话 base"},
	}
	for _, c := range cases {
		got := placeholderOf(t, html, c.id)
		if got != c.want {
			t.Errorf("%s（%s）的 placeholder = %q，want 默认值 %q\n"+
				"改了默认常量就要同步面板文案，否则页面在撒谎", c.id, c.why, got, c.want)
		}
	}

	// User-Agent 是拼装值，placeholder 里应含三段式的两个版本号（不能只写"内置默认"）。
	ua := placeholderOf(t, html, "cbUserAgent")
	for _, seg := range []string{
		"WorkBuddy/" + cbbackend.DefaultClientVersion,
		"CLI/" + cbbackend.DefaultCliVersion,
	} {
		if !strings.Contains(ua, seg) {
			t.Errorf("cbUserAgent 的 placeholder 应含 %q（实际 %q）", seg, ua)
		}
	}
	// global 账号的平台段不同，文案里要提一句，否则用户以为只有一种形态。
	if !strings.Contains(ua, "WorkBuddy AI") {
		t.Errorf("cbUserAgent 的 placeholder 应说明 Global 账号平台段为 WorkBuddy AI（实际 %q）", ua)
	}
}

// TestTraePlaceholdersShowDefaults Trae 两项同理。
func TestTraePlaceholdersShowDefaults(t *testing.T) {
	html := readIndexHTML(t)
	// 签到 UA 的默认值是 trae 包的内部常量（credits.go 的 checkinUserAgent），
	// 未导出，这里锁字面值；改动它时两处会一起被要求更新。
	if got := placeholderOf(t, html, "traeUserAgent"); got != "Trae/0.1.52" {
		t.Errorf("traeUserAgent 的 placeholder = %q，want \"Trae/0.1.52\"", got)
	}
	if got := placeholderOf(t, html, "traeIdeVersion"); got != trae.DefaultIDEVersion {
		t.Errorf("traeIdeVersion 的 placeholder = %q，want 默认值 %q", got, trae.DefaultIDEVersion)
	}
}

// TestNoVagueDefaultPlaceholder 不许再退回「留空 = 内置默认」这类不给值的写法。
//
// 唯一允许保留这种形态的是 api_key（留空 = 不鉴权）：它的"默认"是一种行为
// 开关，不是一个值，写不出具体字符串。
func TestNoVagueDefaultPlaceholder(t *testing.T) {
	html := readIndexHTML(t)
	for _, bad := range []string{
		`placeholder="留空 = 内置默认"`,
		`placeholder="留空 = 内置三段式默认"`,
		`placeholder="留空 = 内置默认提示词"`,
	} {
		if strings.Contains(html, bad) {
			t.Errorf("仍有不给出实际值的占位文案 %s；应写成真实默认值", bad)
		}
	}
}
