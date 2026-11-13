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

// directoryFunctions 需要询问的 function 列表（顺序决定归属优先级：
// 第一个列出某 config_name 的 function 拥有它）。
//
// 取自 dsh 的 TRAE_DIRECTORY_FUNCTIONS（CN 侧），去掉与对话无关的
// design/remote 分组。
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

// catalogCache 进程内缓存（多账号共用；拉取是只读接口，不区分账号所有者）。
type catalogCache struct {
	mu   sync.Mutex
	data *Catalog
}

// globalCatalogCache 全局缓存。模型目录与账号无关，进程内一份即可。
var globalCatalogCache catalogCache

// InvalidateCatalog 清空缓存（面板手动刷新 / 切换账号后可调用）。
func InvalidateCatalog() {
	globalCatalogCache.mu.Lock()
	globalCatalogCache.data = nil
	globalCatalogCache.mu.Unlock()
}

// FetchCatalog 拉取上游模型目录（带缓存）。
//
// 失败一律回退到内置目录且不返回错误——模型列表不可用不该让整个网关降级。
// 调用方可从 Catalog.Live 判断是否来自上游。
func (c *Client) FetchCatalog(ctx context.Context, cfg *Config, acc *Account, force bool) *Catalog {
	globalCatalogCache.mu.Lock()
	if !force && globalCatalogCache.data != nil &&
		time.Since(globalCatalogCache.data.FetchedAt) < catalogTTL {
		hit := globalCatalogCache.data
		globalCatalogCache.mu.Unlock()
		return hit
	}
	globalCatalogCache.mu.Unlock()

	live := c.fetchCatalogLive(ctx, cfg, acc)
	if live == nil {
		// 拉取失败：不写缓存（下次再试），直接返回内置目录。
		return builtinCatalog()
	}
	globalCatalogCache.mu.Lock()
	globalCatalogCache.data = live
	globalCatalogCache.mu.Unlock()
	return live
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

	for _, fn := range directoryFunctions {
		if ctx.Err() != nil {
			break
		}
		models, err := c.fetchDirectoryFunction(ctx, cfg, acc, fn)
		if err != nil {
			continue // 单个 function 失败不中断（新账号常有个别分组不可见）
		}
		anyOK = true
		for _, m := range models {
			if m.ID == "" || seen[m.ID].ID != "" {
				continue
			}
			seen[m.ID] = m
			order = append(order, m.ID)
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
	url := cfg.ChatBase + EndpointGetDetailParamPath
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
