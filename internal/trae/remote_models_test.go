package trae

import "testing"

// TestParseConsumptionRate features 是 JSON 字符串，倍率在
// consumption_rate.{enable, data.rate}。
func TestParseConsumptionRate(t *testing.T) {
	cases := []struct {
		name     string
		features any
		wantRate float64
		wantOK   bool
		wantText string
	}{
		{
			name:     "正常倍率",
			features: `{"consumption_rate":{"enable":true,"data":{"rate":0.15}}}`,
			wantRate: 0.15, wantOK: true, wantText: "x0.15",
		},
		{
			name:     "enable=false 视为未知",
			features: `{"consumption_rate":{"enable":false,"data":{"rate":0.15}}}`,
			wantOK:   false,
		},
		{
			name:     "无 consumption_rate",
			features: `{"reasoning":{"enable":true}}`,
			wantOK:   false,
		},
		{
			name:     "features 非 JSON",
			features: `not json`,
			wantOK:   false,
		},
		{
			name:     "features 不是字符串（对象形态）",
			features: map[string]any{"consumption_rate": map[string]any{"enable": true}},
			wantOK:   false,
		},
		{
			name:     "rate 缺失",
			features: `{"consumption_rate":{"enable":true,"data":{}}}`,
			wantOK:   false,
		},
		{
			name:     "rate=0 如实返回（明示免费）",
			features: `{"consumption_rate":{"enable":true,"data":{"rate":0}}}`,
			wantRate: 0, wantOK: true, wantText: "x0",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rate, text := parseConsumptionRate(c.features)
			if (rate != nil) != c.wantOK {
				t.Fatalf("ok = %v, want %v", rate != nil, c.wantOK)
			}
			if !c.wantOK {
				return
			}
			if *rate != c.wantRate {
				t.Errorf("rate = %v, want %v", *rate, c.wantRate)
			}
			if text != c.wantText {
				t.Errorf("text = %q, want %q", text, c.wantText)
			}
		})
	}
}

// TestParseRemoteModels 目录行解析：展示名、上下文窗口、多模态、倍率。
func TestParseRemoteModels(t *testing.T) {
	body := map[string]any{
		"models": []any{
			map[string]any{
				"name":                  "glm-5.3",
				"display_name":          "GLM-5.3",
				"multimodal":            true,
				"context_window_tokens": map[string]any{"dev": float64(200000)},
				"features":              `{"consumption_rate":{"enable":true,"data":{"rate":0.25}}}`,
			},
			map[string]any{"name": "-"}, // 占位行应过滤
			map[string]any{"name": "no-rate-model"},
		},
	}
	got := parseRemoteModels(body)
	if len(got) != 2 {
		t.Fatalf("模型数 = %d, want 2（占位行应过滤）: %+v", len(got), got)
	}
	m, ok := got["glm-5.3"]
	if !ok {
		t.Fatal("glm-5.3 缺失")
	}
	if m.DisplayName != "GLM-5.3" || m.ContextWindow != 200000 || !m.Multimodal {
		t.Errorf("字段不符: %+v", m)
	}
	if m.CreditMultiplier == nil || *m.CreditMultiplier != 0.25 || m.RateText != "x0.25" {
		t.Errorf("倍率不符: %+v", m)
	}
	// 无倍率：nil（未知），不是 0（免费）。
	nr := got["no-rate-model"]
	if nr.CreditMultiplier != nil {
		t.Errorf("无倍率应为 nil，got %v", *nr.CreditMultiplier)
	}
}

// TestParseRemoteModelsGrouped 真实结构：data.list[].models[] 按 function 分组，
// 同模型跨组重复时去重。
func TestParseRemoteModelsGrouped(t *testing.T) {
	body := map[string]any{
		"code": float64(0),
		"data": map[string]any{
			"list": []any{
				map[string]any{
					"function": "inline_chat",
					"models": []any{
						map[string]any{
							"name":         "kimi-k2",
							"display_name": "Kimi-K2-0905",
							"features":     `{"consumption_rate":{"enable":true,"data":{"rate":0.3}}}`,
						},
					},
				},
				map[string]any{
					"function": "solo_agent",
					"models": []any{
						map[string]any{
							"name":         "kimi-k2", // 重复：应去重
							"display_name": "Kimi-K2-0905",
							"features":     `{"consumption_rate":{"enable":true,"data":{"rate":0.3}}}`,
						},
						map[string]any{
							"name":         "glm-5.3",
							"display_name": "GLM-5.3",
							"features":     `{"consumption_rate":{"enable":true,"data":{"rate":0.12}}}`,
						},
					},
				},
			},
		},
	}
	got := parseRemoteModels(body)
	if len(got) != 2 {
		t.Fatalf("模型数 = %d, want 2（跨组去重）: %+v", len(got), got)
	}
	if m := got["glm-5.3"]; m.CreditMultiplier == nil || *m.CreditMultiplier != 0.12 {
		t.Errorf("glm-5.3 倍率不符: %+v", m)
	}
	if m := got["kimi-k2"]; m.RateText != "x0.3" {
		t.Errorf("kimi-k2 倍率不符: %+v", m)
	}
}
