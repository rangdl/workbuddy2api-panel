package panel

import "testing"

// TestNormalizeTraeTime 上游时间格式转成前端约定的 ISO 形态。
//
// 前端对 created_at 做 slice(0,16).replace('T',' ')，若原样透传
// 「2026-09-14 15:52:38」（第 10 位是空格），发放日期列会对不齐。
func TestNormalizeTraeTime(t *testing.T) {
	cases := []struct{ in, want string }{
		{"2026-09-14 15:52:38", "2026-09-14T15:52:38"},
		{"2026-12-31", "2026-12-31T00:00:00"},
		{"", ""},                     // 空串原样返回
		{"not-a-time", "not-a-time"}, // 无法解析原样返回，由前端兜底
	}
	for _, c := range cases {
		if got := normalizeTraeTime(c.in); got != c.want {
			t.Errorf("normalizeTraeTime(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestTraePackExpiresAt 到期毫秒时间戳；无法解析返回 0。
func TestTraePackExpiresAt(t *testing.T) {
	if got := traePackExpiresAt("2026-12-31 23:59:59"); got <= 0 {
		t.Errorf("应返回正数毫秒时间戳, got %d", got)
	}
	if got := traePackExpiresAt(""); got != 0 {
		t.Errorf("空串应返回 0, got %d", got)
	}
	if got := traePackExpiresAt("garbage"); got != 0 {
		t.Errorf("无法解析应返回 0, got %d", got)
	}
}
