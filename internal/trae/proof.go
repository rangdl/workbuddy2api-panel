package trae

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"math/big"
	"strings"
)

// proof.go DeviceProof 签名（ExchangeToken 的 20405 校验）。
//
// 协议（2026-09-16 实测固化）：
//   - 算法 ECDSA P-256 + SHA-256；
//   - 签名原文 5 个 \n 连接：`POST\n<path>\n<ClientID>\n<AuthCode>\n<Timestamp>\n<Nonce>`
//     （AuthCode 位置在 refresh 场景放 refresh_token）；
//   - 签名编码 **P1363（r||s 固定 64 字节）**——WebCrypto（Electron 客户端 JS 侧）
//     的标准输出，ring 参照实现同为该格式；DER 编码实测被上游拒绝（20405）；
//   - JSON 字段必须 PascalCase（Signature/Timestamp/Nonce），小写会被拒；
//     Timestamp 必须是 JSON number（字符串会被 schema 拒绝）。

// proofSigLen P-256 的 P1363 签名长度（r 32B + s 32B）。
const proofSigLen = 64

// ParseECPrivateKey 解析 PEM 私钥，支持 PKCS#8（"PRIVATE KEY"）
// 与 SEC1（"EC PRIVATE KEY"）两种编码。
func ParseECPrivateKey(pemStr string) (*ecdsa.PrivateKey, error) {
	block, _ := pem.Decode([]byte(strings.TrimSpace(pemStr)))
	if block == nil {
		return nil, fmt.Errorf("PEM 解码失败（缺少 BEGIN/END 包裹？）")
	}
	if key, err := x509.ParsePKCS8PrivateKey(block.Bytes); err == nil {
		ec, ok := key.(*ecdsa.PrivateKey)
		if !ok {
			return nil, fmt.Errorf("私钥不是 EC 类型（实际 %T）", key)
		}
		return ec, nil
	}
	ec, err := x509.ParseECPrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("私钥解析失败（PKCS#8 与 SEC1 均不匹配）: %w", err)
	}
	return ec, nil
}

// PublicKeyPEM 由私钥推导 SPKI PEM 公钥（DeviceInfo.DevicePublicKey 用）。
func PublicKeyPEM(priv *ecdsa.PrivateKey) (string, error) {
	der, err := x509.MarshalPKIXPublicKey(&priv.PublicKey)
	if err != nil {
		return "", fmt.Errorf("公钥 SPKI 编码失败: %w", err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})), nil
}

// ProofMessage 拼装签名原文（导出供测试与排错核对）。
func ProofMessage(signPath, clientID, authCode string, ts int64, nonce string) string {
	return fmt.Sprintf("POST\n%s\n%s\n%s\n%d\n%s", signPath, clientID, authCode, ts, nonce)
}

// SignP1363 对消息做 ECDSA P-256 + SHA-256 签名，输出 P1363（r||s，64 字节）。
func SignP1363(priv *ecdsa.PrivateKey, msg string) ([]byte, error) {
	if priv == nil {
		return nil, fmt.Errorf("设备私钥为空")
	}
	digest := sha256.Sum256([]byte(msg))
	r, s, err := ecdsa.Sign(rand.Reader, priv, digest[:])
	if err != nil {
		return nil, fmt.Errorf("签名失败: %w", err)
	}
	sig := make([]byte, proofSigLen)
	r.FillBytes(sig[:32])
	s.FillBytes(sig[32:])
	return sig, nil
}

// VerifyP1363 校验 P1363 签名（供单测与自检；生产路径不调用）。
func VerifyP1363(pub *ecdsa.PublicKey, msg string, sig []byte) bool {
	if len(sig) != proofSigLen {
		return false
	}
	digest := sha256.Sum256([]byte(msg))
	r := new(big.Int).SetBytes(sig[:32])
	s := new(big.Int).SetBytes(sig[32:])
	return ecdsa.Verify(pub, digest[:], r, s)
}

// DeviceProof 生成 DeviceProof JSON 对象。
//
// signPath 是签名原文里的路径段（如 "/trae/api/v3/oauth/ExchangeToken"），
// 必须与请求实际路径一致——上游按路径参与验签。
func DeviceProof(priv *ecdsa.PrivateKey, signPath, clientID, authCode string, ts int64, nonce string) (map[string]any, error) {
	sig, err := SignP1363(priv, ProofMessage(signPath, clientID, authCode, ts, nonce))
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"Signature": base64.StdEncoding.EncodeToString(sig),
		"Timestamp": ts,
		"Nonce":     nonce,
	}, nil
}

// RandomHex 生成 n 字节随机数的十六进制串（DeviceProof 的 Nonce 用）。
func RandomHex(n int) (string, error) {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("随机数生成失败: %w", err)
	}
	return hex.EncodeToString(buf), nil
}

// CurveP256 返回 P-256 曲线（供外部构造/校验设备密钥时复用）。
func CurveP256() elliptic.Curve { return elliptic.P256() }
