package panel

import (
	"bytes"
	"os"
	"regexp"
	"strings"
	"testing"
)

// TestUpstreamPageRemoved 「上游接入」页必须保持删除状态。
//
// 回归点：该页曾提供 Trae 的启用开关、对话 base、签到排程与设备凭证导入。
// 这些都与改造后的设计冲突：
//   - 启用开关   → 上游平权，加了账号即启用，没有"禁用某个上游"这回事
//   - 对话 base  → 上游路径不该是配置项
//   - 签到排程   → 统一到 config.json（总开关 + 按上游勾选），否则出现两份定时
//   - 设备凭证   → 已确认随页面一并移除（新账号在添加弹窗里带 storage.json）
//
// 如果有人把它加回来，签到定时与"启用"语义会立刻重新分裂。
func TestUpstreamPageRemoved(t *testing.T) {
	body := string(mustReadPanelFile(t, "index.html"))
	mustBeGone := []string{
		`data-view="upstream"`,    // 导航入口
		`id="view-upstream"`,      // 视图容器
		`id="traeEnabled"`,        // 启用开关（平权后不存在）
		`id="traeChatBase"`,       // 对话 base（上游路径不可配）
		`id="traeCheckinEnabled"`, // 签到排程（已并入 config.json）
		`id="traeAccBody"`,        // 上游页专属统计表
	}
	for _, sel := range mustBeGone {
		if strings.Contains(body, sel) {
			t.Errorf("「上游接入」页应已删除，但仍存在 %s", sel)
		}
	}
	// 配置页必须承接仍在的配置项。
	for _, want := range []string{`id="traeIdeVersion"`, `id="traeUserAgent"`, `id="checkinBackends"`} {
		if !strings.Contains(body, want) {
			t.Errorf("配置页缺少 %s（Trae 上游项与分上游签到已迁入）", want)
		}
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
