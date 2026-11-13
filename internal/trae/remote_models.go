package trae

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

// remote_models.go 拉取模型目录的**展示与定价**维度。
//
// 两个目录通道分工不同（dsh 实测）：
//
//	POST /api/ide/v1/get_detail_param   → wire 名册（config_name + function 归属）
//	GET  /api/remote/v1/models?functions=… → 展示名、上下文窗口、multimodal、**积分倍率**
//
// 本文件实现后者。它与 catalog.go 的 mergeModelCatalog 合并使用：
// 前者决定"能不能调"，后者决定"多少钱"。

// EndpointRemoteModels 网页版模型目录。
const EndpointRemoteModels = "/api/remote/v1/models"

// remoteDirectoryFunctions 询问的 function 列表（与 dsh 的 CN 列表一致）。
//
// 为什么列这么多：一个模型可能只出现在某个 function 下，漏问就会缺行。
// 上游对不认识的 function 名宽容（忽略而非报错），所以多问没有代价。
var remoteDirectoryFunctions = []string{
	"solo_agent", "solo_agent_remote", "solo_work_remote", "solo_work_lite",
	"chat_v3", "solo_coder", "builder_v3",
	"chat", "inline_chat", "solo_agent_lite", "solo_design_lite", "solo_design_remote",
}

// remoteModelsTTL 目录缓存时长（与 catalog.go 的 catalogTTL 对齐）。
const remoteModelsTTL = 30 * time.Minute

// RemoteModel 目录里的一行（只保留展示与定价相关的字段）。
type RemoteModel struct {
	// ID wire 名（remote 目录的 name 字段）。
	ID string
	// DisplayName 展示名（display_name，缺失回落到 ID）。
	DisplayName string
	// ContextWindow 上下文窗口（context_window_tokens.dev）。
	ContextWindow int64
	// Multimodal 是否支持图片输入。
	Multimodal bool
	// CreditMultiplier 积分倍率（每 token 的积分倍率）。
	//
	// **指针语义**：nil = 上游未给出（未知），非 nil 且为 0 = 实测免费。
	// 这个区分很重要——把「未知」当成「免费」会让选号把收费模型当成免费层。
	// 来源：features.consumption_rate.{enable, data.rate}，features 是 JSON 字符串。
	CreditMultiplier *float64
	// RateText 倍率原文（"x0.15" 形态，与 CodeBuddy 的 credits 字段同格式）。
	// 空 = 无倍率信息。
	RateText string

	// Usage 上游对该 config 的用途标注。
	//
	// `chat_completion` = 真模型；`summary` / `title_generation` / `fast_apply` /
	// `input_optimization` 等是 IDE 自带工具；`custom_model` 是账号自带的模型。
	// 空 = 上游没说（不能据此排除，见 dsh 的"沉默不是判决"原则）。
	Usage string
	// ConfigSource 1 = Trae 预设；2 = 企业控制台配置；3 = 账号自带模型。
	// 0 = 上游未给。
	ConfigSource int64
	// Invisible 上游在其模型选择器里是否隐藏该 config。
	Invisible bool
}

// remoteCache 进程内缓存。
type remoteCache struct {
	mu   sync.Mutex
	at   time.Time
	data map[string]RemoteModel
}

var globalRemoteCache remoteCache

// InvalidateRemoteModels 清空缓存。
func InvalidateRemoteModels() {
	globalRemoteCache.mu.Lock()
	globalRemoteCache.data = nil
	globalRemoteCache.mu.Unlock()
}

// FetchRemoteModels 拉取模型目录（带缓存）。
//
// 失败返回 nil：目录是**展示增强**，拿不到不该影响对话链路。
func (c *Client) FetchRemoteModels(ctx context.Context, cfg *Config, acc *Account, force bool) map[string]RemoteModel {
	globalRemoteCache.mu.Lock()
	if !force && globalRemoteCache.data != nil && time.Since(globalRemoteCache.at) < remoteModelsTTL {
		hit := globalRemoteCache.data
		globalRemoteCache.mu.Unlock()
		return hit
	}
	globalRemoteCache.mu.Unlock()

	out := c.fetchRemoteModels(ctx, cfg, acc)
	if out == nil {
		return nil
	}
	globalRemoteCache.mu.Lock()
	globalRemoteCache.data = out
	globalRemoteCache.at = time.Now()
	globalRemoteCache.mu.Unlock()
	return out
}

// fetchRemoteModels 真正发一次请求。
func (c *Client) fetchRemoteModels(ctx context.Context, cfg *Config, acc *Account) map[string]RemoteModel {
	if cfg == nil || acc == nil || acc.AccessTokenHeader() == "" {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, catalogFetchTimeout)
	defer cancel()

	url := cfg.ChatBase + EndpointRemoteModels +
		"?functions=" + strings.Join(remoteDirectoryFunctions, "%2C") +
		"&show_custom_model=true"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil
	}
	applyChatHeaders(req, cfg, acc, acc.AccessTokenHeader())
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Origin", "https://www.trae.cn")
	req.Header.Set("Referer", "https://www.trae.cn/")

	resp, err := c.jsonHTTP.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode >= 400 {
		return nil
	}
	// 响应可能是对象 {"models":[...]} 也可能是裸数组（上游有历史变体）。
	var parsed map[string]any
	if err := json.Unmarshal(raw, &parsed); err != nil {
		var arr []any
		if err2 := json.Unmarshal(raw, &arr); err2 == nil {
			return parseRemoteModels(map[string]any{"models": arr})
		}
		log.Printf("WARN: [trae] remote 目录响应非 JSON: %s", truncateForLog(string(raw), 200))
		return nil
	}
	out := parseRemoteModels(parsed)
	if len(out) == 0 {
		// 解析为空时把顶层键打出来——目录结构变化时这是唯一线索。
		keys := make([]string, 0, len(parsed))
		for k := range parsed {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		log.Printf("WARN: [trae] remote 目录解析为空，顶层键=%v，前 300 字节=%s",
			keys, truncateForLog(string(raw), 300))
	}
	return out
}

// parseRemoteModels 从响应里提取模型行。
//
// 真实结构（2026-11 实测）是按 function 分组：
//
//	{"code":0,"data":{"list":[
//	    {"function":"inline_chat","models":[{"name":"kimi-k2", "display_name":"…",
//	        "features":"<JSON 字符串>", "context_window_tokens":{"dev":…}}, …]},
//	    {"function":"solo_agent", "models":[…]}
//	]}}
//
// 同一模型可能出现在多个 function 下，按 name 去重（首个出现的保留）。
func parseRemoteModels(body map[string]any) map[string]RemoteModel {
	// 找到模型数组：优先 data.list[].models[]（真实形态），
	// 其次顶层 models（历史/兜底变体）。
	var list []any
	if raw, ok := findPayloadField(unwrapScopes(body), "list"); ok {
		if groups, ok := raw.([]any); ok {
			for _, g := range groups {
				group, ok := g.(map[string]any)
				if !ok {
					continue
				}
				if ms, ok := group["models"].([]any); ok {
					list = append(list, ms...)
				}
			}
		}
	}
	if len(list) == 0 {
		if raw, ok := findPayloadField(unwrapScopes(body), "models"); ok {
			list, _ = raw.([]any)
		}
	}
	if len(list) == 0 {
		return nil
	}
	out := make(map[string]RemoteModel, len(list))
	for _, item := range list {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		id := strings.TrimSpace(str(m["name"]))
		if id == "" || !isUsableModelName(id) {
			continue
		}
		row := RemoteModel{ID: id, DisplayName: id}
		if dn := strings.TrimSpace(str(m["display_name"])); dn != "" {
			row.DisplayName = dn
		}
		if cw, ok := m["context_window_tokens"].(map[string]any); ok {
			row.ContextWindow = intOf(cw["dev"])
		}
		row.Multimodal = m["multimodal"] == true
		// 可调用性信号（dsh isPromotableWireModel 的三个判据）：
		//   usage           = chat_completion 才是"模型"，否则是工具配置
		//                     （summary / title_generation / fast_apply …）
		//   config_source   = 1 是 Trae 预设，3 是账号自带的自定义模型
		//   invisible       = 上游在选择器里是否展示
		row.Usage = strings.TrimSpace(str(m["usage"]))
		row.ConfigSource = intOf(m["config_source"])
		row.Invisible = m["is_invisible_to_user"] == true
		// 倍率：features 是 JSON **字符串**，需二次解析。
		if rate, text := parseConsumptionRate(m["features"]); rate != nil {
			row.CreditMultiplier = rate
			row.RateText = text
		}
		// 去重：同一模型可能在多个 function 分组下重复出现，
		// 已存在的保留（首个即可，字段内容一致）。
		if _, exists := out[id]; !exists {
			out[id] = row
		}
	}
	return out
}

// parseConsumptionRate 从 features 里提取积分倍率。
//
// 结构（dsh src/model-metadata.ts 实测）：
//
//	features = "{\"consumption_rate\":{\"enable\":true,\"data\":{\"rate\":0.15}}}"
//
// 关键判断（dsh 反复强调）：**缺失不等于免费**。只有 enable=true 且 rate 是
// 有限正数才给出倍率；其余一律返回 nil（未知）。rate=0 也要如实返回，
// 那是上游明示的免费模型。
func parseConsumptionRate(features any) (*float64, string) {
	s, ok := features.(string)
	if !ok || s == "" {
		return nil, ""
	}
	var parsed map[string]any
	if err := json.Unmarshal([]byte(s), &parsed); err != nil {
		return nil, ""
	}
	cr, ok := parsed["consumption_rate"].(map[string]any)
	if !ok || cr["enable"] != true {
		return nil, ""
	}
	data, ok := cr["data"].(map[string]any)
	if !ok {
		return nil, ""
	}
	rate, ok := toFloat(data["rate"])
	if !ok {
		return nil, ""
	}
	return &rate, fmt.Sprintf("x%.4g", rate)
}

// toFloat 宽容取浮点（上游可能给 float64 / json.Number / 字符串）。
func toFloat(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case json.Number:
		f, err := n.Float64()
		return f, err == nil
	case string:
		var f float64
		if _, err := fmt.Sscanf(strings.TrimSpace(n), "%g", &f); err == nil {
			return f, true
		}
	}
	return 0, false
}

// truncateForLog 截断长字符串供日志使用。
func truncateForLog(s string, n int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// isCallableModel 判定一个 config 是否真的是「可当模型调用」的。
//
// 规则来自 dsh 的 isPromotableWireModel（src/catalog.ts:343），三条判据都是
// 上游信号，没有臆造：
//
//  1. usage == "chat_completion" —— Remote 目录把模型和 IDE 自带工具混在一起
//     （summary / title_generation / fast_apply / input_optimization），
//     还可能列出账号自带模型（custom_model）。后两者调不通 llm_utils_chat。
//     **usage 为空时放行**：上游没说 ≠ 不是模型（沉默不是判决）。
//  2. config_source == 1（Trae 预设）或 2（企业控制台配置）。
//     3 = 账号自带模型（用户自己的 base_url/key，网关不该代为计费）。
//     0（未给）放行，理由同上。
//  3. 展示名可用：非空、非占位 "-"、不是裸 config 标识符。
//
// 不做的事：不把 is_invisible_to_user 当"已下线"。实测同一个模型在不同
// function 下一个可见一个隐藏（DeepSeek-V4-Pro 在 solo_work_remote 隐藏、
// 在 solo_coder 可见），把它读成"废弃"会误删在用模型。
func isCallableModel(m RemoteModel) bool {
	// 判据 1：usage 明说不是模型就排除。**为空必须放行**——Remote 目录的
	// usage 经常不给（实测 DeepSeek-V4-Pro / Doubao-Seed-2.0-Code 都是空），
	// 套用 wire 名册那套「usage != chat_completion 即排除」会误杀在用模型。
	if m.Usage != "" && m.Usage != "chat_completion" {
		return false
	}
	// 判据 2：config_source=3 是账号自带模型（用户自己的 base_url/key），
	// 网关不该代为计费。0（未给）与 1/2 放行。
	if m.ConfigSource != 0 && m.ConfigSource != 1 && m.ConfigSource != 2 {
		return false
	}
	name := strings.TrimSpace(m.DisplayName)
	// 判据 3a：空名与占位 "-" 不可用。
	if name == "" || name == "-" {
		return false
	}
	// 判据 3b：**裸标识符**（带下划线且无空格，如 search_agent_qwen_fast）
	// 是 config 名戴着展示名的帽子，不是模型名。
	//
	// 刻意**不**排除「展示名 == ID」：那是 wire 名册的规则（那里的 id 是
	// config_name），而 Remote 目录的 id 本身就是模型名——DeepSeek-V4-Pro
	// 的展示名与 id 相同是完全正常的，排除它会误删在用模型（实测踩过）。
	if !strings.Contains(name, " ") && strings.Contains(name, "_") {
		return false
	}
	return true
}

// IsCallableModel 导出 isCallableModel（server 层生成模型列表时用）。
func IsCallableModel(m RemoteModel) bool { return isCallableModel(m) }
