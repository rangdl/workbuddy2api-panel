package panel

import (
	"net/http"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/trae"
)

// trae_global_enabled_test.go 守「Trae 也像 WorkBuddy 一样能在配置页开关国际版路由」。
//
// 这个能力在后端**早就存在**（trae.Config.Global.Enabled + GlobalEnabled()，
// 被 Account.RegionOf 用于纯 CN 锁定的逃生门），但面板一直没暴露：WorkBuddy
// 区块有「启用国际版路由」勾选框，Trae 区块没有——同一个域概念、同一个上游
// 能力，两个区块不对称，用户只能手改 trae.json。
//
// 三个环节缺一不可：
//  1. GET 回显 global_enabled（不然打开页面永远是未勾选状态）；
//  2. index.html 有控件（不然页面上没有这个框）；
//  3. POST 真能写进去、且**不清空 global 段的其它字段**（最危险的一环）。

// TestTraeGlobalEnabledDefaultsTrue 未配置时回显 true。
//
// trae.GlobalRealm.Enabled 是指针，缺省按 true 处理——因为 Trae 的默认配置
// 是空对象，bool 零值 false 会让升级后所有国际账号静默停用。
func TestTraeGlobalEnabledDefaultsTrue(t *testing.T) {
	dir := t.TempDir()
	p, path := newTraePanel(t, dir)
	cfg := trae.Config{Enabled: true, ChatBase: trae.DefaultChatBase}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	writeTraeTestConfig(t, path, &cfg)

	rec := traeDo(p, http.MethodGet, "/panel/api/trae_config?no_checkin=1", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), `"global_enabled":true`) {
		t.Errorf("未配置 global.enabled 时应回显 true，实际：%s", rec.Body.String())
	}
}

// TestTraeGlobalEnabledRoundTrip 关掉后再读出来必须是 false，且 global 段的
// base 不被清空。
//
// 后者是这次改动最容易引入的真实缺陷：saveTraeConfig 的 next 是从零值构造的，
// 原先逐字段复制，没有 Global 这一项。只加 Enabled 而忘了整段带过，会顺手把
// 用户手改的 global.chat_base / api_base 抹掉。
func TestTraeGlobalEnabledRoundTrip(t *testing.T) {
	dir := t.TempDir()
	p, path := newTraePanel(t, dir)
	off := false
	cfg := trae.Config{
		Enabled:  true,
		ChatBase: trae.DefaultChatBase,
		Global: trae.GlobalRealm{
			TraeRealm: trae.TraeRealm{
				ChatBase: "https://intl.example.test",
				APIBase:  "https://intl-api.example.test",
			},
			Enabled: &off,
		},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	writeTraeTestConfig(t, path, &cfg)

	// ① 回显 false。
	rec := traeDo(p, http.MethodGet, "/panel/api/trae_config?no_checkin=1", "")
	if !strings.Contains(rec.Body.String(), `"global_enabled":false`) {
		t.Fatalf("应回显 false，实际：%s", rec.Body.String())
	}

	// ② 重新打开并保存。
	rec = traeDo(p, http.MethodPost, "/panel/api/trae_config",
		`{"ide_version":"0.1.50","checkin_user_agent":"","global_enabled":true}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("保存失败 status=%d body=%s", rec.Code, rec.Body)
	}

	saved, err := trae.Load(path, "")
	if err != nil {
		t.Fatal(err)
	}
	if !saved.GlobalEnabled() {
		t.Error("保存后 global_enabled 应为 true")
	}
	// global 段的 base 必须原样保留（保存只该改开关，不该毁配置）。
	if saved.Global.ChatBase != "https://intl.example.test" {
		t.Errorf("global.chat_base 被清空了：%q", saved.Global.ChatBase)
	}
	if saved.Global.APIBase != "https://intl-api.example.test" {
		t.Errorf("global.api_base 被清空了：%q", saved.Global.APIBase)
	}
}

// TestTraeGlobalEnabledOmittedKeepsValue 请求里没带 global_enabled 时不许改动它。
//
// 旧前端 / 脚本不会发这个字段。如果后端用 bool 值类型接，零值 false 会让
// 「没提交」被解释成「用户要求关闭」——一次旧客户端保存就把国际账号全停掉。
func TestTraeGlobalEnabledOmittedKeepsValue(t *testing.T) {
	dir := t.TempDir()
	p, path := newTraePanel(t, dir)
	on := true
	cfg := trae.Config{
		Enabled:  true,
		ChatBase: trae.DefaultChatBase,
		Global:   trae.GlobalRealm{Enabled: &on},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	writeTraeTestConfig(t, path, &cfg)

	rec := traeDo(p, http.MethodPost, "/panel/api/trae_config",
		`{"ide_version":"0.1.50","checkin_user_agent":""}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("保存失败 status=%d body=%s", rec.Code, rec.Body)
	}
	saved, err := trae.Load(path, "")
	if err != nil {
		t.Fatal(err)
	}
	if !saved.GlobalEnabled() {
		t.Error("未提交 global_enabled 时不该把开关改成关闭")
	}
}

// TestTraeGlobalEnabledExposedInConfigPage 配置页要有对应控件，且与前端的
// 读取/下发接线一致。
func TestTraeGlobalEnabledExposedInConfigPage(t *testing.T) {
	p := newTestPanel()
	rec := traeDo(p, http.MethodGet, "/panel/", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("/panel/ 返回 %d", rec.Code)
	}
	html := rec.Body.String()

	if !strings.Contains(html, `type="checkbox" id="traeGlobalEnabled"`) {
		t.Error("配置页缺少 Trae 的「启用国际版路由」勾选框（id=traeGlobalEnabled）")
	}
	// 说明文字不许塞进 .switch 内部（横排 flex 会把它挤成窄条）。
	i := strings.Index(html, `id="traeGlobalEnabled"`)
	start := strings.LastIndex(html[:i], `<div class="switch"`)
	end := strings.Index(html[i:], "</div>")
	if start < 0 || end < 0 {
		t.Fatal("找不到该控件所在的 .switch 块")
	}
	if strings.Contains(html[start:i+end], `class="hint"`) {
		t.Error("说明文字应在 .switch 外部另起 <div class=\"hint\">")
	}

	// 前端接线：读取回填 + 保存下发两处都要在。
	rec = traeDo(p, http.MethodGet, "/panel/js/config.js", "")
	js := rec.Body.String()
	if !strings.Contains(js, "$('traeGlobalEnabled').checked = c.global_enabled !== false") {
		t.Error("config.js 应把 global_enabled 回填到勾选框（缺省按 true）")
	}
	if !strings.Contains(js, "out.global_enabled = !!glEl.checked") {
		t.Error("config.js 应把勾选框状态下发给后端")
	}
}
