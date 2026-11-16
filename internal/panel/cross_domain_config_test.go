package panel

import (
	"net/http/httptest"
	"strings"
	"testing"
)

// cross_domain_config_test.go 守「pool.cross_domain_fallback 在配置页可配」。
//
// 为什么要守：这个开关此前只能改 config.json，面板上没有入口。它的语义是
// 「裸模型名在首选域（cn）选不到号时改用另一域」——正是「避免业务中断」的
// 手段，用户找不到它就只能读源码。三个环节缺一不可，缺任何一个都会让用户
// 在页面上「看不到」或「保存了没生效」：
//
//  1. index.html 有对应的表单控件（不然页面上没有这个框）；
//  2. js/config.js 的 CFG_MAP 有映射（不然读取不回填、保存不下发——最隐蔽，
//     页面看着正常，保存却不生效）；
//  3. 控件名与 CFG_MAP 的键**完全一致**（拼错一个字母就是上面那种假成功）。

const crossDomainField = "cross_domain_fallback"

// TestCrossDomainFallbackExposedInConfigPage 配置页必须暴露该开关。
func TestCrossDomainFallbackExposedInConfigPage(t *testing.T) {
	p := newTestPanel()

	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest("GET", "/panel/", nil))
	if rec.Code != 200 {
		t.Fatalf("/panel/ 返回 %d", rec.Code)
	}
	html := rec.Body.String()

	// ① 表单控件存在，且是 checkbox（与相邻的「快过期积分优先」同形）。
	want := `name="` + crossDomainField + `"`
	if !strings.Contains(html, want) {
		t.Fatalf("配置页缺少 %s 控件（找不到 %s）", crossDomainField, want)
	}
	if !strings.Contains(html, `type="checkbox" name="`+crossDomainField+`"`) {
		t.Errorf("%s 应是 checkbox（与相邻布尔开关同形）", crossDomainField)
	}

	// ② 说明文案必须解释「显式前缀永不回退」——那是这个开关最容易被误解的地方。
	//    用户以为开了它就「处处跨域」，实际 cn:/global: 前缀仍然钉死。
	if !strings.Contains(html, "永不回退") {
		t.Error("说明文案应写明「显式 cn:/global: 前缀永不回退」")
	}
}

// TestCrossDomainFallbackHintOutsideSwitch 说明文字必须在 .switch **外面**。
//
// .switch 是 `justify-content: space-between` 的横排 flex（标签在左、开关在右）。
// 把长说明塞进它内部，文字会被挤成夹在两者之间的窄条：截图里那行「裸模型名
// 跨域回退 开启后，请求 model 不带…」就是这个词条的真实观感。
// 项目里其它带说明的开关一律是「.switch 结束后紧跟一个 <div class="hint">」，
// 这里锁住同形，避免以后又把 span 塞回去。
func TestCrossDomainFallbackHintOutsideSwitch(t *testing.T) {
	p := newTestPanel()
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest("GET", "/panel/", nil))
	html := rec.Body.String()

	i := strings.Index(html, `name="`+crossDomainField+`"`)
	if i < 0 {
		t.Fatalf("找不到 %s 控件", crossDomainField)
	}
	// 取控件所在的那个 .switch 块（到它的闭合 </div> 为止）。
	start := strings.LastIndex(html[:i], `<div class="switch"`)
	if start < 0 {
		t.Fatal("控件不在 .switch 块内")
	}
	end := strings.Index(html[i:], "</div>")
	if end < 0 {
		t.Fatal(".switch 块没有闭合")
	}
	block := html[start : i+end]
	// 只查 hint：标签本身是 <span class="lb">，那是 .switch 的正常构成。
	if strings.Contains(block, `class="hint"`) {
		t.Errorf("说明文字被塞进了 .switch 内部，会被横排 flex 挤成窄条；"+
			"应改为 .switch 结束后另起 <div class=\"hint\">。实际块：%s", block)
	}

	// 说明确实存在（没有为了让上面的断言通过而干脆删掉）。
	if !strings.Contains(html, "裸模型名跨域回退") {
		t.Error("找不到开关标签")
	}
}

// TestCrossDomainFallbackMappedInJS CFG_MAP 必须有映射，否则保存不生效。
//
// 这是「假成功」类缺陷：控件画出来了、用户勾了、点保存提示成功，但
// collectConfig 根本不认识这个字段，config.json 里什么都没写。
func TestCrossDomainFallbackMappedInJS(t *testing.T) {
	p := newTestPanel()
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest("GET", "/panel/js/config.js", nil))
	if rec.Code != 200 {
		t.Fatalf("/panel/js/config.js 返回 %d", rec.Code)
	}
	js := rec.Body.String()

	// CFG_MAP 条目形态：cross_domain_fallback: ['pool', 'cross_domain_fallback'],
	want := crossDomainField + ": ['pool', '" + crossDomainField + "']"
	if !strings.Contains(js, want) {
		t.Fatalf("config.js 缺少 CFG_MAP 映射（找不到 %s）——控件会读不回填、保存不下发", want)
	}
}
