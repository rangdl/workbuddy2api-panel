package main

import (
	"strings"
	"testing"
)

// TestPresentNeverLeaksValue 脱敏红线：present 只报"有无 + 长度"，绝不回显内容。
func TestPresentNeverLeaksValue(t *testing.T) {
	secret := "eyJhbGciOiJSUzI1NiIsImtpZCI6ImtleS0xIn0.SUPER-SECRET-PAYLOAD"
	got := present(secret)
	if strings.Contains(got, "SUPER-SECRET") || strings.Contains(got, secret) {
		t.Fatalf("present 泄漏了凭证内容: %q", got)
	}
	if !strings.Contains(got, "已获取") {
		t.Errorf("present = %q，应说明已获取", got)
	}
	if present("") != "无" || present("   ") != "无" {
		t.Errorf("空值应报「无」，得到 %q / %q", present(""), present("   "))
	}
}

// TestKeyFingerprint 私钥指纹：可辨识同一把密钥，但不可逆、不泄漏原文。
func TestKeyFingerprint(t *testing.T) {
	pemA := "-----BEGIN PRIVATE KEY-----\nAAA\n-----END PRIVATE KEY-----"
	pemB := "-----BEGIN PRIVATE KEY-----\nBBB\n-----END PRIVATE KEY-----"
	fpA, fpB := keyFingerprint(pemA), keyFingerprint(pemB)
	if fpA == fpB {
		t.Error("不同私钥应得到不同指纹")
	}
	if len(fpA) != 16 {
		t.Errorf("指纹应为 16 位 hex（SHA-256 前 8 字节），得到 %d 位: %s", len(fpA), fpA)
	}
	if strings.Contains(fpA, "PRIVATE") || strings.Contains(fpA, "AAA") {
		t.Errorf("指纹泄漏了私钥内容: %s", fpA)
	}
	if keyFingerprint(pemA) != fpA {
		t.Error("同一私钥的指纹应稳定")
	}
	if keyFingerprint("") != "无" {
		t.Errorf("空私钥应报「无」，得到 %q", keyFingerprint(""))
	}
}

// TestGenDevice 生成的设备标识形态：device_id 15 位数字、machine_id 32 位 hex。
func TestGenDevice(t *testing.T) {
	for i := 0; i < 5; i++ {
		dID, mID := genDevice()
		if len(dID) != 15 {
			t.Fatalf("device_id 应为 15 位，得到 %d: %s", len(dID), dID)
		}
		for _, r := range dID {
			if r < '0' || r > '9' {
				t.Fatalf("device_id 应全为数字，得到 %s", dID)
			}
		}
		if len(mID) != 64 {
			t.Fatalf("machine_id 应为 64 位 hex（32 字节），得到 %d", len(mID))
		}
		if strings.ContainsFunc(mID, func(r rune) bool {
			return !strings.ContainsRune("0123456789abcdef", r)
		}) {
			t.Fatalf("machine_id 非十六进制: %s", mID)
		}
	}
	// 两次生成不应相同（随机性）
	a1, b1 := genDevice()
	a2, b2 := genDevice()
	if a1 == a2 && b1 == b2 {
		t.Error("两次生成不应完全相同")
	}
}
