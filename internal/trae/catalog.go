// catalog.go 上游模型目录拉取。
//
// 本地早先只返回硬编码的 24 个模型：新模型上架必须改代码发版，而且不知道
// 某个模型属于哪个 function —— 而 function 归属直接决定上游会不会回 4001
// （「该 config_name 不被这个 function 服务」）。
//
// 两个目录通道（dsh 实测）：
//
//	POST /api/ide/v1/get_detail_param      → wire 名册：config_name + function 归属
//	                                          + model_detail_list[].max_tokens
//	GET  /api/remote/v1/models?functions=… → 展示名、上下文窗口、multimodal
//
// 代理只需要前者（wire id 与准入），后者用于展示增强。这里实现前者 +
// 内置目录兜底：拉取失败时行为与旧版完全一致。
package trae

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

// EndpointGetDetailParam 模型名册（wire id + function 归属）。
const EndpointGetDetailParamPath = "/api/ide/v1/get_detail_param"

// catalogTTL 模型目录缓存时长。目录变化很慢，但新模型上架后用户希望尽快可见。
const catalogTTL = 30 * time.Minute

// catalogFetchTimeout 单次目录拉取的超时。
const catalogFetchTimeout = 20 * time.Second

// directoryFunctions 目录查询要问的 function 列表（**顺序即归属优先级**：
// 第一个列出某 config_name 的 function 拥有它）。
//
// 这张表**只用于「有哪些模型」**。聊天调用该用哪个 function 由下面
// chatFunctions 决定——两者不是同一张表，混用会让模型「能看见但调不动」。
//
// 顺序的第一位是 solo_work_lite：它是默认聊天通道，让共享 config 归属它
// 才能保证「目录里看到的模型就是聊天能用的函数」。
// solo_agent 排第二仅作补全：它列的条目最多（国际侧 7/7 覆盖），但它**不能
// 聊天**，所以 chatFunctions 里没有它——见那个变量的注释。
var directoryFunctions = []string{
	FunctionSoloWorkLite,
	FunctionSoloAgent,
	"chat_v3",
	"chat",
	"inline_chat",
	"solo_agent_remote",
	"solo_work_remote",
	"solo_agent_lite",
	"solo_coder",
	"solo_builder",
	"builder",
	"builder_v3",
	"ui_builder_v2",
	"multimodal",
	"utils",
}

// chatFunctions 真正能承载 llm_utils_chat 的 function，按优先级排序。
//
// **solo_agent 不在其中**，这是实测结论（dsh 2026-10-01，issue #19）：
// 它是 Remote-agent 名册函数，不是聊天函数。任何模型用
// function:"solo_agent" 发出去都会回 `4011`，而同一模型同一 body 换成
// solo_work_remote / solo_work_lite / solo_agent_remote 就正常。
//
// 它被排除在**聊天**之外但不排除在**目录**之外是刻意的：
// get_detail_param 用它问能拿到最全的条目（国际侧 7/7），是 wire 名的好来源；
// 只是拿到之后必须由这张表重新决定"用哪个函数调用"。
//
// solo_agent_remote 带 Remote plan 门控，排在两个 IDE 函数之后；
// chat_v3 最宽（国际 40 条）排最后，只补空缺不抢优先级。
var chatFunctions = map[ServiceRegion][]string{
	RegionCN: {
		"solo_work_remote", FunctionSoloWorkLite, "solo_agent_remote",
		"chat_v3", "solo_coder",
	},
	RegionGlobal: {
		"solo_work_remote", FunctionSoloWorkLite, "solo_agent_remote", "chat_v3",
	},
}

// chatFunctionOrder 某域的聊天 function 优先级。
func chatFunctionOrder(region ServiceRegion) []string {
	if fns, ok := chatFunctions[region]; ok {
		return fns
	}
	return chatFunctions[RegionCN]
}

// CatalogModel 一个模型条目。
type CatalogModel struct {
	// ID wire id（config_name），对话请求用这个。
	ID string `json:"id"`
	// DisplayName 展示名（上游给的 display_name，缺失时等于 ID）。
	DisplayName string `json:"display_name"`
	// Function 该模型所属的 function（决定请求体 function 字段）。
	Function string `json:"function"`
	// MaxTokens 上游公布的单次输出上限（0 = 未知）。
	MaxTokens int64 `json:"max_tokens,omitempty"`
	// PromptMaxTokens 上下文窗口（0 = 未知）。
	PromptMaxTokens int64 `json:"prompt_max_tokens,omitempty"`
}

// Catalog 模型目录快照。
type Catalog struct {
	Models []CatalogModel
	// FetchedAt 拉取时刻（零值 = 来自内置目录）。
	FetchedAt time.Time
	// Live 是否来自上游（false = 内置兜底）。
	Live bool
}

// catalogCache 进程内缓存，**按域分表**。
//
// 为什么必须分域：国际版与国内版的模型阵容基本不重叠（国内 GLM/Kimi/DeepSeek，
// 国际 Gemini/GPT/MiniMax）。改造前这里是一份全局单例，注释写「模型目录与账号
// 无关」——那句话只在单域时代成立。混用会让国内账号看到国际目录、反之亦然。
//
// 键用域而不是账号：同一域内不同账号看到的目录相同（目录是上游级数据），
// 一份就够。
type catalogCache struct {
	mu   sync.Mutex
	data map[string]*Catalog
}

// globalCatalogCache 全局缓存。按域分表，进程内一份即可。
var globalCatalogCache = catalogCache{data: make(map[string]*Catalog, 2)}

// InvalidateCatalog 清空缓存（面板手动刷新 / 切换账号后可调用）。
func InvalidateCatalog() {
	globalCatalogCache.mu.Lock()
	globalCatalogCache.data = make(map[string]*Catalog, 2)
	globalCatalogCache.mu.Unlock()
}

// FetchCatalog 拉取上游模型目录（带缓存，按账号所属域分表）。
//
// 失败一律回退到内置目录且不返回错误——模型列表不可用不该让整个网关降级。
// 调用方可从 Catalog.Live 判断是否来自上游。
func (c *Client) FetchCatalog(ctx context.Context, cfg *Config, acc *Account, force bool) *Catalog {
	key := catalogCacheKey(cfg, acc)
	globalCatalogCache.mu.Lock()
	if !force {
		if hit, ok := globalCatalogCache.data[key]; ok && hit != nil &&
			time.Since(hit.FetchedAt) < catalogTTL {
			globalCatalogCache.mu.Unlock()
			return hit
		}
	}
	globalCatalogCache.mu.Unlock()

	live := c.fetchCatalogLive(ctx, cfg, acc)
	if live == nil {
		// 拉取失败：不写缓存（下次再试），直接返回内置目录。
		return builtinCatalog()
	}
	globalCatalogCache.mu.Lock()
	globalCatalogCache.data[key] = live
	globalCatalogCache.mu.Unlock()
	return live
}

// catalogCacheKey 目录缓存的分表键（账号所属域）。
func catalogCacheKey(cfg *Config, acc *Account) string {
	if acc == nil {
		return string(RegionCN)
	}
	return string(acc.RegionOf(cfg))
}

// fetchCatalogLive 逐个 function 询问 get_detail_param 并合并。
func (c *Client) fetchCatalogLive(ctx context.Context, cfg *Config, acc *Account) *Catalog {
	if cfg == nil || acc == nil || acc.AccessTokenHeader() == "" {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, catalogFetchTimeout)
	defer cancel()

	// config_name → CatalogModel；先到先得（第一个列出它的 function 拥有它）。
	seen := make(map[string]CatalogModel, 64)
	order := make([]string, 0, 64)
	anyOK := false
	region := RegionCN
	if acc != nil {
		region = acc.RegionOf(cfg)
	}
	chatSet := make(map[string]int, 8) // function → 聊天优先级（越小越优先）
	for i, fn := range chatFunctionOrder(region) {
		chatSet[fn] = i
	}

	for _, fn := range directoryFunctions {
		if ctx.Err() != nil {
			break
		}
		models, err := c.fetchDirectoryFunction(ctx, cfg, acc, fn)
		if err != nil {
			continue // 单个 function 失败不中断（新账号常有个别分组不可见）
		}
		anyOK = true
		chatRank, chatable := chatSet[fn]
		for _, m := range models {
			if m.ID == "" {
				continue
			}
			cur, exists := seen[m.ID]
			if !exists {
				// 首次见到：先记下来（归属暂时是这个 function）。
				seen[m.ID] = m
				order = append(order, m.ID)
				if chatable {
					m.Function = fn
					seen[m.ID] = m
				}
				continue
			}
			// 已见过：只有「当前归属不能聊天、而这个 function 能聊天」时才改写。
			//
			// 为什么必须改写：目录查询阶段先到先得的归属可能是 solo_agent
			// 这类**不能聊天**的函数（它列得最全），直接拿去发 llm_utils_chat
			// 会得到 4011。归属必须收敛到某个真正能承载聊天的 function。
			if !chatable {
				continue
			}
			if prevRank, prevChatable := chatSet[cur.Function]; prevChatable && prevRank <= chatRank {
				continue
			}
			cur.Function = fn
			seen[m.ID] = cur
		}
	}
	if !anyOK {
		return nil
	}
	out := &Catalog{Models: make([]CatalogModel, 0, len(order)), FetchedAt: time.Now(), Live: true}
	for _, id := range order {
		out.Models = append(out.Models, seen[id])
	}
	sort.Slice(out.Models, func(i, j int) bool { return out.Models[i].ID < out.Models[j].ID })
	return out
}

// fetchDirectoryFunction 问一个 function 的模型清单。
func (c *Client) fetchDirectoryFunction(ctx context.Context, cfg *Config, acc *Account, fn string) ([]CatalogModel, error) {
	payload := map[string]any{
		"function":            fn,
		"config_names":        nil,
		"need_prompt":         false,
		"current_config_info": nil,
		"poly_prompt":         true,
		"mode_type":           nil,
		"agent_type":          nil,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	url := cfg.chatBaseForAccount(acc) + EndpointGetDetailParamPath
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	applyChatHeaders(req, cfg, acc, acc.AccessTokenHeader())
	req.Header.Set("Accept", "application/json")

	resp, err := c.jsonHTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBodyBytes))
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	var parsed map[string]any
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, fmt.Errorf("响应非 JSON")
	}
	if code := intOf(parsed["code"]); code != 0 {
		return nil, fmt.Errorf("业务错误 code=%d", code)
	}
	return parseDirectoryModels(parsed, fn), nil
}

// parseDirectoryModels 从单次 get_detail_param 响应里提取模型行。
//
// 真实结构（dsh / bluechonk 实测）：config_info_list[]，每行含
// config_name / display_config.display_name / model_detail_list[]。
func parseDirectoryModels(body map[string]any, fn string) []CatalogModel {
	raw, ok := findPayloadField(unwrapScopes(body), "config_info_list")
	if !ok {
		return nil
	}
	list, ok := raw.([]any)
	if !ok {
		return nil
	}
	out := make([]CatalogModel, 0, len(list))
	for _, item := range list {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		id := strings.TrimSpace(str(m["config_name"]))
		if id == "" || !isUsableModelName(id) {
			continue
		}
		entry := CatalogModel{ID: id, DisplayName: id, Function: fn}
		if dc, ok := m["display_config"].(map[string]any); ok {
			if dn := strings.TrimSpace(str(dc["display_name"])); dn != "" {
				entry.DisplayName = dn
			}
		}
		// 输出上限与上下文窗口在 model_detail_list 里。
		if dl, ok := m["model_detail_list"].([]any); ok && len(dl) > 0 {
			if d, ok := dl[0].(map[string]any); ok {
				entry.MaxTokens = intOf(d["max_tokens"])
				entry.PromptMaxTokens = intOf(d["prompt_max_tokens"])
			}
		}
		if entry.PromptMaxTokens == 0 {
			if cw, ok := m["context_window_tokens"].(map[string]any); ok {
				entry.PromptMaxTokens = intOf(cw["dev"])
			}
		}
		out = append(out, entry)
	}
	return out
}

// isUsableModelName 过滤掉占位/不可调用的行。
//
// dsh 的实测：`-`、纯下划线标识符、空串都在名册里出现过，但都不能调。
func isUsableModelName(id string) bool {
	if id == "" || id == "-" {
		return false
	}
	// 纯下划线（如 "__"）视为占位。
	if strings.Trim(id, "_") == "" {
		return false
	}
	return true
}

// builtinCatalog 内置目录兜底（拉取失败时使用，行为与旧版一致）。
func builtinCatalog() *Catalog {
	ids := KnownModels()
	out := &Catalog{Models: make([]CatalogModel, 0, len(ids))}
	for _, id := range ids {
		out.Models = append(out.Models, CatalogModel{
			ID:          id,
			DisplayName: id,
			Function:    FunctionForModel(strings.ToLower(id)),
		})
	}
	return out
}

// FunctionForConfigName 从目录里查某个 wire id 对应的 function。
// 查不到返回空串，调用方回落到内置规则。
func (cat *Catalog) FunctionForConfigName(id string) string {
	if cat == nil {
		return ""
	}
	for _, m := range cat.Models {
		if m.ID == id {
			return m.Function
		}
	}
	return ""
}
