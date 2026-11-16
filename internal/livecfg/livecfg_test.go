package livecfg

import "testing"

// TestNormalizeHiddenModels 归一化语义：
//   - nil/空/全空白 → nil（不隐藏）
//   - 裸名 → 两域都藏（"cn:x" + "global:x"）
//   - "cn:"/"global:" 前缀 → 只藏该域
//   - 非 cn/global 前缀（如 "xx:yy"）视为裸名整体（与 resolveModel 对称：
//     前缀不合法时 realm=cn、bare=原串，网关侧检查 "cn:xx:yy" 命中）
func TestNormalizeHiddenModels(t *testing.T) {
	if got := NormalizeHiddenModels(nil); got != nil {
		t.Errorf("nil → %v, want nil", got)
	}
	if got := NormalizeHiddenModels([]string{}); got != nil {
		t.Errorf("empty → %v, want nil", got)
	}
	if got := NormalizeHiddenModels([]string{"  ", ""}); got != nil {
		t.Errorf("all blank → %v, want nil", got)
	}

	got := NormalizeHiddenModels([]string{"deepseek-v3"})
	for _, want := range []string{"cn:deepseek-v3", "global:deepseek-v3"} {
		if _, ok := got[want]; !ok {
			t.Errorf("bare name: missing %q in %v", want, got)
		}
	}
	if len(got) != 2 {
		t.Errorf("bare name: got %d entries %v, want 2", len(got), got)
	}

	got = NormalizeHiddenModels([]string{"cn:pro-model"})
	if _, ok := got["cn:pro-model"]; !ok {
		t.Errorf("cn: prefix: missing %q in %v", "cn:pro-model", got)
	}
	if _, ok := got["global:pro-model"]; ok {
		t.Errorf("cn: prefix: %q must not hide global realm", "global:pro-model")
	}

	// 前后空白被裁掉
	got = NormalizeHiddenModels([]string{"  glm-5.2  "})
	if _, ok := got["cn:glm-5.2"]; !ok {
		t.Errorf("trimmed bare name: missing %q in %v", "cn:glm-5.2", got)
	}

	// 非法前缀视为裸名整体
	got = NormalizeHiddenModels([]string{"xx:yy"})
	for _, want := range []string{"cn:xx:yy", "global:xx:yy"} {
		if _, ok := got[want]; !ok {
			t.Errorf("odd prefix: missing %q in %v", want, got)
		}
	}
}

// TestNormalizeHiddenModelsTraeThreePart Trae 的三段式 id 必须被正确折算。
//
// 历史缺陷：三段式 "trae:cn:x" 被当成裸名，产出 "cn:trae:cn:x" / "global:trae:cn:x"
// 这种永远不会命中的键——**Trae 的模型从来没有被真正隐藏过**。而下游的过滤点
// 都是按「域 + 裸名」查表的，所以三段式必须折算成它对应的两段式。
func TestNormalizeHiddenModelsTraeThreePart(t *testing.T) {
	got := NormalizeHiddenModels([]string{"trae:cn:glm-5.2"})
	if _, ok := got["cn:glm-5.2"]; !ok {
		t.Errorf("三段式应折算为 %q，实际 %v", "cn:glm-5.2", got)
	}
	if _, ok := got["global:glm-5.2"]; ok {
		t.Errorf("三段式指定 cn 域时不该隐藏 global，实际 %v", got)
	}
	// 不该产生垃圾键。
	for k := range got {
		if len(k) > 0 && (k == "cn:trae:cn:glm-5.2" || k == "global:trae:cn:glm-5.2") {
			t.Errorf("产生了永远不会命中的垃圾键 %q", k)
		}
	}

	got = NormalizeHiddenModels([]string{"trae:global:gpt-5.4"})
	if _, ok := got["global:gpt-5.4"]; !ok {
		t.Errorf("Trae global 三段式应折算为 %q，实际 %v", "global:gpt-5.4", got)
	}
	if _, ok := got["cn:gpt-5.4"]; ok {
		t.Errorf("指定 global 时不该隐藏 cn，实际 %v", got)
	}
}
