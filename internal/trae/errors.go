package trae

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"
)

// errors.go Trae 错误分类与冷却建议。
//
// 与 CodeBuddy 侧（upstream.Classify 的 6004/11101/11102 业务码）完全不同的码表，
// 故独立实现。分类决定三件事：是否换号、冷却多久、是否禁用账号。

// ErrKind 错误分类。
type ErrKind string

const (
	ErrNone ErrKind = ""
	// ErrPlanLimit 套餐/积分额度用尽（code 1005）：账号级长冷却，等次日或充值。
	ErrPlanLimit ErrKind = "plan_limit"
	// ErrSessionDead 凭证失效（HTTP 401）：需重新登录，账号直接禁用。
	ErrSessionDead ErrKind = "session_dead"
	// ErrDeviceProof 设备校验失败（20403 Device not match / 20405 Device proof required）：
	// 设备凭证与签名私钥不匹配，需重新导入；账号禁用而非冷却（冷却不会自愈）。
	ErrDeviceProof ErrKind = "device_proof"
	// ErrModelUnknown 模型不可用（4001 / 4023）：模型级问题，不罚账号。
	ErrModelUnknown ErrKind = "model_unknown"
	// ErrSoftRate 限流（HTTP 429）：短冷却。
	ErrSoftRate ErrKind = "soft_rate"
	// ErrServer 上游 5xx：中冷却。
	ErrServer ErrKind = "server"
	// ErrClient 其他 4xx（请求本身有问题）：不冷却（换号也一样错）。
	ErrClient ErrKind = "client"
)

// 冷却时长（分类 → 时长；0 表示不冷却）。
const (
	planLimitCooldown = 12 * time.Hour
	softRateCooldown  = 60 * time.Second
	serverCooldown    = 5 * time.Minute
)

// Classification 一次错误分类的结果。
type Classification struct {
	Kind ErrKind
	// Cooldown 账号级冷却时长（0 = 不冷却）。
	Cooldown time.Duration
	// Disable 是否应直接禁用账号（凭证/设备问题，冷却不会自愈）。
	Disable bool
	// Code 上游原始 code（HTTP 状态或业务码），供日志与面板展示。
	Code string
	// Message 可读原因（优先上游原文）。
	Message string
}

// Classify 按 HTTP 状态 + 响应体（或流内 error 事件）分类。
//
// status 为 0 表示错误来自流内 error 事件（此时用 streamCode/streamMsg）。
func Classify(status int, body []byte, streamCode int64, streamMsg string) Classification {
	// 流内错误：优先按业务码判定。
	if status == 0 && (streamCode != 0 || streamMsg != "") {
		return classifyBizCode(streamCode, streamMsg, status)
	}
	// 业务码可能出现在响应体里（HTTP 200/400 均可能）。
	if code, msg := bizCodeFromBody(body); code != 0 {
		if c := classifyBizCode(code, msg, status); c.Kind != ErrNone {
			return c
		}
	}
	switch {
	case status == http.StatusUnauthorized:
		return Classification{
			Kind: ErrSessionDead, Disable: true, Code: "401",
			Message: orDefault(strings.TrimSpace(string(body)), "token 已失效，需重新登录"),
		}
	case status == http.StatusTooManyRequests:
		return Classification{
			Kind: ErrSoftRate, Cooldown: softRateCooldown, Code: "429",
			Message: orDefault(strings.TrimSpace(string(body)), "上游限流"),
		}
	case status >= 500:
		return Classification{
			Kind: ErrServer, Cooldown: serverCooldown, Code: itoa(status),
			Message: orDefault(strings.TrimSpace(string(body)), "上游服务异常"),
		}
	case status >= 400:
		return Classification{
			Kind: ErrClient, Code: itoa(status),
			Message: orDefault(strings.TrimSpace(string(body)), "请求被上游拒绝"),
		}
	}
	return Classification{Kind: ErrNone}
}

// classifyBizCode 业务码 → 分类。
func classifyBizCode(code int64, msg string, status int) Classification {
	m := strings.TrimSpace(msg)
	switch code {
	case 1005:
		return Classification{
			Kind: ErrPlanLimit, Cooldown: planLimitCooldown, Code: "1005",
			Message: orDefault(m, "套餐额度用尽"),
		}
	case 20403:
		return Classification{
			Kind: ErrDeviceProof, Disable: true, Code: "20403",
			Message: orDefault(m, "设备不匹配（Device not match）：请重新导入设备凭证"),
		}
	case 20405:
		return Classification{
			Kind: ErrDeviceProof, Disable: true, Code: "20405",
			Message: orDefault(m, "缺少设备签名（Device proof required）：该账号需导入设备私钥"),
		}
	case 4001, 4023:
		return Classification{
			Kind: ErrModelUnknown, Code: itoa64(code),
			Message: orDefault(m, "模型在该账号不可用"),
		}
	case 401:
		return Classification{
			Kind: ErrSessionDead, Disable: true, Code: "401",
			Message: orDefault(m, "凭证失效，需重新登录"),
		}
	}
	// 未知业务码但 HTTP 层有信息：交给状态码分支判定。
	if status >= 400 {
		return Classification{}
	}
	return Classification{Kind: ErrNone}
}

// bizCodeFromBody 从响应体提取业务 code（宽容：数字或字符串）。
func bizCodeFromBody(body []byte) (int64, string) {
	if len(body) == 0 {
		return 0, ""
	}
	var m map[string]any
	if json.Unmarshal(body, &m) != nil {
		return 0, ""
	}
	code := intOf(m["code"])
	msg := str(m["message"])
	if msg == "" {
		msg = str(m["msg"])
	}
	if code == 0 {
		// 兼容 {error:{code,message}} 形态
		if errObj, ok := m["error"].(map[string]any); ok {
			code = intOf(errObj["code"])
			if msg == "" {
				msg = str(errObj["message"])
			}
		}
	}
	return code, msg
}

// orDefault 空串回落默认文案。
func orDefault(s, def string) string {
	if strings.TrimSpace(s) == "" {
		return def
	}
	return s
}

// itoa / itoa64 小工具（避免为两处引入 strconv 的重复判断）。
func itoa(n int) string { return itoa64(int64(n)) }

func itoa64(n int64) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
