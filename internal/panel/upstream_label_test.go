package panel

import (
	"os"
	"strings"
	"testing"
)

// upstream_label_test.go 守「上游展示名全站唯一一份，且各处显示一致」。
//
// 起因是用户在模型页看到两张表里同一个上游叫两个名字：
//
//	合并视图的提供者徽标：CodeBuddy·Global
//	明细视图的上游徽标：  CodeBuddy
//	账号池分组行：        WorkBuddy
//
// 三处各写了一份映射表（accounts.js / config.js / models.js），其中模型页
// 那份写成 'CodeBuddy'。这类漂移不会让任何逻辑测试失败——只有人眼能看出来。
// 所以这里直接断言「源文件里不再有第二份定义」+「徽标渲染结果一致」。

// readJS 读 js 目录下的脚本。
func readJS(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile("js/" + name)
	if err != nil {
		t.Fatalf("读 %s 失败: %v", name, err)
	}
	return string(b)
}

// TestUpstreamLabelDefinedOnce 展示名常量只允许在 accounts.js 里定义一次。
//
// 其余文件必须赋值引用（`= BACKEND_LABEL`），不能再写一份字面量 map——
// 那正是两个名字的来源。
func TestUpstreamLabelDefinedOnce(t *testing.T) {
	accounts := readJS(t, "accounts.js")
	const def = "const BACKEND_LABEL = { codebuddy: 'WorkBuddy', trae: 'Trae' };"
	if !strings.Contains(accounts, def) {
		t.Fatalf("accounts.js 应有唯一一份定义：%s", def)
	}

	// 其它文件里不许再出现任何形式的字面量映射。
	for _, name := range []string{"models.js", "config.js", "usage.js"} {
		src := readJS(t, name)
		for _, bad := range []string{
			"{ codebuddy: 'CodeBuddy'",
			"{ codebuddy: 'WorkBuddy'",
			`codebuddy: "CodeBuddy"`,
			`codebuddy: "WorkBuddy"`,
		} {
			if strings.Contains(src, bad) {
				t.Errorf("%s 又写了一份上游展示名字面量 %q；应引用 accounts.js 的 BACKEND_LABEL", name, bad)
			}
		}
	}

	// 引用点必须在（防止有人把别名删了导致 ReferenceError）。
	if !strings.Contains(readJS(t, "models.js"), "const MD_UPSTREAM_LABEL = BACKEND_LABEL;") {
		t.Error("models.js 的 MD_UPSTREAM_LABEL 应直接引用 BACKEND_LABEL")
	}
	if !strings.Contains(readJS(t, "config.js"), "const BACKEND_LABEL_CFG = BACKEND_LABEL;") {
		t.Error("config.js 的 BACKEND_LABEL_CFG 应直接引用 BACKEND_LABEL")
	}
}

// TestDetailViewHasNoSourceTag 明细视图不再在 ID 前挂来源标签。
//
// 明细视图的 ID 本身就把来源写全了（`cn:x` / `global:x` / `trae:cn:x`），
// 前面再挂一个 `wb:cn` / `trae:cn` 徽标是把同一份信息写两遍，还挤占模型名的
// 横向空间。合并视图则相反：一行一模型，ID 里没有来源，必须靠徽标标出。
func TestDetailViewHasNoSourceTag(t *testing.T) {
	out := runModelsJS(t, `
const rows = [
  { id:'cn:glm-5.2', upstream:'codebuddy', realm:'cn', name:'GLM-5.2' },
  { id:'global:gpt-5.4', upstream:'codebuddy', realm:'global', name:'GPT-5.4' },
  { id:'trae:cn:deepseek-v4-pro', upstream:'trae', realm:'cn', name:'DeepSeek V4 Pro' },
];
process.stdout.write(JSON.stringify({
  tagEmpty: rows.every(m => mdUpstreamTag(m) === ''),
  // 徽标渲染函数本身仍然可用（合并视图要用），只是明细视图不再调用它。
  chipStillWorks: mdProviderTag({upstream:'trae',realm:'cn',id:'trae:cn:x'}, false).includes('trae:cn'),
}));`)
	const want = `{"tagEmpty":true,"chipStillWorks":true}`
	if out != want {
		t.Fatalf("明细视图=%s\nwant %s", out, want)
	}
}

// TestMergedProviderTagShowsCallPrefix 合并视图的来源徽标显示**可直填的调用前缀**。
//
// 写展示名（WorkBuddy·CN）用户还得自己翻译回 `wb:` / `trae:` 才能填进客户端；
// 直接显示前缀就少一道心算。`wb` 是后端给 CodeBuddy 注册的别名
// （backend/codebuddy 的 Aliases()），与 `cn:<模型>` 等价。
func TestMergedProviderTagShowsCallPrefix(t *testing.T) {
	out := runModelsJS(t, `
const txt = p => mdProviderTag(p, false).replace(/<[^>]*>/g,'');
process.stdout.write(JSON.stringify({
  buddyCn:     txt({upstream:'codebuddy', realm:'cn',     id:'cn:x'}),
  buddyGlobal: txt({upstream:'codebuddy', realm:'global', id:'global:x'}),
  traeCn:      txt({upstream:'trae',      realm:'cn',     id:'trae:cn:x'}),
  traeGlobal:  txt({upstream:'trae',      realm:'global', id:'trae:global:x'}),
}));`)
	const want = `{"buddyCn":"wb:cn","buddyGlobal":"wb:global","traeCn":"trae:cn","traeGlobal":"trae:global"}`
	if out != want {
		t.Fatalf("来源徽标=%s\nwant %s", out, want)
	}
}

// TestIndexHTMLUsesWorkBuddyLabel 静态 HTML 里的上游名同样只能是 WorkBuddy。
//
// 筛选项与「添加账号」的单选直接写在 index.html 里，不走 JS 常量，所以
// 上面的常量检查管不到它们——这两处此前也写着 CodeBuddy。
func TestIndexHTMLUsesWorkBuddyLabel(t *testing.T) {
	b, err := os.ReadFile("index.html")
	if err != nil {
		t.Fatalf("读 index.html 失败: %v", err)
	}
	html := string(b)
	// 用户可见的 ">CodeBuddy<" 形式（注释里提到 CodeBuddy 是允许的：
	// 它确实是后端标识符与目录名）。
	if strings.Contains(html, "> CodeBuddy<") || strings.Contains(html, ">CodeBuddy<") {
		t.Error("index.html 里仍有以 CodeBuddy 显示的选项；用户可见文案统一为 WorkBuddy")
	}
	for _, want := range []string{`<option value="codebuddy">WorkBuddy</option>`,
		`value="codebuddy" checked> WorkBuddy</label>`} {
		if !strings.Contains(html, want) {
			t.Errorf("index.html 缺少 %q", want)
		}
	}
}

// TestMergedProviderTagMarksHidden 合并视图的来源徽标要标出 hidden。
//
// 后端 /panel/api/models 的每条明细都带 hidden 字段（panel.go 的 entry["hidden"]）。
// 徽标不读它的话，「页面看得见、客户端调不到」在模型页仍然存在。
//
// 明细视图的标签去掉后，hidden 改由 ID 本身划线 + 「已隐藏」标记表达（见
// mdRowHtml），那是 DOM 层的事，这里只锁合并视图的徽标。
func TestMergedProviderTagMarksHidden(t *testing.T) {
	out := runModelsJS(t, `
const a = { upstream:'codebuddy', realm:'cn', id:'cn:glm-5.2', hidden:true };
const b = { upstream:'codebuddy', realm:'cn', id:'cn:kimi-k3', hidden:false };
const f = p => { const h = mdProviderTag(p, false); return h.includes('line-through') && h.includes('models.hidden'); };
process.stdout.write(JSON.stringify({hidden:f(a), visible:f(b)}));`)
	const want = `{"hidden":true,"visible":false}`
	if out != want {
		t.Fatalf("hidden 标记=%s\nwant %s", out, want)
	}
}
