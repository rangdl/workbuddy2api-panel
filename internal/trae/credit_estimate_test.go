package trae

import "testing"

// TestEstimateCredits 估算公式：未命中输入 + 缓存读（打折）+ 缓存写（溢价）+ 输出。
func TestEstimateCredits(t *testing.T) {
	usage := map[string]any{
		"prompt_tokens":               float64(1000),
		"cache_read_input_tokens":     float64(800),
		"cache_creation_input_tokens": float64(0),
		"completion_tokens":           float64(100),
	}
	got, ok := EstimateCredits(usage, 0.15)
	if !ok {
		t.Fatal("应能估算")
	}
	// (1000-800)*0.15 + 800*0.15*0.1 + 0 + 100*0.15*1.0
	// = 30 + 12 + 0 + 15 = 57
	if got < 56.9 || got > 57.1 {
		t.Errorf("估算 = %v, want ~57", got)
	}
}

// TestEstimateCreditsUnknownRate 未知倍率不估算——给 0 会被误读成免费。
func TestEstimateCreditsUnknownRate(t *testing.T) {
	usage := map[string]any{"prompt_tokens": float64(100), "completion_tokens": float64(10)}
	for _, rate := range []float64{0, -1} {
		if _, ok := EstimateCredits(usage, rate); ok {
			t.Errorf("rate=%v 不应估算", rate)
		}
	}
	if _, ok := EstimateCredits(nil, 0.15); ok {
		t.Error("无 usage 不应估算")
	}
}

// TestEstimateCreditsZeroTokens 全 0 token 不估算。
func TestEstimateCreditsZeroTokens(t *testing.T) {
	usage := map[string]any{"prompt_tokens": float64(0), "completion_tokens": float64(0)}
	if _, ok := EstimateCredits(usage, 0.15); ok {
		t.Error("零 token 不应估算")
	}
}

// TestEstimateCacheCheaper 同样的 token 数，命中缓存后估算应更低。
func TestEstimateCacheCheaper(t *testing.T) {
	noCache := map[string]any{"prompt_tokens": float64(1000), "completion_tokens": float64(100)}
	withCache := map[string]any{
		"prompt_tokens": float64(1000), "cache_read_input_tokens": float64(900),
		"completion_tokens": float64(100),
	}
	a, _ := EstimateCredits(noCache, 0.15)
	b, _ := EstimateCredits(withCache, 0.15)
	if b >= a {
		t.Errorf("命中缓存应更便宜: 无缓存 %v, 有缓存 %v", a, b)
	}
}

// TestEstimateCreditsFromUsage 分量形态与 map 形态结果一致。
func TestEstimateCreditsFromUsage(t *testing.T) {
	a, _ := EstimateCredits(map[string]any{
		"prompt_tokens": float64(500), "cache_read_input_tokens": float64(200),
		"cache_creation_input_tokens": float64(50), "completion_tokens": float64(80),
	}, 0.2)
	b, _ := EstimateCreditsFromUsage(500, 200, 50, 80, 0.2)
	if a != b {
		t.Errorf("两种形态不一致: %v vs %v", a, b)
	}
}
