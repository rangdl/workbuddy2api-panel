package server

import (
	"context"
	"sort"

	backendtrae "github.com/linguo2625469/workbuddy2api-panel/internal/backend/trae"
	"github.com/linguo2625469/workbuddy2api-panel/internal/trae"
	"log"
)

// trae_models.go 把 Trae 的模型并入统一的 /v1/models 列表。
//
// 为什么并进同一份而不是单独一个端点：客户端只认一份模型列表，分两处会让
// 「这个模型多少钱」需要跨接口对照。倍率统一放在 credits 字段（"x0.15"），
// 与 CodeBuddy 的格式完全一致。

// traeModelEntries 生成 Trae 的模型条目。
//
// 输出两个 ID：
//   - "trae:cn:<模型>"  —— 显式指定上游（推荐，路由确定）
//   - "<模型>"          —— 裸名（跨上游统一调度，可能落到 CodeBuddy 或 Trae）
//
// 只列裸名的话客户端无法保证走 Trae；只列显式名则享受不到跨上游回退。
func (h *Handler) traeModelEntries() []map[string]any {
	be := h.traeBackend()
	if be == nil {
		return nil
	}
	cfg := h.traeConfig()
	if cfg == nil {
		return nil
	}
	acc := h.firstTraeAccount(cfg)
	if acc == nil {
		return nil
	}
	// remote 目录给展示名、上下文窗口与**倍率**；拉不到时回落内置模型表
	// （至少让客户端知道有哪些模型，只是没有定价）。
	remote := h.cfg.TraeClient.FetchRemoteModels(context.Background(), cfg, acc, false)

	ids := make([]string, 0, len(remote))
	if len(remote) > 0 {
		for id, row := range remote {
			// 只列真正可当模型调用的 config：Remote 目录把模型和各路 IDE
			// 工具配置混在一起，直接全列会让客户端看到一堆积木名。
			// 判据见 trae.isCallableModel（源自 dsh 的实测规则）。
			if !trae.IsCallableModel(row) {
				continue
			}
			ids = append(ids, id)
		}
	} else {
		ids = trae.KnownModels()
	}
	sort.Strings(ids)
	// 摘要：目录总行数与可调用数，便于观察过滤强度。
	if len(remote) > 0 {
		log.Printf("[trae] 模型目录：上游 %d 行，可调用 %d 个", len(remote), len(ids))
	}

	out := make([]map[string]any, 0, len(ids)*2)
	for _, id := range ids {
		row := remote[id]
		display := row.DisplayName
		if display == "" {
			display = id
		}
		// 两个 ID 形态共用同一份元数据。
		for _, wireID := range []string{"trae:" + defaultTraeRealm + ":" + id, id} {
			entry := map[string]any{
				"id":       wireID,
				"object":   "model",
				"created":  1753600000,
				"owned_by": "trae",
				"name":     display,
			}
			if row.ContextWindow > 0 {
				entry["context_length"] = row.ContextWindow
			}
			if row.Multimodal {
				entry["supports_images"] = true
			}
			// 倍率：与 CodeBuddy 同格式（"x0.15"）。
			// 只有上游明示（enable=true 且 rate 有效）才写——缺失表示未知，
			// 不能当成免费（dsh account-pool.ts 的教训）。
			if row.RateText != "" {
				entry["credits"] = row.RateText
			}
			out = append(out, entry)
		}
	}
	return out
}

// defaultTraeRealm 当前 Trae 的默认域（本仓库只实现 CN）。
const defaultTraeRealm = "cn"

// traeBackend 返回 Trae 的 backend 实现（未注册/未启用返回 nil）。
func (h *Handler) traeBackend() *backendtrae.Backend {
	for _, b := range h.enabledBackends() {
		if tb, ok := b.(*backendtrae.Backend); ok {
			return tb
		}
	}
	return nil
}

// traeConfig 当前 Trae 配置（热重载后即时生效；可能为 nil）。
func (h *Handler) traeConfig() *trae.Config {
	return h.traeCfg.Load()
}

// firstTraeAccount 取一个可用的 Trae 账号（拉目录只需任意一个）。
//
// 优先未禁用的；全部禁用时返回 nil（目录拉不到就回落内置模型表）。
func (h *Handler) firstTraeAccount(cfg *trae.Config) *trae.Account {
	if cfg == nil {
		return nil
	}
	for i := range cfg.Accounts {
		a := &cfg.Accounts[i]
		if a.Disabled || a.AccessToken == "" {
			continue
		}
		return a
	}
	return nil
}
