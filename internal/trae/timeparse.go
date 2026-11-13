package trae

import (
	"strconv"
	"strings"
	"time"
)

// timeparse.go 积分包到期时间解析。
//
// 放在 trae 包而不是 backend/trae：解析的是本包 CreditPack.EndTime 的字段
// 语义，面板（积分到期提醒）与 backend（Credits 查询）都要用，两边各自
// 复制一份会漂移。

// packTimeLayouts 上游 end_time / expire_time 的已知格式。
//
// 实测形如 "2026-09-14 15:52:38"（无时区，按本地时区解释）。多列几种格式
// 是因为不同积分包的字段来源不同（base / pack 两层都有 expire_time）。
var packTimeLayouts = []string{
	"2006-01-02 15:04:05",
	"2006-01-02T15:04:05Z07:00",
	"2006-01-02T15:04:05",
	"2006-01-02",
}

// ParsePackTime 解析积分包的到期时间。
//
// 空串 / 纯数字（Unix 秒）/ 无法识别的格式一律返回 ok=false——
// 调用方据此把「到期时间未知」与「已到期」区分开，不会被零值误导。
func ParsePackTime(s string) (time.Time, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, false
	}
	// 纯数字按 Unix 秒（部分包给的是时间戳）。
	if n, err := strconv.ParseInt(s, 10, 64); err == nil {
		if n <= 0 {
			return time.Time{}, false
		}
		return time.Unix(n, 0), true
	}
	for _, layout := range packTimeLayouts {
		if t, err := time.ParseInLocation(layout, s, time.Local); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}
