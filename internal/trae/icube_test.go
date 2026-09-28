package trae

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha512"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
)

// pkcs7Pad 测试用填充（与 pkcs7Unpad 对称）。
func pkcs7Pad(b []byte, blockSize int) []byte {
	n := blockSize - len(b)%blockSize
	for i := 0; i < n; i++ {
		b = append(b, byte(n))
	}
	return b
}

// encryptTC 测试用 tc 信封加密（与 TCDecrypt 对称），用于往返验证。
func encryptTC(t *testing.T, body string, privateMode bool) string {
	t.Helper()
	random := make([]byte, tcRandomLen)
	if _, err := rand.Read(random); err != nil {
		t.Fatal(err)
	}
	key, iv := deriveTCKeys(random, derivePepper(privateMode))
	sum := sha512.Sum512([]byte(body))
	plain := pkcs7Pad(append(sum[:], []byte(body)...), aes.BlockSize)
	block, err := aes.NewCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	enc := make([]byte, len(plain))
	cipher.NewCBCEncrypter(block, iv).CryptBlocks(enc, plain)
	env := make([]byte, 0, tcMagicLen+tcRandomLen+len(enc))
	env = append(env, tcMagic[:]...)
	env = append(env, random...)
	env = append(env, enc...)
	return base64.StdEncoding.EncodeToString(env)
}

// TestPepperTables 常量表完整性：AES 模式 pepper 前 6 字节是已知值
// （82^31=77, 9^221=212, ...）。表抄错会让所有解密静默失败，必须有闸门。
func TestPepperTables(t *testing.T) {
	want := []byte{77, 212, 194, 230, 184, 49}
	p := derivePepper(false)
	for i, w := range want {
		if p[i] != w {
			t.Fatalf("pepper[%d] = %d, want %d（常量表抄录有误）", i, p[i], w)
		}
	}
	// AES_PRIVATE 模式 pepper 必须与 AES 模式不同（两组表不同源）。
	if derivePepper(true) == p {
		t.Fatal("两种模式的 pepper 不应相同")
	}
}

// TestTCDecryptRoundTrip 往返：加密 → 解密得到原文。
func TestTCDecryptRoundTrip(t *testing.T) {
	for _, privateMode := range []bool{false, true} {
		body := `{"privateKeyPEM":"-----BEGIN PRIVATE KEY-----\nMIGHAgEA\n-----END PRIVATE KEY-----","deviceId":"123"}`
		env := encryptTC(t, body, privateMode)
		got, err := TCDecrypt(env, privateMode)
		if err != nil {
			t.Fatalf("privateMode=%v 解密失败: %v", privateMode, err)
		}
		if got != body {
			t.Fatalf("privateMode=%v 往返不一致:\n got=%q\nwant=%q", privateMode, got, body)
		}
	}
}

// TestTCDecryptRejects 非法输入必须报错而非返回垃圾。
func TestTCDecryptRejects(t *testing.T) {
	// 非 base64
	if _, err := TCDecrypt("!!!not-base64!!!", false); err == nil {
		t.Error("非 base64 应报错")
	}
	// magic 不匹配
	bad := base64.StdEncoding.EncodeToString(make([]byte, 64))
	if _, err := TCDecrypt(bad, false); err == nil || !strings.Contains(err.Error(), "magic") {
		t.Errorf("magic 不匹配应报错，得到 %v", err)
	}
	// 长度不足
	short := base64.StdEncoding.EncodeToString([]byte{116, 99, 5, 16, 0, 0, 1, 2})
	if _, err := TCDecrypt(short, false); err == nil {
		t.Error("长度不足应报错")
	}
	// 密文被篡改 → 完整性校验失败
	env := encryptTC(t, `{"privateKeyPEM":"x"}`, false)
	raw, _ := base64.StdEncoding.DecodeString(env)
	raw[len(raw)-1] ^= 0xFF
	if _, err := TCDecrypt(base64.StdEncoding.EncodeToString(raw), false); err == nil {
		t.Error("篡改后的信封应校验失败")
	}
}

// TestDecryptICubeValue 单条目解密：deviceId 来自键名，私钥来自值。
func TestDecryptICubeValue(t *testing.T) {
	body := `{"privateKeyPEM":"-----BEGIN PRIVATE KEY-----\nAAAA\n-----END PRIVATE KEY-----"}`
	cred, err := DecryptICubeValue("99887766", encryptTC(t, body, false))
	if err != nil {
		t.Fatal(err)
	}
	if cred.DeviceID != "99887766" {
		t.Errorf("DeviceID = %q", cred.DeviceID)
	}
	if !strings.Contains(cred.PrivateKeyPEM, "BEGIN PRIVATE KEY") {
		t.Errorf("PrivateKeyPEM = %q", cred.PrivateKeyPEM)
	}

	// 明文缺 privateKeyPEM → 报错
	if _, err := DecryptICubeValue("1", encryptTC(t, `{"other":1}`, false)); err == nil {
		t.Error("缺少 privateKeyPEM 应报错")
	}
}

// TestParseStorageJSON 从客户端 storage.json 提取设备凭证（含 machineId）。
func TestParseStorageJSON(t *testing.T) {
	pem := "-----BEGIN PRIVATE KEY-----\\nAAAA\\n-----END PRIVATE KEY-----"
	entry := encryptTC(t, `{"privateKeyPEM":"`+pem+`"}`, false)
	storage := map[string]any{
		"telemetry.machineId":       "machine-abc",
		ICubeKeyPrefix + "11223344": entry,
		"unrelated.key":             "whatever",
	}
	raw, _ := json.Marshal(storage)
	creds, err := ParseStorageJSON(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(creds) != 1 {
		t.Fatalf("凭证数 = %d, want 1", len(creds))
	}
	if creds[0].DeviceID != "11223344" {
		t.Errorf("DeviceID = %q", creds[0].DeviceID)
	}
	if creds[0].MachineID != "machine-abc" {
		t.Errorf("MachineID = %q", creds[0].MachineID)
	}

	// 无 iCubeAuthInfo 条目 → 空切片而非报错
	empty, _ := json.Marshal(map[string]any{"foo": "bar"})
	creds, err = ParseStorageJSON(empty)
	if err != nil {
		t.Fatalf("无条目不应报错: %v", err)
	}
	if len(creds) != 0 {
		t.Errorf("无条目应返回空切片，得到 %d", len(creds))
	}

	// 非 JSON → 报错
	if _, err := ParseStorageJSON([]byte("not json")); err == nil {
		t.Error("非 JSON 应报错")
	}
}
