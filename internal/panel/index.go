// index.go 面板静态资源与安全响应头。
//
// 资源经 go:embed 打进二进制（随服务部署，无外部构建步骤）：
//   - index.html  页面骨架
//   - js/*.js     前端逻辑，按视图拆分为多个文件（见下方 jsFiles）
//   - mobile.css  手机端（≤760px）适配样式（本仓库自有，上游不维护 → 同步上游不冲突）
//   - mobile.js   手机端联动逻辑（抽屉开合、宽表卡片化字段名），须在全部 js/*.js 之后加载
//
// 拆分理由：单文件 4000 行时，任何改动都要先在大文件里定位；按视图拆开后
// 改日志只需读 logs.js（~350 行）。拆分是纯搬迁，函数体一行未改。
// 加载顺序由 jsFiles 固定：core 提供公共能力，boot 必须最后（它立即调用 start()）。
//
// 安全头对"面板页面与全部 /panel/api/* 响应"统一生效：CSP 限制脚本只能来自本服务，
// 禁止被 iframe 嵌套（防点击劫持），禁 MIME 嗅探，并声明不泄露 Referer 出去。
package panel

import (
	"embed"
	_ "embed"
	"net/http"
	"strings"
)

//go:embed index.html
var indexHTML []byte

//go:embed mobile.css
var mobileCSS []byte

//go:embed mobile.js
var mobileJS []byte

// jsFiles 前端逻辑文件的固定加载顺序（同时决定 /panel/js/{name}.js 路由）。
//
// 顺序不能乱：
//   - core 提供 $ / api / esc / 格式化等公共能力，必须最先；
//   - 各视图模块之间只通过全局函数名互相引用，定义先后不敏感；
//   - boot 里有顶层 start() 与首屏 go()，必须最后。
//
//go:embed js/core.js js/accounts.js js/models.js js/logs.js js/usage.js js/packages.js js/tasks.js js/config.js js/boot.js
var jsFS embed.FS

// jsLoadOrder 与上面的 embed 列表一致，供路由逐个输出。
var jsLoadOrder = []string{
	"core", "accounts", "models", "logs", "usage", "packages", "tasks", "config", "boot",
}

// csp 内容安全策略（严格版，无需 unsafe-inline）：
//   - default-src 'none'        默认全禁，逐个开口
//   - script-src 'self'         只跑同源脚本（app.js / mobile.js）；页面无内联事件处理器/内联脚本
//   - style-src 'self' 'unsafe-inline'
//     style 的内联是设计取舍：页面有少量 style="..." 属性（进度条宽度、表格列宽），
//     允许内联样式不会导致脚本执行；仍禁止外部样式域与 @import 外链。
//   - connect-src 'self'        前端 fetch 只能打本服务
//   - img-src 'self' data:      图标/内联图
//   - form-action 'none'        页面无表单提交目标（配置页是 JS 提交）
//   - frame-ancestors 'none'    禁止被任何站点 iframe 嵌套（点击劫持）
//   - base-uri 'none'          禁止注入 <base> 改写相对路径
const csp = "default-src 'none'; script-src 'self'; style-src 'self' 'unsafe-inline'; " +
	"connect-src 'self'; img-src 'self' data:; form-action 'none'; " +
	"frame-ancestors 'none'; base-uri 'none'"

// setSecurityHeaders 写入面板统一安全响应头（页面与 API 都要，API 也含 JSON 数据）。
func setSecurityHeaders(w http.ResponseWriter) {
	w.Header().Set("Content-Security-Policy", csp)
	w.Header().Set("X-Content-Type-Options", "nosniff") // 禁 MIME 嗅探
	w.Header().Set("X-Frame-Options", "DENY")           // 老浏览器兜底（CSP frame-ancestors 的等价项）
	w.Header().Set("Referrer-Policy", "no-referrer")    // 不外泄面板地址给外部站点
	w.Header().Set("Cross-Origin-Opener-Policy", "same-origin")
	w.Header().Set("Cross-Origin-Resource-Policy", "same-origin")
}

// index 输出面板页面（静态无秘密；数据接口 /panel/api/* 才走鉴权）。
func (p *Panel) index(w http.ResponseWriter, r *http.Request) {
	setSecurityHeaders(w)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(indexHTML)
}

// jsScript 输出前端逻辑文件（同源脚本，供 CSP script-src 'self' 加载）。
// 路径 /panel/js/{name}.js，name 必须在 jsLoadOrder 白名单内——
// 直接拼 embed 路径会让 ../ 之类的输入有机会越界，白名单同时也固定了可用文件集。
func (p *Panel) jsScript(w http.ResponseWriter, r *http.Request) {
	// 路由模式是 /panel/js/{name}.js，net/http 的 {name} 会吞掉整段含扩展名的文本，
	// 这里先剥掉 .js 再与白名单比对。
	name := strings.TrimSuffix(r.PathValue("name"), ".js")
	ok := false
	for _, n := range jsLoadOrder {
		if n == name {
			ok = true
			break
		}
	}
	if !ok {
		http.NotFound(w, r)
		return
	}
	data, err := jsFS.ReadFile("js/" + name + ".js")
	if err != nil {
		http.NotFound(w, r)
		return
	}
	setSecurityHeaders(w)
	w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

// mobileStyle 输出手机端适配样式（同源样式表，CSP style-src 'self' 加载）。
// 单独成文而非写进 index.html：上游频繁改 index.html，独立文件可避免同步上游时反复冲突。
func (p *Panel) mobileStyle(w http.ResponseWriter, r *http.Request) {
	setSecurityHeaders(w)
	w.Header().Set("Content-Type", "text/css; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(mobileCSS)
}

// mobileScript 输出手机端联动逻辑（同源脚本，须在 app.js 之后加载）。
func (p *Panel) mobileScript(w http.ResponseWriter, r *http.Request) {
	setSecurityHeaders(w)
	w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(mobileJS)
}
