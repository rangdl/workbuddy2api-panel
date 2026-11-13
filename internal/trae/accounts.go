// accounts.go Trae 账号的按账号分文件存储。
//
// 与 CodeBuddy 的 auths/workbuddy-<uid>.json 保持同一形态：
// 一个账号一个文件，文件名前缀区分 provider。
//
// 为什么必须分文件（而不是全塞在 trae.json 里）：
//   - **并发安全**：refreshToken 是轮换链，两个账号同时续期时，若都重写同一个
//     trae.json，后写者会把先写者刚更新的另一个账号覆盖掉（各自 tmp+rename
//     只保证单次写原子，不保证跨账号不丢更新）。
//   - **权限最小化**：账号文件含明文 refreshToken 与设备私钥，单独 0600。
//   - **与 CodeBuddy 一致**：备份 / 手工编辑 / 导入导出的心智模型统一。
//
// trae.json 只保留全局配置（开关、base、排程、UA 等），不再存账号。
package trae

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// AccountFilePrefix Trae 账号文件名前缀（与 workbuddy- 区分）。
const AccountFilePrefix = "trae-"

// accountFileSuffix 账号文件后缀。
const accountFileSuffix = ".json"

// AccountPath 返回某账号的落盘路径。
//
// uid 会做文件系统安全化处理：JWT 里取出的 uid 通常是纯数字或短 hash，
// 但登录兜底会用 "trae-"+token 尾 8 位，理论上可能带入路径分隔符。
func AccountPath(dir, uid string) string {
	return filepath.Join(dir, AccountFilePrefix+safeFileName(uid)+accountFileSuffix)
}

// safeFileName 把 uid 里可能破坏路径的字符替换成下划线。
func safeFileName(uid string) string {
	uid = strings.TrimSpace(uid)
	var b strings.Builder
	b.Grow(len(uid))
	for _, r := range uid {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '-', r == '_', r == '.':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	return b.String()
}

// LoadAccounts 读取目录下全部 trae-*.json 账号文件。
//
// 单个文件损坏不影响其余账号（与 auth.LoadDir 同取舍），但会打 WARN——
// 静默跳过会让"账号神秘消失"无从排查。目录不存在返回空列表而非错误。
func LoadAccounts(dir string) ([]Account, error) {
	if strings.TrimSpace(dir) == "" {
		return nil, nil
	}
	pattern := filepath.Join(dir, AccountFilePrefix+"*"+accountFileSuffix)
	files, err := filepath.Glob(pattern)
	if err != nil {
		return nil, fmt.Errorf("glob %s: %w", pattern, err)
	}
	sort.Strings(files) // 稳定顺序：面板 / 轮转顺序不受文件系统枚举顺序影响
	out := make([]Account, 0, len(files))
	seen := make(map[string]string, len(files))
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			log.Printf("WARN: [trae] 读取账号文件失败 %s: %v", f, err)
			continue
		}
		var a Account
		if err := json.Unmarshal(raw, &a); err != nil {
			log.Printf("WARN: [trae] 账号文件解析失败 %s: %v", f, err)
			continue
		}
		a.normalize()
		if a.UID == "" {
			log.Printf("WARN: [trae] 账号文件缺少 uid，已跳过: %s", f)
			continue
		}
		if a.AccessToken == "" && a.RefreshToken == "" {
			log.Printf("WARN: [trae] 账号 %s 既无 access_token 也无 refresh_token，已跳过: %s", a.UID, f)
			continue
		}
		if prev, ok := seen[a.UID]; ok {
			log.Printf("WARN: [trae] uid %s 重复（%s 与 %s），后者覆盖", a.UID, prev, f)
		}
		seen[a.UID] = f
		out = append(out, a)
	}
	return out, nil
}

// SaveAccount 原子写回单个账号文件（tmp + rename，权限 0600）。
//
// 三个调用点：OAuth 登录成功、设备凭证导入、token 续期轮换。
// 续期路径是并发热点——每个账号写自己的文件，互不影响。
func SaveAccount(dir string, a *Account) error {
	if strings.TrimSpace(dir) == "" {
		return fmt.Errorf("trae: auth_dir 为空，无法保存账号")
	}
	if a == nil {
		return fmt.Errorf("trae: nil account")
	}
	a.normalize()
	if a.UID == "" {
		return fmt.Errorf("trae: 账号缺少 uid，拒绝落盘")
	}
	if a.AccessToken == "" && a.RefreshToken == "" {
		return fmt.Errorf("trae: 账号 %s 既无 access_token 也无 refresh_token，拒绝落盘", a.UID)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("创建账号目录失败: %w", err)
	}
	b, err := json.MarshalIndent(a, "", "  ")
	if err != nil {
		return fmt.Errorf("序列化失败: %w", err)
	}
	b = append(b, '\n')
	path := AccountPath(dir, a.UID)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return fmt.Errorf("写入失败: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("原子替换失败: %w", err)
	}
	return nil
}

// RemoveAccount 删除账号文件。文件不存在不算错误（幂等）。
func RemoveAccount(dir, uid string) error {
	if strings.TrimSpace(dir) == "" {
		return fmt.Errorf("trae: auth_dir 为空")
	}
	err := os.Remove(AccountPath(dir, uid))
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}
