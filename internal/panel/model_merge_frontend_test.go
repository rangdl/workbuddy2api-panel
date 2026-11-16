package panel

import (
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
)

// model_merge_frontend_test.go 用 node + vm 直接跑 models.js 里的合并渲染逻辑。
//
// 为什么必须真跑而不是只检查字符串存在：合并视图的核心是「一行一个模型 +
// 一串来源徽标」，拼错一个字段名（比如把 providers 写成 provider）不会让
// 任何 Go 测试失败，页面却会变成一片空白或「—」。项目里已有同型做法
// （TestAppJSExpirySortedByExpiry），这里沿用。

// runModelsJS 抽出 models.js 中从 MergeProviderTag 到 renderMergedModels 的片段，
// 在 node 里执行并把结果序列化回来。
func runModelsJS(t *testing.T, body string) string {
	t.Helper()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; merged model render test skipped")
	}
	// 抽片段的边界：从 mdRealmOf 开始（mdUpstreamTag 依赖它，也顺带把
	// MD_UPSTREAM_LABEL = BACKEND_LABEL 的接线真跑一遍），到 mdApplyView 结束。
	const startMark = "// mdRealmOf 取模型 id 里的域段"
	const endMark = "// mdApplyView 切换"
	// exportsJS 在片段末尾把需要的函数收集成一个对象返回。
	// 必须与片段**同一次** runInContext 执行——函数声明留在该次执行的词法
	// 作用域里，第二次调用取不到。
	exportsJS := "\n({mdProviderTag:mdProviderTag,mdProviderTags:mdProviderTags," +
		"mdUpstreamTag:mdUpstreamTag," +
		"mdMergedRowHtml:mdMergedRowHtml," +
		"mdMergedMatch:mdMergedMatch,mdMergedSortList:mdMergedSortList,renderMergedModels:renderMergedModels});\n"
	script := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const start = src.indexOf(START_MARK);
const end = src.indexOf(END_MARK);
if (start < 0 || end < 0 || end <= start) throw new Error('merged render region not found');
const ctx = {
  esc: s => String(s == null ? '' : s).replace(/[&<>"]/g, c => ({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;'}[c])),
  fmtK: n => { n = Number(n || 0); return n >= 1000 ? Math.round(n / 1000) + 'K' : String(n); },
  BACKEND_LABEL: { codebuddy: 'WorkBuddy', trae: 'Trae' },
  $: () => ({ innerHTML: '', textContent: '', className: '', hidden: false }),
  mdFilter: { q: '', upstream: '', realm: '', cap: '', effort: '', promo: '', sort: 'default' },
  mdView: 'merged', mdMerged: [],
  String, Number, Math, Array, Object, JSON,
};
vm.createContext(ctx);
const api = vm.runInContext(src.slice(start, end) + EXPORTS_JS, ctx);
Object.assign(globalThis, api);
` + body
	script = strings.NewReplacer(
		"START_MARK", strconv.Quote(startMark),
		"END_MARK", strconv.Quote(endMark),
		"EXPORTS_JS", strconv.Quote(exportsJS),
	).Replace(script)
	f, err := os.CreateTemp(t.TempDir(), "mdmerge-*.cjs")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(script); err != nil {
		t.Fatal(err)
	}
	f.Close()
	out, err := exec.Command(node, f.Name(), "js/models.js").CombinedOutput()
	if err != nil {
		t.Fatalf("node run failed: %v\n%s", err, out)
	}
	return strings.TrimSpace(string(out))
}

// TestMergedRowRendersProvidersAndKeepsOneRowPerModel 一行一模型，来源全部列出。
func TestMergedRowRendersProvidersAndKeepsOneRowPerModel(t *testing.T) {
	out := runModelsJS(t, `
const m = {
  id: 'deepseek-v4.1-flash', bare_id: 'deepseek-v4.1-flash',
  name: 'DeepSeek-V4.1-Flash', hidden_all: false, provider_count: 3,
  context_length: 1000000, max_output_tokens: 128000,
  supports_tool_call: true, supported_efforts: ['low','high'],
  providers: [
    { upstream: 'codebuddy', realm: 'cn', id: 'cn:deepseek-v4.1-flash', credits: 'x0.03', context_length: 1000000 },
    { upstream: 'trae', realm: 'cn', id: 'trae:cn:DeepSeek-V4.1-Flash', credits: 'x0.06', context_length: 200000 },
    { upstream: 'trae', realm: 'global', id: 'trae:global:DeepSeek-V4.1-Flash', credits: 'x0.06' },
  ],
};
const html = mdMergedRowHtml(m);
process.stdout.write(JSON.stringify({
  providers: [...html.matchAll(/md-prov[^"]*" data-up="([^"]*)" data-realm="([^"]*)"/g)].map(x => x[1]+'·'+x[2]),
  providerRows: (html.match(/md-prov-list/g) || []).length,
  hasModel: html.includes('deepseek-v4.1-flash'),
  context: (html.match(/(\d+)K<\/td><td class="num">/) || [])[1],
}));`)
	want := `{"providers":["codebuddy·cn","trae·cn","trae·global"],"providerRows":1,"hasModel":true,"context":"1000"}`
	if out != want {
		t.Fatalf("rendered=%s\nwant %s", out, want)
	}
}

// TestMergedRowMarksFullyHidden 全部来源被藏的模型要标「已隐藏」并划线。
//
// 运维页面上「被藏了什么」本身是需要知道的信息——不能像客户端那样直接不返回。
func TestMergedRowMarksFullyHidden(t *testing.T) {
	out := runModelsJS(t, `
const hidden = {
  id: 'glm-5.2', bare_id: 'glm-5.2', hidden_all: true, provider_count: 1,
  providers: [{ upstream: 'trae', realm: 'cn', id: 'trae:cn:glm-5.2', hidden: true }],
};
const visible = {
  id: 'kimi-k3', bare_id: 'kimi-k3', hidden_all: false, provider_count: 1,
  providers: [{ upstream: 'trae', realm: 'cn', id: 'trae:cn:kimi-k3' }],
};
const a = mdMergedRowHtml(hidden), b = mdMergedRowHtml(visible);
process.stdout.write(JSON.stringify({
  hiddenTag: a.includes('已隐藏'),
  hiddenStruck: a.includes('line-through'),
  visibleTag: b.includes('已隐藏'),
  visibleStruck: b.includes('line-through'),
}));`)
	want := `{"hiddenTag":true,"hiddenStruck":true,"visibleTag":false,"visibleStruck":false}`
	if out != want {
		t.Fatalf("hidden render=%s\nwant %s", out, want)
	}
}

// TestMergedFilterMatchesAnyProvider 筛上游/域时按「任一来源命中」判定。
//
// 语义差别很关键：明细视图里筛 Trae = 只留 Trae 的行；合并视图里筛 Trae
// = 只留有 Trae 来源的模型，但该模型的**其它来源仍然显示**——那正是用户
// 想看的信息（这个模型还有谁提供）。
func TestMergedFilterMatchesAnyProvider(t *testing.T) {
	out := runModelsJS(t, `
const m = {
  id: 'deepseek-v4.1-flash', bare_id: 'deepseek-v4.1-flash', hidden_all: false,
  providers: [
    { upstream: 'codebuddy', realm: 'cn', id: 'cn:x' },
    { upstream: 'trae', realm: 'global', id: 'trae:global:x' },
  ],
};
const cases = {
  trae:        { q:'', upstream:'trae', realm:'', cap:'', effort:'', promo:'', sort:'default' },
  codebuddy:   { q:'', upstream:'codebuddy', realm:'', cap:'', effort:'', promo:'', sort:'default' },
  globalRealm: { q:'', upstream:'', realm:'global', cap:'', effort:'', promo:'', sort:'default' },
  cnRealm:     { q:'', upstream:'', realm:'cn', cap:'', effort:'', promo:'', sort:'default' },
  none:        { q:'', upstream:'nope', realm:'', cap:'', effort:'', promo:'', sort:'default' },
  search:      { q:'global', upstream:'', realm:'', cap:'', effort:'', promo:'', sort:'default' },
};
const r = {};
for (const k in cases) r[k] = mdMergedMatch(m, cases[k]);
process.stdout.write(JSON.stringify(r));`)
	want := `{"trae":true,"codebuddy":true,"globalRealm":true,"cnRealm":true,"none":false,"search":true}`
	if out != want {
		t.Fatalf("filter=%s\nwant %s", out, want)
	}
}

// TestMergedSortByCheapestRate 合并视图按「最便宜的来源」排倍率。
func TestMergedSortByCheapestRate(t *testing.T) {
	out := runModelsJS(t, `
const a = { id:'cheap', bare_id:'cheap', providers:[{upstream:'codebuddy',realm:'cn',id:'cn:a',credits:'x0.03'}] };
const b = { id:'mixed', bare_id:'mixed', providers:[
  {upstream:'codebuddy',realm:'cn',id:'cn:b',credits:'x0.5'},
  {upstream:'trae',realm:'cn',id:'trae:cn:b',credits:'x0.02'},
]};
const c = { id:'pricey', bare_id:'pricey', providers:[{upstream:'trae',realm:'cn',id:'trae:cn:c',credits:'x0.9'}] };
const d = { id:'noprice', bare_id:'noprice', providers:[{upstream:'trae',realm:'cn',id:'trae:cn:d'}] };
const f = { q:'', upstream:'', realm:'', cap:'', effort:'', promo:'', sort:'rate' };
process.stdout.write(JSON.stringify(mdMergedSortList([a,b,c,d], f).map(x => x.id)));`)
	// mixed 的最便宜来源是 x0.02，应排第一；无价格排最后。
	want := `["mixed","cheap","pricey","noprice"]`
	if out != want {
		t.Fatalf("rate sort=%s\nwant %s", out, want)
	}
}
