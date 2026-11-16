package panel

import (
	"sort"
	"testing"
)

// model_merge_test.go 守「一行一模型」的聚合口径。
//
// 用户要的是：模型列表里 deepseek 只显示一条，然后标出它由哪些
// 上游×域提供（WorkBuddy·CN / Trae·CN / Trae·Global），而不是按上游
// 各列一遍。聚合的每一处取舍都会直接影响这张表是否可信，所以逐条断言。

// mkEntry 构造一条面板目录条目（模拟 /panel/api/models 的平铺输出）。
func mkEntry(upstream, realm, bareID string, extra map[string]any) map[string]any {
	prefix := panelPrefixFor(upstream, realm)
	e := map[string]any{
		"id":       prefix + bareID,
		"upstream": upstream,
		"realm":    realm,
		"name":     bareID,
	}
	for k, v := range extra {
		e[k] = v
	}
	return e
}

// findMerged 在聚合结果里按归一 id 找一条。
func findMerged(list []map[string]any, id string) map[string]any {
	for _, m := range list {
		if m["id"] == id {
			return m
		}
	}
	return nil
}

// TestMergeDedupesByNormalizedName 大小写不同的同名模型必须合成一行。
//
// 这是需求的核心：Trae 目录里是 `DeepSeek-V4.1-Flash`，WorkBuddy 是
// `deepseek-v4.1-flash`。不归一就会变成两行，而用户要的是「deepseek 一条」。
func TestMergeDedupesByNormalizedName(t *testing.T) {
	entries := []map[string]any{
		mkEntry("codebuddy", "cn", "deepseek-v4.1-flash", nil),
		mkEntry("trae", "cn", "DeepSeek-V4.1-Flash", nil),
		mkEntry("trae", "global", "DeepSeek-V4.1-Flash", nil),
	}
	merged := panelMergeModels(entries)
	if len(merged) != 1 {
		t.Fatalf("应合成 1 行，实际 %d 行：%v", len(merged), mergedIDs(merged))
	}
	m := merged[0]
	provs, _ := m["providers"].([]map[string]any)
	if len(provs) != 3 {
		t.Fatalf("提供者数 = %d, want 3（%v）", len(provs), provs)
	}
	if got := m["id"]; got != "deepseek-v4.1-flash" {
		t.Errorf("聚合 id = %v, want 归一后小写", got)
	}
	// 三个来源的完整调用名都要保留（客户端实际要填的值）。
	wantIDs := []string{
		"cn:deepseek-v4.1-flash",
		"trae:cn:DeepSeek-V4.1-Flash",
		"trae:global:DeepSeek-V4.1-Flash",
	}
	var gotIDs []string
	for _, p := range provs {
		gotIDs = append(gotIDs, p["id"].(string))
	}
	sort.Strings(gotIDs)
	sort.Strings(wantIDs)
	for i := range wantIDs {
		if gotIDs[i] != wantIDs[i] {
			t.Errorf("调用名 = %v, want %v", gotIDs, wantIDs)
		}
	}
}

func mergedIDs(list []map[string]any) []string {
	out := make([]string, 0, len(list))
	for _, m := range list {
		out = append(out, m["id"].(string))
	}
	return out
}

// TestMergeCapabilitiesUnionAndMaxForLimits 能力取并集、上限取最大。
//
// 主行代表「这个模型能干什么」——按最宽松的来源算。差异不隐藏，每个来源
// 自己的值完整保留在 providers[] 里，悬停可查。
func TestMergeCapabilitiesUnionAndMaxForLimits(t *testing.T) {
	entries := []map[string]any{
		mkEntry("codebuddy", "cn", "glm-5.2", map[string]any{
			"context_length":     int64(200000),
			"max_output_tokens":  int64(64000),
			"supports_tool_call": true,
			"supported_efforts":  []string{"low", "high"},
		}),
		mkEntry("trae", "cn", "glm-5.2", map[string]any{
			"context_length":    int64(1000000),
			"max_output_tokens": int64(128000),
			"supports_images":   true,
			"supported_efforts": []string{"high", "max"},
		}),
	}
	merged := panelMergeModels(entries)
	m := findMerged(merged, "glm-5.2")
	if m == nil {
		t.Fatal("未合成 glm-5.2")
	}
	if got := m["context_length"]; got != int64(1000000) {
		t.Errorf("上下文 = %v, want 1000000（取最大）", got)
	}
	if got := m["max_output_tokens"]; got != int64(128000) {
		t.Errorf("最大输出 = %v, want 128000（取最大）", got)
	}
	if m["supports_tool_call"] != true {
		t.Error("工具调用应取并集（WorkBuddy 支持）")
	}
	if m["supports_images"] != true {
		t.Error("视觉应取并集（Trae 支持）")
	}
	// 档位并集去重。
	eff, _ := m["supported_efforts"].([]string)
	sort.Strings(eff)
	want := []string{"high", "low", "max"}
	if len(eff) != len(want) {
		t.Fatalf("档位 = %v, want %v", eff, want)
	}
	for i := range want {
		if eff[i] != want[i] {
			t.Errorf("档位 = %v, want %v", eff, want)
		}
	}
	// 各来源自己的值必须保留（主行不撒谎的前提）。
	provs, _ := m["providers"].([]map[string]any)
	if len(provs) != 2 {
		t.Fatalf("提供者数 = %d", len(provs))
	}
	var sums []int64
	for _, p := range provs {
		v, _ := p["context_length"].(int64)
		sums = append(sums, v)
	}
	sort.Slice(sums, func(i, j int) bool { return sums[i] < sums[j] })
	if sums[0] != 200000 || sums[1] != 1000000 {
		t.Errorf("各来源上下文 = %v, want [200000 1000000]（原值必须保留）", sums)
	}
}

// TestMergeRateNotCombined 倍率不合并：各来源各带各的。
//
// 合并成一个数会误导计费预期——同一个模型在两个上游是分别计费的。
func TestMergeRateNotCombined(t *testing.T) {
	entries := []map[string]any{
		mkEntry("codebuddy", "cn", "kimi-k3", map[string]any{"credits": "x0.5"}),
		mkEntry("trae", "cn", "kimi-k3", map[string]any{"credits": "x0.8"}),
	}
	m := findMerged(panelMergeModels(entries), "kimi-k3")
	if m == nil {
		t.Fatal("未合成 kimi-k3")
	}
	// 主行不带单一倍率。
	if _, has := m["credits"]; has {
		t.Error("合并行不该带单一 credits（会误导计费预期）")
	}
	provs, _ := m["providers"].([]map[string]any)
	rates := map[string]string{}
	for _, p := range provs {
		rates[p["upstream"].(string)], _ = p["credits"].(string)
	}
	if rates["codebuddy"] != "x0.5" || rates["trae"] != "x0.8" {
		t.Errorf("各来源倍率 = %v, want codebuddy=x0.5 trae=x0.8", rates)
	}
}

// TestMergeProviderCountSortsFirst 多来源的模型排在前面（可切换、可容灾）。
func TestMergeProviderCountSortsFirst(t *testing.T) {
	entries := []map[string]any{
		mkEntry("codebuddy", "cn", "solo-model", nil),   // 1 个来源
		mkEntry("codebuddy", "cn", "shared-model", nil), // 3 个来源
		mkEntry("trae", "cn", "shared-model", nil),
		mkEntry("trae", "global", "shared-model", nil),
	}
	merged := panelMergeModels(entries)
	if len(merged) != 2 {
		t.Fatalf("应 2 行，实际 %d", len(merged))
	}
	if merged[0]["id"] != "shared-model" {
		t.Errorf("首行 = %v, want shared-model（来源多的排前面）", merged[0]["id"])
	}
	if merged[0]["provider_count"] != 3 {
		t.Errorf("provider_count = %v, want 3", merged[0]["provider_count"])
	}
}

// TestMergeHiddenAllOnlyWhenEveryProviderHidden 只有全部来源都被藏才标隐藏。
//
// 部分来源被藏时模型仍然可调（换个上游就行），标成「已隐藏」会让人以为调不到。
func TestMergeHiddenAllOnlyWhenEveryProviderHidden(t *testing.T) {
	// 部分隐藏。
	entries := []map[string]any{
		mkEntry("codebuddy", "cn", "half", map[string]any{"hidden": true}),
		mkEntry("trae", "cn", "half", map[string]any{"hidden": false}),
	}
	m := findMerged(panelMergeModels(entries), "half")
	if m["hidden_all"] == true {
		t.Error("只有一个来源被藏时不该标 hidden_all")
	}

	// 全部隐藏。
	entries = []map[string]any{
		mkEntry("codebuddy", "cn", "all", map[string]any{"hidden": true}),
		mkEntry("trae", "cn", "all", map[string]any{"hidden": true}),
	}
	m = findMerged(panelMergeModels(entries), "all")
	if m["hidden_all"] != true {
		t.Error("全部来源被藏时应标 hidden_all")
	}
}

// TestBareModelID 前缀剥离（两段式与三段式）。
func TestBareModelID(t *testing.T) {
	cases := map[string]string{
		"cn:glm-5.2":                  "glm-5.2",
		"global:glm-5.2":              "glm-5.2",
		"trae:cn:glm-5.2":             "glm-5.2",
		"trae:global:DeepSeek-V4-Pro": "DeepSeek-V4-Pro",
		"glm-5.2":                     "glm-5.2",
		// 不是已知域前缀 → 原样返回（例如带冒号的模型名）。
		"claude-3:5": "claude-3:5",
	}
	for in, want := range cases {
		if got := panelBareModelID(in); got != want {
			t.Errorf("panelBareModelID(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestPanelModelHiddenCoversTraeThreePartId hidden 必须能命中 Trae 的三段式 id。
//
// 历史缺陷：NormalizeHiddenModels 只产出两段式 "cn:x"，而 Trae 的完整 id 是
// "trae:cn:x"，拿完整 id 查表永远不命中——Trae 模型从来没有被真正隐藏过。
func TestPanelModelHiddenCoversTraeThreePartId(t *testing.T) {
	// 模拟 NormalizeHiddenModels 的输出（两段式）。
	hidden := map[string]struct{}{"cn:glm-5.2": {}, "global:gpt-5.4": {}}

	if !panelModelHidden(hidden, "trae", "cn", "glm-5.2") {
		t.Error("Trae CN 的 glm-5.2 应被 cn:glm-5.2 命中")
	}
	if !panelModelHidden(hidden, "codebuddy", "cn", "glm-5.2") {
		t.Error("WorkBuddy CN 的 glm-5.2 同样应被命中")
	}
	if panelModelHidden(hidden, "trae", "global", "glm-5.2") {
		t.Error("只藏了 cn 域，global 不该被藏")
	}
	if !panelModelHidden(hidden, "trae", "global", "gpt-5.4") {
		t.Error("global 的 gpt-5.4 应被命中")
	}
	// 空集合 = 不藏。
	if panelModelHidden(nil, "trae", "cn", "glm-5.2") {
		t.Error("空黑名单不该隐藏任何东西")
	}
}
