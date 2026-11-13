package trae

import (
	"context"
	"errors"
	"fmt"

	"github.com/linguo2625469/workbuddy2api-panel/internal/gateway"
	traepkg "github.com/linguo2625469/workbuddy2api-panel/internal/trae"
)

// Backend Trae 上游实现。
type Backend struct {
	Client *traepkg.Client
	// ConfigFn 返回当前生效配置（热重载后即时生效）。
	ConfigFn func() *traepkg.Config
}

// New 构造 backend。
func New(c *traepkg.Client, cfgFn func() *traepkg.Config) *Backend {
	return &Backend{Client: c, ConfigFn: cfgFn}
}

func (b *Backend) Name() string { return Name }

func (b *Backend) Aliases() []string { return nil }

func (b *Backend) Enabled() bool {
	cfg := b.config()
	return cfg != nil && cfg.Enabled && b.Client != nil
}

func (b *Backend) config() *traepkg.Config {
	if b.ConfigFn == nil {
		return nil
	}
	return b.ConfigFn()
}

func (b *Backend) Accounts() ([]gateway.Account, error) {
	cfg := b.config()
	if cfg == nil {
		return nil, nil
	}
	return Wrap(cfg), nil
}

// Supports 该账号能否服务这个模型。
//
// Trae 的准入由 modelPairs（内置映射）+ 上游目录共同决定：
// 先在 modelPairs 里查，查不到再看目录里有没有该 config_name。
// 两者都未命中时**拒绝**——Trae 对未收录模型会回 4001，提前排除比让
// 上游拒绝更省额度。
func (b *Backend) Supports(acc gateway.Account, bareModel string) bool {
	if acc == nil || bareModel == "" {
		return true
	}
	a := Unwrap(acc)
	if a == nil || b.Client == nil {
		return false
	}
	if _, _, ok := traepkg.ModelPair(bareModel); ok {
		return true
	}
	cfg := b.config()
	if cfg == nil {
		return false
	}
	// Remote 目录的判据更严：要过 isCallableModel（usage/config_source/展示名）。
	// 该模型在目录里存在但不可调用（工具配置 / 账号自带模型）时应拒绝——
	// 否则会选中它再被上游 4001 拒绝，白烧一次额度。
	if remote := b.Client.FetchRemoteModels(context.Background(), cfg, a, false); remote != nil {
		if row, ok := remote[bareModel]; ok {
			return traepkg.IsCallableModel(row)
		}
	}
	// Remote 未覆盖时回落 wire 名册（get_detail_param）。
	cat := b.Client.FetchCatalog(context.Background(), cfg, a, false)
	if cat == nil || !cat.Live {
		return false
	}
	return cat.FunctionForConfigName(bareModel) != ""
}

// WireModel 客户端模型名 → Trae 的 config_name。
func (b *Backend) WireModel(acc gateway.Account, bareModel string) string {
	if cfgName, _, ok := traepkg.ModelPair(bareModel); ok {
		return cfgName
	}
	return bareModel
}

// Chat 发一次请求。
func (b *Backend) Chat(ctx context.Context, acc gateway.Account, in *gateway.Inbound) (*gateway.Outbound, error) {
	a := Unwrap(acc)
	if a == nil {
		return nil, errors.New("trae: account is not a Trae account")
	}
	cfg := b.config()
	if cfg == nil {
		return nil, errors.New("trae: config not loaded")
	}

	body, err := traepkg.PrepareChatBody(in.Raw, traepkg.PrepareOptions{
		UID:            a.UID,
		DeviceID:       a.DeviceID,
		MachineID:      a.MachineID,
		IDEVersion:     cfg.IDEVersion,
		IDEVersionCode: cfg.IDEVersionCode,
		AppID:          cfg.AppID,
		Function:       b.functionFor(cfg, a, in.Ref.Bare),
	})
	if err != nil {
		return nil, fmt.Errorf("trae: prepare body: %w", err)
	}

	rc, status, respBody, err := b.Client.ChatStream(ctx, cfg, a, body)
	if err != nil {
		return nil, err
	}
	if status >= 400 {
		cls := traepkg.Classify(status, respBody, 0, "")
		if rc != nil {
			rc.Close()
		}
		return nil, &gateway.Failure{
			Decision:   Decide(cls),
			HTTPStatus: status,
			Body:       respBody,
		}
	}
	return &gateway.Outbound{
		Status: status,
		Stream: rc,
		Convert: func(emit func(frame []byte) error) (gateway.StreamStats, error) {
			// Trae 的自定义 SSE 必须整体重写（CodeBuddy 只需帧规范化）。
			res, err := traepkg.ConvertStream(rc, in.ChatID, in.Ref.Bare, emit)
			stats := gateway.StreamStats{
				SentAny:      res.SentAny,
				ErrorCode:    res.ErrorCode,
				ErrorMsg:     res.ErrorMsg,
				FinishReason: "",
			}
			if res.Usage != nil {
				u := &gateway.Usage{
					PromptTokens:     intOf(res.Usage["prompt_tokens"]),
					CompletionTokens: intOf(res.Usage["completion_tokens"]),
					TotalTokens:      intOf(res.Usage["total_tokens"]),
					CacheRead:        intOf(res.Usage["cache_read_input_tokens"]),
					CacheCreation:    intOf(res.Usage["cache_creation_input_tokens"]),
					Reasoning:        intOf(res.Usage["reasoning_tokens"]),
				}
				stats.Usage = u
			}
			if res.HasRemain {
				stats.Remain = &gateway.Remain{
					Available: res.RemainIDE, // 对话只消耗 IDE 积分，不含 Work
					Work:      res.RemainWork,
					Raw:       res.RemainRaw,
				}
			}
			return stats, err
		},
	}, nil
}

// Credits 查询余额。
//
// 关键：Available 必须取 CreditsSummary.General（product_id != 209），
// **不能**用 Total——Total 含 Work 积分，而对话只消耗 IDE 额度，
// 用 Total 会让快耗尽的账号看起来健康，选号权重随之失真。
func (b *Backend) Credits(ctx context.Context, acc gateway.Account) (*gateway.Credits, error) {
	a := Unwrap(acc)
	if a == nil {
		return nil, errors.New("trae: account is not a Trae account")
	}
	cfg := b.config()
	if cfg == nil {
		return nil, errors.New("trae: config not loaded")
	}
	sum, err := b.Client.QueryCredits(ctx, cfg, a)
	if err != nil {
		return nil, err
	}
	out := &gateway.Credits{
		Available: sum.General,
		Limit:     sum.Limit,
	}
	// 到期时间：取最早一批仍有剩余的包（CreditPack.EndTime 是上游原样字符串）。
	for _, p := range sum.Packs {
		if p.IsWork() || p.Remaining() <= 0 {
			continue
		}
		if t, ok := parsePackTime(p.EndTime); ok {
			if out.Earliest.IsZero() || t.Before(out.Earliest) {
				out.Earliest = t
			}
		}
	}
	return out, nil
}

// RetryHook Trae 没有内容拦截降级重试。
func (b *Backend) RetryHook() gateway.RetryHook { return nil }

// functionFor 查模型对应的上游 function（目录优先，内置表兜底）。
func (b *Backend) functionFor(cfg *traepkg.Config, a *traepkg.Account, bareModel string) string {
	if bareModel == "" || b.Client == nil {
		return ""
	}
	cat := b.Client.FetchCatalog(context.Background(), cfg, a, false)
	if cat == nil || !cat.Live {
		return ""
	}
	return cat.FunctionForConfigName(bareModel)
}

// intOf 宽容取整（上游可能给 float64 / json.Number / 字符串）。
func intOf(v any) int64 {
	switch n := v.(type) {
	case float64:
		return int64(n)
	case int64:
		return n
	case int:
		return int64(n)
	default:
		return 0
	}
}

var _ gateway.Backend = (*Backend)(nil)
