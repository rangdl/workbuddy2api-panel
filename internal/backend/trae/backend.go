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
