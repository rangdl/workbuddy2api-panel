package trae

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/sha512"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
)

// icube.go Trae 客户端设备凭证（icube）解析。
//
// 背景：Trae 把 OAuth 设备的 EC P-256 密钥对存在客户端
// `User/globalStorage/storage.json` 的 `iCubeAuthInfo://icube-dc:<deviceId>` 键下，
// 值经项目自研「tc」信封加密（AES-128-CBC）。
//
// 关键事实：**tc 信封的 pepper 是随安装包分发的公开常量表**（从 out/main.js 提取，
// 非密钥），因此只要拿到密文即可解密——本网关作为服务端无需 Trae 客户端在场，
// 用户把 storage.json 里对应条目的值贴进来即可。
//
// 解密属"读用户自有凭证"，私钥只在内存中用于签名，不落盘、不进日志。

// byteCrypto 四常量表（Trae CN resources/app/out/main.js 2026-09-16 实测提取）。
// 有效 pepper 由两组异或得到：AES 模式 = Woe^Voe，AES_PRIVATE 模式 = Joe^Hoe。
var (
	woeT = [64]byte{82, 9, 106, 213, 48, 54, 165, 56, 191, 64, 163, 158, 129, 243, 215, 251, 124, 227, 57, 130, 155, 47, 255, 135, 52, 142, 67, 68, 196, 222, 233, 203, 84, 123, 148, 50, 166, 194, 35, 61, 238, 76, 149, 11, 66, 250, 195, 78, 8, 46, 161, 102, 40, 217, 36, 178, 118, 91, 162, 73, 109, 139, 209, 37}
	voeT = [64]byte{31, 221, 168, 51, 136, 7, 199, 49, 177, 18, 16, 89, 39, 128, 236, 95, 96, 81, 127, 169, 25, 181, 74, 13, 45, 229, 122, 159, 147, 201, 156, 239, 160, 224, 59, 77, 174, 42, 245, 176, 200, 235, 187, 60, 131, 83, 153, 97, 23, 43, 4, 126, 186, 119, 214, 38, 225, 105, 20, 99, 85, 33, 12, 125}
	joeT = [64]byte{191, 192, 216, 250, 122, 246, 220, 97, 31, 254, 98, 27, 8, 72, 71, 176, 135, 99, 96, 18, 127, 101, 203, 104, 211, 102, 191, 125, 37, 72, 150, 156, 51, 229, 121, 35, 17, 153, 141, 177, 110, 131, 150, 128, 172, 255, 254, 6, 18, 140, 55, 62, 236, 249, 135, 64, 135, 12, 117, 4, 89, 149, 168, 209}
	hoeT = [64]byte{246, 204, 26, 232, 232, 70, 129, 109, 223, 146, 169, 242, 23, 241, 105, 145, 50, 196, 165, 42, 254, 120, 3, 54, 244, 207, 209, 85, 53, 6, 138, 106, 175, 148, 31, 204, 186, 186, 165, 182, 87, 142, 49, 10, 39, 110, 26, 154, 86, 56, 173, 125, 18, 64, 198, 225, 99, 99, 83, 82, 191, 134, 76, 170}
)

// tc 信封结构常量（对齐 byteCrypto）。
const (
	tcMagicLen  = 6
	tcRandomLen = 32
	tcTagLen    = 64 // SHA-512 完整性校验标签
	// ICubeKeyPrefix storage.json 中设备凭证键的前缀。
	ICubeKeyPrefix = "iCubeAuthInfo://icube-dc:"
)

var tcMagic = [6]byte{116, 99, 5, 16, 0, 0}

// DeviceCredential 从客户端凭证中解析出的设备身份。
type DeviceCredential struct {
	// DeviceID 设备 id（数字串，与 storage.json 键名内嵌的一致）。
	DeviceID string `json:"device_id"`
	// PrivateKeyPEM EC P-256 私钥（PKCS#8 或 SEC1 PEM）。
	PrivateKeyPEM string `json:"private_key_pem"`
	// MachineID 遥测机器 id（DeviceInfo 构造用；storage.json 的 telemetry.machineId）。
	MachineID string `json:"machine_id"`
	// AppVersion 客户端版本（安装目录 package.json 的 version）。
	AppVersion string `json:"app_version"`
}

// TCDecrypt 解密 tc 信封，返回明文（JSON 字符串）。
//
// 算法（byteCrypto 逆向，BlueChonk 交叉验证）：
//
//	pepper    = Woe^Voe（privateMode 时 Joe^Hoe）
//	derived   = SHA512( SHA512(random) || pepper )     取前 32B
//	key, iv   = derived[0:16], derived[16:32]
//	plain     = AES-128-CBC-decrypt(cipher) 去 PKCS7
//	校验      = plain[0:64] == SHA512(plain[64:])
func TCDecrypt(b64 string, privateMode bool) (string, error) {
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(b64))
	if err != nil {
		return "", fmt.Errorf("tc 信封 base64 解码失败: %w", err)
	}
	if len(raw) < tcMagicLen+tcRandomLen+aes.BlockSize {
		return "", fmt.Errorf("tc 信封长度异常: %d", len(raw))
	}
	for i := 0; i < tcMagicLen; i++ {
		if raw[i] != tcMagic[i] {
			return "", fmt.Errorf("tc 信封 magic 不匹配: %02x（期望 746305100000）", raw[:tcMagicLen])
		}
	}
	random := raw[tcMagicLen : tcMagicLen+tcRandomLen]
	cipherText := raw[tcMagicLen+tcRandomLen:]
	if len(cipherText)%aes.BlockSize != 0 {
		return "", fmt.Errorf("tc 密文长度非 AES 块整数倍: %d", len(cipherText))
	}

	pepper := derivePepper(privateMode)
	key, iv := deriveTCKeys(random, pepper)

	block, err := aes.NewCipher(key)
	if err != nil {
		return "", fmt.Errorf("AES 初始化失败: %w", err)
	}
	padded := make([]byte, len(cipherText))
	cipher.NewCBCDecrypter(block, iv).CryptBlocks(padded, cipherText)
	plain, err := pkcs7Unpad(padded, aes.BlockSize)
	if err != nil {
		return "", fmt.Errorf("tc 信封解密失败（pepper 表可能已随客户端版本变化）: %w", err)
	}
	if len(plain) <= tcTagLen {
		return "", fmt.Errorf("tc 解密后明文过短: %d", len(plain))
	}
	tag, body := plain[:tcTagLen], plain[tcTagLen:]
	sum := sha512.Sum512(body)
	for i := 0; i < tcTagLen; i++ {
		if sum[i] != tag[i] {
			return "", fmt.Errorf("tc 信封完整性校验失败（SHA-512 不匹配）")
		}
	}
	return string(body), nil
}

// derivePepper 按模式异或出 64 字节 pepper。
func derivePepper(privateMode bool) [64]byte {
	var p [64]byte
	if privateMode {
		for i := range p {
			p[i] = joeT[i] ^ hoeT[i]
		}
		return p
	}
	for i := range p {
		p[i] = woeT[i] ^ voeT[i]
	}
	return p
}

// deriveTCKeys 密钥派生：SHA512(random) || pepper → SHA512 → 前 32B 切分 key/iv。
func deriveTCKeys(random []byte, pepper [64]byte) (key, iv []byte) {
	inner := sha512.Sum512(random)
	buf := make([]byte, 0, len(inner)+len(pepper))
	buf = append(buf, inner[:]...)
	buf = append(buf, pepper[:]...)
	outer := sha512.Sum512(buf)
	return outer[:16], outer[16:32]
}

// pkcs7Unpad 去除 PKCS#7 填充（严格校验，防止把损坏密文当合法明文）。
func pkcs7Unpad(b []byte, blockSize int) ([]byte, error) {
	if len(b) == 0 || len(b)%blockSize != 0 {
		return nil, fmt.Errorf("填充长度非法: %d", len(b))
	}
	n := int(b[len(b)-1])
	if n == 0 || n > blockSize || n > len(b) {
		return nil, fmt.Errorf("PKCS#7 填充值非法: %d", n)
	}
	for _, c := range b[len(b)-n:] {
		if int(c) != n {
			return nil, fmt.Errorf("PKCS#7 填充字节不一致")
		}
	}
	return b[:len(b)-n], nil
}

// DecryptICubeValue 解密单个 iCubeAuthInfo 值，返回设备凭证。
// deviceID 由调用方从 storage.json 的键名给出（值本身不含设备 id）。
func DecryptICubeValue(deviceID, b64 string) (*DeviceCredential, error) {
	plain, err := TCDecrypt(b64, false)
	if err != nil {
		return nil, err
	}
	var parsed struct {
		PrivateKeyPEM string `json:"privateKeyPEM"`
	}
	if err := json.Unmarshal([]byte(plain), &parsed); err != nil {
		return nil, fmt.Errorf("tc 明文非预期 JSON: %w", err)
	}
	if !strings.Contains(parsed.PrivateKeyPEM, "BEGIN") {
		return nil, fmt.Errorf("tc 明文中缺少 privateKeyPEM 字段")
	}
	return &DeviceCredential{
		DeviceID:      strings.TrimSpace(deviceID),
		PrivateKeyPEM: strings.TrimSpace(parsed.PrivateKeyPEM),
	}, nil
}

// ParseStorageJSON 从 Trae 客户端 storage.json 中提取全部设备凭证。
//
// 会顺带读取 telemetry.machineId（DeviceInfo 构造用）。返回空切片表示
// 文件里没有 iCubeAuthInfo 条目（未登录过 / 版本布局变化）。
func ParseStorageJSON(raw []byte) ([]*DeviceCredential, error) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil, fmt.Errorf("storage.json 解析失败: %w", err)
	}
	var machineID string
	if v, ok := obj["telemetry.machineId"]; ok {
		_ = json.Unmarshal(v, &machineID)
	}
	out := make([]*DeviceCredential, 0, 2)
	for k, v := range obj {
		deviceID, ok := strings.CutPrefix(k, ICubeKeyPrefix)
		if !ok {
			continue
		}
		var b64 string
		if err := json.Unmarshal(v, &b64); err != nil {
			continue
		}
		cred, err := DecryptICubeValue(deviceID, b64)
		if err != nil {
			// 单个条目解密失败不影响其余条目（pepper 表变化时至少能报出原因）。
			continue
		}
		cred.MachineID = machineID
		out = append(out, cred)
	}
	return out, nil
}
