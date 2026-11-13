package trae

import "testing"

// TestParsePackTime 到期时间解析：空串 / 0 / 无法识别都要返回 false，
// 不能悄悄变成零值时间（那会被当成"已到期"清掉）。
func TestParsePackTime(t *testing.T) {
	if _, ok := parsePackTime(""); ok {
		t.Error("空串应返回 false")
	}
	if _, ok := parsePackTime("0"); ok {
		t.Error("0 应返回 false")
	}
	if _, ok := parsePackTime("not-a-time"); ok {
		t.Error("无法识别的格式应返回 false")
	}
	got, ok := parsePackTime("2026-09-14 15:52:38")
	if !ok {
		t.Fatal("标准格式应能解析")
	}
	if got.Year() != 2026 || got.Month() != 9 || got.Day() != 14 {
		t.Errorf("解析结果不符: %v", got)
	}
	// Unix 秒
	if _, ok := parsePackTime("1790000000"); !ok {
		t.Error("Unix 秒应能解析")
	}
}
