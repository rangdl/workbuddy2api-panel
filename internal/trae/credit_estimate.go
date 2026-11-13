package trae

import "math"

// credit_estimate.go 按 token 用量与模型倍率估算积分消耗。
//
// **这是估算，不是上游的计费事实。** Trae 的 chat 流里没有任何扣费字段
// （实测：token_usage 只有 prompt/completion/cache 计数，没有 credit），
// 唯一权威的来源是积分查询接口的余额差。
//
// 所以这里的值只用于「数量级参考」：日志里标成 credit≈，面板标明估算，
// 绝不参与选号的成本分层（那是 CodeBuddy 的实测台账，口径必须可信）。
//
// 公式（倍率的语义是「每 token 的积分倍率」，与 IDE 模型选择器显示的 x0.15 同源）：
//
//	credits ≈ (未命中输入 × rate) + (缓存读入 × rate × cacheDiscount)
//	          + (缓存写入 × rate × cacheWriteFactor) + (输出 × rate × outputFactor)
//
// 系数取舍说明：
//   - cacheDiscount 取 0.1：业界缓存命中通常按 1/10 计价（Anthropic/OpenAI 同量级）。
//     Trae 未公开该系数，这是**假设值**。
//   - cacheWriteFactor 取 1.25：写入缓存在部分厂商有溢价。
//   - outputFactor 取 1.0：默认输出与输入同价；多数厂商输出更贵，但 Trae 的
//     单一 rate 字段暗示它不分输入输出。
//
// 这三个系数集中在常量里，将来若能用余额差校准就改这里。

const (
	// traeCacheReadDiscount 缓存读取的计价折扣（假设值）。
	traeCacheReadDiscount = 0.1
	// traeCacheWriteFactor 缓存写入的计价系数（假设值）。
	traeCacheWriteFactor = 1.25
	// traeOutputFactor 输出 token 的计价系数（假设值）。
	traeOutputFactor = 1.0
)

// EstimateCredits 估算一次请求消耗的积分。
//
// usage 为上游 token_usage 原始 map；rate 为该模型的积分倍率。
// rate <= 0 或 usage 为空时返回 (0, false)——**不估算未知倍率**，
// 宁可空着也不要给一个假的 0（那会被误读成"免费"）。
func EstimateCredits(usage map[string]any, rate float64) (float64, bool) {
	if usage == nil || rate <= 0 {
		return 0, false
	}
	prompt := float64(intOf(usage["prompt_tokens"]))
	cacheRead := float64(intOf(usage["cache_read_input_tokens"]))
	cacheWrite := float64(intOf(usage["cache_creation_input_tokens"]))
	completion := float64(intOf(usage["completion_tokens"]))
	if prompt <= 0 && completion <= 0 {
		return 0, false
	}
	// prompt_tokens 已含缓存部分，未命中 = 总数 - 命中。
	miss := prompt - cacheRead
	if miss < 0 {
		miss = 0
	}
	total := miss*rate +
		cacheRead*rate*traeCacheReadDiscount +
		cacheWrite*rate*traeCacheWriteFactor +
		completion*rate*traeOutputFactor
	return math.Round(total*10000) / 10000, true
}

// EstimateCreditsFromUsage 同上，但接收已归一化的分量（gateway.Usage 的形态）。
func EstimateCreditsFromUsage(prompt, cacheRead, cacheWrite, completion int64, rate float64) (float64, bool) {
	if rate <= 0 || (prompt <= 0 && completion <= 0) {
		return 0, false
	}
	miss := prompt - cacheRead
	if miss < 0 {
		miss = 0
	}
	total := float64(miss)*rate +
		float64(cacheRead)*rate*traeCacheReadDiscount +
		float64(cacheWrite)*rate*traeCacheWriteFactor +
		float64(completion)*rate*traeOutputFactor
	return math.Round(total*10000) / 10000, true
}
