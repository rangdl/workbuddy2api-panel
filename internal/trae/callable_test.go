package trae

import "testing"

// TestIsCallableModel 可调用性三判据（源自 dsh isPromotableWireModel）。
func TestIsCallableModel(t *testing.T) {
	cases := []struct {
		name string
		m    RemoteModel
		want bool
	}{
		{
			name: "真模型",
			m:    RemoteModel{ID: "glm-5.3", DisplayName: "GLM-5.3", Usage: "chat_completion", ConfigSource: 1},
			want: true,
		},
		{
			name: "工具配置（summary）应排除",
			m:    RemoteModel{ID: "summary", DisplayName: "Summary", Usage: "summary", ConfigSource: 1},
			want: false,
		},
		{
			name: "fast_apply 工具应排除",
			m:    RemoteModel{ID: "fast_apply", DisplayName: "Fast Apply", Usage: "fast_apply", ConfigSource: 1},
			want: false,
		},
		{
			name: "账号自带模型应排除（不该代为计费）",
			m:    RemoteModel{ID: "my-model", DisplayName: "My Model", Usage: "custom_model", ConfigSource: 3},
			want: false,
		},
		{
			name: "config_source=3 即使 usage 正常也排除",
			m:    RemoteModel{ID: "x", DisplayName: "X", Usage: "chat_completion", ConfigSource: 3},
			want: false,
		},
		{
			name: "企业控制台配置（2）放行",
			m:    RemoteModel{ID: "corp-model", DisplayName: "Corp", Usage: "chat_completion", ConfigSource: 2},
			want: true,
		},
		{
			name: "usage 为空放行（沉默不是判决）",
			m:    RemoteModel{ID: "kimi-k2.8-preview", DisplayName: "Kimi K2.8", ConfigSource: 1},
			want: true,
		},
		{
			name: "config_source 未给放行",
			m:    RemoteModel{ID: "a", DisplayName: "A", Usage: "chat_completion"},
			want: true,
		},
		{
			name: "占位展示名应排除",
			m:    RemoteModel{ID: "x", DisplayName: "-", Usage: "chat_completion", ConfigSource: 1},
			want: false,
		},
		{
			name: "空展示名应排除",
			m:    RemoteModel{ID: "x", DisplayName: "", Usage: "chat_completion", ConfigSource: 1},
			want: false,
		},
		{
			name: "裸下划线标识符应排除（config 名戴展示名帽子）",
			m:    RemoteModel{ID: "search_agent_qwen_fast", DisplayName: "search_agent_qwen_fast", Usage: "chat_completion", ConfigSource: 1},
			want: false,
		},
		{
			// Remote 目录的 id 本身就是模型名，展示名与之相同很正常。
			// 实测误判过：DeepSeek-V4-Pro 因此被误删。
			name: "展示名等于 ID 但格式正常应放行",
			m:    RemoteModel{ID: "DeepSeek-V4-Pro", DisplayName: "DeepSeek-V4-Pro", ConfigSource: 1},
			want: true,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := IsCallableModel(c.m); got != c.want {
				t.Errorf("IsCallableModel = %v, want %v (%+v)", got, c.want, c.m)
			}
		})
	}
}

// TestParseRemoteModelsCapturesCallabilitySignals 目录行要带上三个判据字段。
func TestParseRemoteModelsCapturesCallabilitySignals(t *testing.T) {
	body := map[string]any{
		"data": map[string]any{"list": []any{
			map[string]any{"function": "chat_v3", "models": []any{
				map[string]any{
					"name": "glm-5.3", "display_name": "GLM-5.3",
					"usage": "chat_completion", "config_source": float64(1),
					"is_invisible_to_user": true,
				},
			}},
		}},
	}
	got := parseRemoteModels(body)
	m, ok := got["glm-5.3"]
	if !ok {
		t.Fatal("应解析出 glm-5.3")
	}
	if m.Usage != "chat_completion" || m.ConfigSource != 1 || !m.Invisible {
		t.Errorf("可调用性信号未解析: %+v", m)
	}
}
