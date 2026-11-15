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

// 扩展上下文：只有 max_mode=true 时才认 context_window_tokens.max。
// 上游对多数模型都下发 max 数值，不开 max_mode 就不按它服务——照抄会虚报窗口。
func TestParseRemoteModelsMaxContext(t *testing.T) {
	body := map[string]any{
		"models": []any{
			map[string]any{
				"name":                  "with-max",
				"max_mode":              true,
				"context_window_tokens": map[string]any{"dev": float64(128000), "max": float64(1000000)},
			},
			map[string]any{
				"name":                  "without-max",
				"context_window_tokens": map[string]any{"dev": float64(128000), "max": float64(1000000)},
			},
		},
	}
	got := parseRemoteModels(body)
	if m := got["with-max"]; m.MaxContextWindow != 1000000 || m.ContextWindow != 128000 {
		t.Errorf("max_mode=true 应同时给出扩展与基础窗口: %+v", m)
	}
	if m := got["without-max"]; m.MaxContextWindow != 0 || m.ContextWindow != 128000 {
		t.Errorf("max_mode 缺失时不应认 max 数值: %+v", m)
	}
}

// 思考档位：上游 wire 名要翻回网关档位名；未知名字丢弃而不是臆造。
func TestParseRemoteModelsReasoning(t *testing.T) {
	body := map[string]any{
		"models": []any{
			map[string]any{
				"name":     "thinker",
				"features": `{"reasoning":{"enable":true}}`,
				"reasoning_effort_config": map[string]any{
					"options":       []any{"light", "high", "extra_high"},
					"default_level": "high",
				},
			},
			map[string]any{
				// 上游给了未知名：必须丢弃，不能当作档位透出。
				"name":     "odd-efforts",
				"features": `{"reasoning":{"enable":false}}`,
				"reasoning_effort_config": map[string]any{
					"options":       []any{"turbo", "extra_high"},
					"default_level": "turbo",
				},
			},
			map[string]any{"name": "no-reasoning-block"},
		},
	}
	got := parseRemoteModels(body)

	th := got["thinker"]
	if !th.ReasoningSupported {
		t.Error("thinker: reasoning.enable=true 应标为支持")
	}
	wantEfforts := []string{"low", "high", "xhigh"}
	if len(th.ReasoningEfforts) != len(wantEfforts) {
		t.Fatalf("thinker 档位 = %v, want %v", th.ReasoningEfforts, wantEfforts)
	}
	for i, w := range wantEfforts {
		if th.ReasoningEfforts[i] != w {
			t.Errorf("thinker 档位[%d] = %q, want %q（全量 %v）", i, th.ReasoningEfforts[i], w, th.ReasoningEfforts)
		}
	}
	if th.ReasoningDefault != "high" {
		t.Errorf("thinker 默认档 = %q, want high", th.ReasoningDefault)
	}

	odd := got["odd-efforts"]
	if odd.ReasoningSupported {
		t.Error("odd-efforts: reasoning.enable=false 不应标为支持")
	}
	if len(odd.ReasoningEfforts) != 1 || odd.ReasoningEfforts[0] != "xhigh" {
		t.Errorf("未知档位应被丢弃，只留 xhigh: %v", odd.ReasoningEfforts)
	}
	if odd.ReasoningDefault != "" {
		t.Errorf("默认档 turbo 未识别，应留空，got %q", odd.ReasoningDefault)
	}

	none := got["no-reasoning-block"]
	if none.ReasoningSupported || len(none.ReasoningEfforts) != 0 || none.ReasoningDefault != "" {
		t.Errorf("无 reasoning 段时应保持零值: %+v", none)
	}
}

// 上游档位名与网关档位名必须往返一致：payload.go 负责正向（发出去），
// 这里负责反向（展示）。展示层只能列**真能送达**的档位——payload 是多对一
// 映射（medium→high、max→extra_high），这两个档位即便上游公布也发不出去，
// 标成「支持」就是骗用户。
func TestWireEffortMapIsInverseOfPayload(t *testing.T) {
	for wire, gw := range wireEffortToGateway {
		back, ok := reasoningEffortMap[gw]
		if !ok {
			t.Errorf("网关档位 %q 在 payload 的 reasoningEffortMap 里缺失", gw)
			continue
		}
		if back != wire {
			t.Errorf("往返不一致：wire %q → gateway %q → payload %q（应为 %q）——"+
				"展示会列出实际发送时被静默降级的档位", wire, gw, back, wire)
		}
	}
	// 反向检查：payload 能接受的每个档位，要么在展示表里有来源，要么是已知的
	// 降级项。降级项允许存在（payload 需要容忍客户端的各种写法），但展示层
	// 绝不能把它们列为可选档位。
	downgraded := map[string]bool{"minimal": true, "medium": true, "max": true}
	shown := make(map[string]bool, len(wireEffortToGateway))
	for _, gw := range wireEffortToGateway {
		shown[gw] = true
	}
	for gw := range reasoningEffortMap {
		if shown[gw] || downgraded[gw] {
			continue
		}
		t.Errorf("payload 能发出档位 %q，展示表里却没有来源，也不在已知降级项里", gw)
	}
	// 降级项必须真的被降级（否则这个白名单会掩盖未来的映射修正）。
	for gw := range downgraded {
		if wire, ok := reasoningEffortMap[gw]; ok {
			if back, hit := wireEffortToGateway[wire]; hit && back == gw {
				t.Errorf("%q 已是精确映射（→ %q），应从 downgraded 白名单移除", gw, wire)
			}
		}
	}
}
