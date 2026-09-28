package trae

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"strings"
	"testing"
)

// newTestKey 生成一把 P-256 私钥（测试用）。
func newTestKey(t *testing.T) *ecdsa.PrivateKey {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func pkcs8PEM(t *testing.T, k *ecdsa.PrivateKey) string {
	t.Helper()
	der, err := x509.MarshalPKCS8PrivateKey(k)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
}

func sec1PEM(t *testing.T, k *ecdsa.PrivateKey) string {
	t.Helper()
	der, err := x509.MarshalECPrivateKey(k)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der}))
}

// TestParseECPrivateKey 两种 PEM 编码都要能解析，且解析结果与原文一致。
func TestParseECPrivateKey(t *testing.T) {
	k := newTestKey(t)
	for name, pemStr := range map[string]string{
		"PKCS#8": pkcs8PEM(t, k),
		"SEC1":   sec1PEM(t, k),
	} {
		got, err := ParseECPrivateKey(pemStr)
		if err != nil {
			t.Fatalf("%s 解析失败: %v", name, err)
		}
		if !got.Equal(k) {
			t.Fatalf("%s 解析出的私钥不一致", name)
		}
	}

	// 非法输入
	for name, in := range map[string]string{
		"空串":       "",
		"无 PEM 包裹": "MIGHAgEA",
		"垃圾内容":     "-----BEGIN PRIVATE KEY-----\nnot-base64\n-----END PRIVATE KEY-----",
	} {
		if _, err := ParseECPrivateKey(in); err == nil {
			t.Errorf("%s 应报错", name)
		}
	}
}

// TestSignVerifyP1363 签名长度必须是 64 字节（r||s），且能被验签；
// 篡改消息或签名必须验签失败。
func TestSignVerifyP1363(t *testing.T) {
	k := newTestKey(t)
	msg := ProofMessage("/trae/api/v3/oauth/ExchangeToken", "ono9krqynydwx5", "auth-code-1", 1753600000, "deadbeef")
	sig, err := SignP1363(k, msg)
	if err != nil {
		t.Fatal(err)
	}
	if len(sig) != proofSigLen {
		t.Fatalf("签名长度 = %d, want %d（P1363 r||s）", len(sig), proofSigLen)
	}
	if !VerifyP1363(&k.PublicKey, msg, sig) {
		t.Fatal("签名验签失败")
	}
	if VerifyP1363(&k.PublicKey, msg+"x", sig) {
		t.Error("篡改消息后不应验签通过")
	}
	bad := make([]byte, proofSigLen)
	copy(bad, sig)
	bad[0] ^= 0xFF
	if VerifyP1363(&k.PublicKey, msg, bad) {
		t.Error("篡改签名后不应验签通过")
	}
	if VerifyP1363(&k.PublicKey, msg, sig[:32]) {
		t.Error("长度不足的签名不应验签通过")
	}
}

// TestProofMessage 签名原文格式（5 个 \n 连接、Timestamp 为十进制整数）。
func TestProofMessage(t *testing.T) {
	got := ProofMessage("/p", "cid", "code", 42, "n1")
	want := "POST\n/p\ncid\ncode\n42\nn1"
	if got != want {
		t.Fatalf("ProofMessage = %q, want %q", got, want)
	}
	if n := strings.Count(got, "\n"); n != 5 {
		t.Fatalf("分隔符数量 = %d, want 5", n)
	}
}

// TestDeviceProof 输出字段必须 PascalCase，且 Timestamp 为数值（上游 schema 拒绝字符串）。
func TestDeviceProof(t *testing.T) {
	k := newTestKey(t)
	proof, err := DeviceProof(k, "/trae/api/v3/oauth/ExchangeToken", "cid", "code", 1753600000, "nonce-1")
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"Signature", "Timestamp", "Nonce"} {
		if _, ok := proof[key]; !ok {
			t.Fatalf("缺少字段 %s（必须 PascalCase）: %v", key, proof)
		}
	}
	if _, bad := proof["signature"]; bad {
		t.Error("不应出现小写字段名")
	}
	ts, ok := proof["Timestamp"].(int64)
	if !ok {
		t.Fatalf("Timestamp 必须是整数，得到 %T", proof["Timestamp"])
	}
	if ts != 1753600000 {
		t.Errorf("Timestamp = %d", ts)
	}
	sigB64, _ := proof["Signature"].(string)
	raw, err := base64.StdEncoding.DecodeString(sigB64)
	if err != nil {
		t.Fatalf("Signature 不是合法 base64: %v", err)
	}
	if len(raw) != proofSigLen {
		t.Fatalf("Signature 解码后 %d 字节, want %d", len(raw), proofSigLen)
	}
	// 用同一原文可验签（证明签名覆盖了正确内容）。
	msg := ProofMessage("/trae/api/v3/oauth/ExchangeToken", "cid", "code", ts, "nonce-1")
	if !VerifyP1363(&k.PublicKey, msg, raw) {
		t.Error("DeviceProof 的签名与签名原文不匹配")
	}
}

// TestPublicKeyPEM SPKI PEM 形状（DeviceInfo.DevicePublicKey 用）。
func TestPublicKeyPEM(t *testing.T) {
	k := newTestKey(t)
	pemStr, err := PublicKeyPEM(k)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(pemStr, "-----BEGIN PUBLIC KEY-----") {
		t.Fatalf("公钥 PEM 头不对: %q", pemStr[:40])
	}
	block, _ := pem.Decode([]byte(pemStr))
	if block == nil {
		t.Fatal("公钥 PEM 无法解码")
	}
	pub, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	ec, ok := pub.(*ecdsa.PublicKey)
	if !ok {
		t.Fatalf("公钥类型 = %T, want *ecdsa.PublicKey", pub)
	}
	if !ec.Equal(&k.PublicKey) {
		t.Error("导出的公钥与私钥不匹配")
	}
}

// TestRandomHex 长度与十六进制合法性。
func TestRandomHex(t *testing.T) {
	a, err := RandomHex(32)
	if err != nil {
		t.Fatal(err)
	}
	if len(a) != 64 {
		t.Fatalf("32 字节应变 64 位 hex，得到 %d", len(a))
	}
	b, _ := RandomHex(32)
	if a == b {
		t.Error("两次随机数不应相同")
	}
	if strings.ContainsFunc(a, func(r rune) bool {
		return !strings.ContainsRune("0123456789abcdef", r)
	}) {
		t.Errorf("非十六进制字符: %s", a)
	}
}
