package panel

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestTraeAccountTableRendering Trae 账号表的渲染必须与账号池同构：
// 9 列、积分/用量/成功失败都从 stats 里取、凭证不回显。
//
// 做法：把 config.js 里 traeCreditsHtml / traeUsageHtml / traeStatusHtml 三段
// 连同 core.js 的格式化函数一起放进 node 求值（不引 DOM——这三个函数只拼字符串）。
// Go 侧的测试无法发现「字段名写错导致整列显示 —」这类问题，故补这一层。
func TestTraeAccountTableRendering(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; trae table rendering test skipped")
	}
	script := `const fs = require('fs');
const vm = require('vm');
const core = fs.readFileSync('js/core.js', 'utf8');
const cfg = fs.readFileSync('js/config.js', 'utf8');

// 从 core.js 抠出格式化函数（它们互相独立，不含 DOM 依赖）。
function grab(src, names) {
  let out = '';
  for (const n of names) {
    const start = src.indexOf('function ' + n + '(');
    if (start < 0) throw new Error('未找到函数 ' + n);
    // 按大括号配对截取整个函数体。
    let i = src.indexOf('{', start), depth = 0, end = -1;
    for (; i < src.length; i++) {
      if (src[i] === '{') depth++;
      else if (src[i] === '}') { depth--; if (depth === 0) { end = i + 1; break; } }
    }
    out += src.slice(start, end) + '\n';
  }
  return out;
}
const helpers = grab(core, ['esc', 'ago', 'formatTokenCount', 'formatLatency', 'formatRate']);
const targets = grab(cfg, ['traeCreditsHtml', 'traeUsageHtml', 'traeStatusHtml']);

const sandbox = { console, Number, Math, String, Date, isFinite, isNaN, parseInt, parseFloat };
vm.createContext(sandbox);
vm.runInContext(helpers + targets, sandbox, { filename: 'trae-render.js' });

const assert = (cond, msg) => { if (!cond) { console.log('FAIL: ' + msg); process.exit(1); } };

// ── 积分列 ──────────────────────────────────────────────────
// 有上限 → 显示 剩余/额度 + 进度条
let h = sandbox.traeCreditsHtml({ credits: 1500, credits_limit: 6000, credits_ide: 1200, credits_work: 300 });
assert(h.includes('1,500'), '积分剩余未渲染: ' + h);
assert(h.includes('/6,000'), '额度上限未渲染: ' + h);
assert(h.includes('width:25%'), '进度条百分比错误（1500/6000 应为 25%）: ' + h);
// 只有 notify_usage 的余额（无上限）→ 不画进度条
h = sandbox.traeCreditsHtml({ credits: 400, credits_ide: 300, credits_work: 100 });
assert(h.includes('400'), '余额未渲染: ' + h);
assert(!h.includes('<div class="bar">'), '无上限时不应画进度条: ' + h);
// 完全未知 → 破折号，不能是 NaN
h = sandbox.traeCreditsHtml({});
assert(h.includes('—'), '未知积分应显示破折号: ' + h);
assert(!h.includes('NaN'), '积分列出现 NaN: ' + h);
// 缺失字段（undefined）也不能 NaN
h = sandbox.traeCreditsHtml(undefined);
assert(!h.includes('NaN'), 'undefined stats 出现 NaN: ' + h);

// ── 用量列 ──────────────────────────────────────────────────
h = sandbox.traeUsageHtml({ last_latency_ms: 850, token_usage: { request_count: 12, total_tokens: 1500000, last_tokens_per_second: 42.5 } });
assert(h.includes('12'), '请求数未渲染: ' + h);
assert(h.includes('1.5m'), 'total_tokens 未按 m 缩写: ' + h);
assert(h.includes('850ms'), '延迟未渲染: ' + h);
assert(h.includes('42.5tok/s'), '吞吐未渲染: ' + h);
// 零值不能出现 NaN / undefined
h = sandbox.traeUsageHtml({});
assert(!h.includes('NaN') && !h.includes('undefined'), '空 stats 用量列异常: ' + h);

// ── 状态列 ──────────────────────────────────────────────────
h = sandbox.traeStatusHtml({ has_token: true, token_tail: 'abcd', has_refresh: true, has_device_key: true }, null);
assert(h.includes('可用'), '可用状态未渲染: ' + h);
assert(h.includes('token abcd'), 'token 尾号未渲染: ' + h);
assert(h.includes('今日已签') === false, '未签到时不应显示已签');
// 冷却优先于可用
h = sandbox.traeStatusHtml({ has_token: true }, { remaining_sec: 300, reason: '额度用尽' });
assert(h.includes('冷却'), '冷却状态未渲染: ' + h);
assert(h.includes('5 分钟'), '冷却剩余时间错误: ' + h);
// 禁用优先于一切
h = sandbox.traeStatusHtml({ has_token: true, disabled: true }, { remaining_sec: 300 });
assert(h.includes('已禁用'), '禁用状态未渲染: ' + h);
// 签到状态
h = sandbox.traeStatusHtml({ has_token: true, checkin_done: true });
assert(h.includes('今日已签'), '账号已签未渲染: ' + h);
h = sandbox.traeStatusHtml({ has_token: true, checkin_device_already: true });
assert(h.includes('设备已签'), '设备已签未渲染: ' + h);
// 凭证缺失要显式提示
h = sandbox.traeStatusHtml({ has_token: false });
assert(h.includes('无 token'), '缺 token 未提示: ' + h);
h = sandbox.traeStatusHtml({ has_token: true, has_refresh: false, has_device_key: false });
assert(h.includes('无 refresh'), '缺 refresh 未提示: ' + h);
assert(h.includes('无设备密钥'), '缺设备密钥未提示: ' + h);

console.log('RENDER OK');
process.exit(0);
`
	hf, err := os.CreateTemp(t.TempDir(), "trae-render-*.cjs")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := hf.WriteString(script); err != nil {
		t.Fatal(err)
	}
	hf.Close()

	cmd := exec.Command(node, hf.Name())
	cmd.Dir = "." // 测试工作目录 = internal/panel
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("Trae 账号表渲染测试失败: %v\n%s", err, out)
	}
	if !bytes.Contains(out, []byte("RENDER OK")) {
		t.Fatalf("未通过:\n%s", out)
	}
	_ = filepath.Join // 保持 import 使用（不同 Go 版本对未用 import 的处理一致）
}

// TestTraeAccountTableColumns index.html 的 Trae 账号表必须与账号池同列数、
// 同列序——两张表共用一套移动端卡片布局（mobile.css 用 nth-child 映射）。
func TestTraeAccountTableColumns(t *testing.T) {
	p := newTestPanel()
	body := string(mustReadPanelFile(t, "index.html"))
	_ = p

	// 表头列名按顺序必须一致（色条列无文本，不参与）。
	want := []string{"账号", "状态", "积分", "成功 / 失败", "在途", "用量", "最近成功"}
	if got := traeTableHeaders(t, body, "trae-acc-tbl"); !equalStrings(got, want) {
		t.Errorf("Trae 表头 = %v, want %v", got, want)
	}
	if got := traeTableHeaders(t, body, `class="acc"`); !equalStrings(got, want) {
		t.Errorf("账号池表头 = %v（两表必须同构）", got)
	}

	// 渲染容器的 id 必须与 JS 里取的一致。
	if !bytes.Contains([]byte(body), []byte(`id="traeAccBody"`)) {
		t.Error("index.html 缺少 traeAccBody（JS 渲染目标）")
	}
	if bytes.Contains([]byte(body), []byte(`id="traeAccounts"`)) {
		t.Error("旧的 traeAccounts 容器应已移除")
	}
}

// mustReadPanelFile 读取 internal/panel 下的静态资源。
func mustReadPanelFile(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(name)
	if err != nil {
		t.Fatalf("读取 %s: %v", name, err)
	}
	return b
}

// traeTableHeaders 从 index.html 里按类名锚点找出表格的表头文本。
//
// 只取 <th> 内**不含 HTML 标签**的纯文本：形如 `<th class="mark" aria-hidden="true"></th>`
// 的色条列没有文字，不该出现在结果里；带内部标签的表头（如排序箭头）这里用不上。
func traeTableHeaders(t *testing.T, html, anchor string) []string {
	t.Helper()
	i := bytes.Index([]byte(html), []byte(anchor))
	if i < 0 {
		t.Fatalf("未找到锚点 %q", anchor)
	}
	rest := html[i:]
	th := bytes.Index([]byte(rest), []byte("<thead>"))
	if th < 0 {
		t.Fatalf("锚点 %q 之后没有 <thead>", anchor)
	}
	rest = rest[th:]
	end := bytes.Index([]byte(rest), []byte("</thead>"))
	if end < 0 {
		t.Fatalf("锚点 %q 的 thead 未闭合", anchor)
	}
	seg := rest[:end]
	var out []string
	for {
		s := bytes.Index([]byte(seg), []byte("<th"))
		if s < 0 {
			break
		}
		seg = seg[s:]
		gt := bytes.Index([]byte(seg), []byte(">"))
		if gt < 0 {
			break
		}
		seg = seg[gt+1:]
		c := bytes.Index([]byte(seg), []byte("</th>"))
		if c < 0 {
			break
		}
		txt := strings.TrimSpace(string(seg[:c]))
		// 去掉内部标签（本文件里的表头没有，但避免将来加了排序箭头就挂）。
		txt = regexp.MustCompile(`<[^>]*>`).ReplaceAllString(txt, "")
		txt = strings.Join(strings.Fields(txt), " ")
		if txt != "" {
			out = append(out, txt)
		}
		seg = seg[c+5:]
	}
	return out
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
