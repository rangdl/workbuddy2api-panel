package trae

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestAccountPath 文件名带 trae- 前缀与安全化处理。
func TestAccountPath(t *testing.T) {
	dir := "/tmp/auths"
	if got := AccountPath(dir, "123456"); got != filepath.Join(dir, "trae-123456.json") {
		t.Errorf("AccountPath = %q", got)
	}
	// 可能破坏路径的字符被替换，不会跑出目录（filepath.Base 必须仍是该目录下的文件名）。
	got := AccountPath(dir, "../../etc/passwd")
	if filepath.Dir(got) != dir {
		t.Errorf("uid 未被安全化: %q", got)
	}
	if !strings.HasPrefix(filepath.Base(got), AccountFilePrefix) {
		t.Errorf("文件名缺少前缀: %q", got)
	}
}

// TestSaveAndLoadAccount 往返：写一账号一文件，读回来内容一致。
func TestSaveAndLoadAccount(t *testing.T) {
	dir := t.TempDir()
	in := &Account{
		UID: "u-1", Name: "主号", AccessToken: "at-1", RefreshToken: "rt-1",
		ExpiresAt: 1700000000, DeviceID: "1234567890123456", MachineID: "m-1",
		PrivateKeyPEM: "-----BEGIN PRIVATE KEY-----\nx\n-----END PRIVATE KEY-----",
		PublicKeyPEM:  "-----BEGIN PUBLIC KEY-----\ny\n-----END PUBLIC KEY-----",
		AppVersion:    "0.1.52",
	}
	if err := SaveAccount(dir, in); err != nil {
		t.Fatal(err)
	}
	// 权限必须是 0600（含明文 token 与设备私钥）。
	info, err := os.Stat(AccountPath(dir, "u-1"))
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("权限 = %o, want 600", perm)
	}

	out, err := LoadAccounts(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 1 {
		t.Fatalf("账号数 = %d", len(out))
	}
	got := out[0]
	if got.UID != in.UID || got.AccessToken != in.AccessToken || got.RefreshToken != in.RefreshToken {
		t.Errorf("往返不一致: %+v", got)
	}
	if got.DeviceID != in.DeviceID || got.PrivateKeyPEM != in.PrivateKeyPEM || got.PublicKeyPEM != in.PublicKeyPEM {
		t.Errorf("设备凭证往返不一致: %+v", got)
	}
}

// TestLoadAccountsSkipsBroken 单个坏文件不影响其余账号，目录不存在返回空。
func TestLoadAccountsSkipsBroken(t *testing.T) {
	dir := t.TempDir()
	if err := SaveAccount(dir, &Account{UID: "good", AccessToken: "t"}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "trae-bad.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "trae-empty.json"), []byte(`{"uid":"x"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	// 非 trae- 前缀的文件不应被读入。
	if err := os.WriteFile(filepath.Join(dir, "workbuddy-other.json"), []byte(`{"uid":"w","accessToken":"t"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	out, err := LoadAccounts(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 1 || out[0].UID != "good" {
		t.Fatalf("应只剩 good 一个账号，得到 %+v", out)
	}

	empty, err := LoadAccounts(filepath.Join(dir, "nope"))
	if err != nil {
		t.Errorf("目录不存在不应报错: %v", err)
	}
	if len(empty) != 0 {
		t.Errorf("目录不存在应返回空，得到 %d", len(empty))
	}
}

// TestSaveAccountRefusesEmptyCredentials 空凭证拒绝落盘（避免写出无法使用的账号文件）。
func TestSaveAccountRefusesEmptyCredentials(t *testing.T) {
	dir := t.TempDir()
	if err := SaveAccount(dir, &Account{UID: "u1"}); err == nil {
		t.Error("无 token 应拒绝落盘")
	}
	if err := SaveAccount(dir, &Account{AccessToken: "t"}); err == nil {
		t.Error("无 uid 应拒绝落盘")
	}
}

// TestRemoveAccount 删除幂等。
func TestRemoveAccount(t *testing.T) {
	dir := t.TempDir()
	if err := SaveAccount(dir, &Account{UID: "u1", AccessToken: "t"}); err != nil {
		t.Fatal(err)
	}
	if err := RemoveAccount(dir, "u1"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(AccountPath(dir, "u1")); !os.IsNotExist(err) {
		t.Error("文件应已删除")
	}
	if err := RemoveAccount(dir, "u1"); err != nil {
		t.Errorf("重复删除应幂等: %v", err)
	}
}

// TestMigrateLegacyAccounts 旧版内嵌账号拆成一账号一文件，并清空源数组。
func TestMigrateLegacyAccounts(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "trae.json")
	raw := `{"enabled":true,"chat_base":"https://h.example","accounts":[
		{"uid":"u1","access_token":"t1","device_id":"1234567890123456"},
		{"uid":"u2","access_token":"t2"}]}`
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}

	n, err := MigrateLegacyAccounts(path, "")
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("迁移数 = %d, want 2", n)
	}

	// 账号文件已生成。
	accounts, err := LoadAccounts(DefaultAuthDir(path))
	if err != nil {
		t.Fatal(err)
	}
	if len(accounts) != 2 {
		t.Fatalf("账号文件数 = %d", len(accounts))
	}

	// 源配置的全局字段保留、accounts 被清空。
	cfg, err := Load(path, "")
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Enabled || cfg.ChatBase != "https://h.example" {
		t.Errorf("全局配置未保留: %+v", cfg)
	}
	body, _ := os.ReadFile(path)
	if strings.Contains(string(body), `"accounts"`) {
		t.Errorf("源配置仍包含 accounts: %s", body)
	}

	// 幂等：再跑一次不迁移任何东西。
	n, err = MigrateLegacyAccounts(path, "")
	if err != nil || n != 0 {
		t.Errorf("二次迁移应无操作，得到 n=%d err=%v", n, err)
	}
}

// TestMigrateLegacyAccountsKeepsSourceOnFailure 迁移失败时源配置保持不变，
// 避免账号凭空消失。
func TestMigrateLegacyAccountsKeepsSourceOnFailure(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "trae.json")
	// 第一条缺 uid（必然失败），第二条正常。
	raw := `{"accounts":[{"access_token":"t0"},{"uid":"u1","access_token":"t1"}]}`
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}

	n, err := MigrateLegacyAccounts(path, "")
	if err == nil {
		t.Fatal("有失败项时应返回错误")
	}
	if n != 1 {
		t.Errorf("成功迁移数 = %d, want 1", n)
	}
	body, _ := os.ReadFile(path)
	if !strings.Contains(string(body), `"accounts"`) {
		t.Error("源配置不应被清空（否则失败的账号会丢失）")
	}
}

// TestMigrateLegacyAccountsHonorsAuthDir 迁移目录遵循 trae.json 里的 auth_dir。
func TestMigrateLegacyAccountsHonorsAuthDir(t *testing.T) {
	dir := t.TempDir()
	custom := filepath.Join(dir, "custom-auths")
	path := filepath.Join(dir, "trae.json")
	raw := `{"auth_dir":"` + custom + `","accounts":[{"uid":"u1","access_token":"t1"}]}`
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := MigrateLegacyAccounts(path, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(AccountPath(custom, "u1")); err != nil {
		t.Errorf("账号未写入自定义目录: %v", err)
	}
}

// TestConcurrentSaveAccountNoLoss 并发续期多个账号：各写各的文件，互不覆盖。
//
// 这是分文件存储的核心收益——旧实现重写整个 trae.json 时，后写者会丢掉
// 先写者刚更新的账号。
func TestConcurrentSaveAccountNoLoss(t *testing.T) {
	dir := t.TempDir()
	const n = 8
	done := make(chan error, n)
	for i := 0; i < n; i++ {
		go func(i int) {
			uid := "u-" + string(rune('a'+i))
			done <- SaveAccount(dir, &Account{
				UID: uid, AccessToken: "at-" + uid, RefreshToken: "rt-" + uid,
				DeviceID: "1234567890123456",
			})
		}(i)
	}
	for i := 0; i < n; i++ {
		if err := <-done; err != nil {
			t.Fatalf("并发写失败: %v", err)
		}
	}
	accounts, err := LoadAccounts(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(accounts) != n {
		t.Fatalf("账号数 = %d, want %d（有账号被覆盖）", len(accounts), n)
	}
	// 每个账号的 token 必须与自己的 uid 对应（没串号）。
	byUID := make(map[string]string, len(accounts))
	for _, a := range accounts {
		byUID[a.UID] = a.AccessToken
	}
	for i := 0; i < n; i++ {
		uid := "u-" + string(rune('a'+i))
		if byUID[uid] != "at-"+uid {
			t.Errorf("%s 的 token 不符: %q", uid, byUID[uid])
		}
	}
}

// TestLoadUsesExplicitAuthDir 账号目录必须由调用方显式指定。
//
// 标准布局里配置在 config/trae.json、账号在根 auths/，两者**不同级**——
// 若按配置文件位置推导会得到 config/auths/，与 workbuddy 账号分家。
func TestLoadUsesExplicitAuthDir(t *testing.T) {
	root := t.TempDir()
	configDir := filepath.Join(root, "config")
	authDir := filepath.Join(root, "auths")
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(configDir, "trae.json")
	if err := os.WriteFile(path, []byte(`{"enabled":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	// 账号写在根 auths/（与 workbuddy 同一目录）。
	if err := SaveAccount(authDir, &Account{UID: "trae-u1", AccessToken: "t1"}); err != nil {
		t.Fatal(err)
	}
	// 同目录下放一个 workbuddy 账号，确认不会被 trae 误读。
	if err := os.WriteFile(filepath.Join(authDir, "workbuddy-u2.json"),
		[]byte(`{"uid":"u2","accessToken":"t2"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(path, authDir)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.AuthDir != authDir {
		t.Errorf("AuthDir = %q, want %q", cfg.AuthDir, authDir)
	}
	if len(cfg.Accounts) != 1 || cfg.Accounts[0].UID != "trae-u1" {
		t.Errorf("应只读到 trae 账号，得到 %+v", cfg.Accounts)
	}

	// 不传默认目录时会退回 config/auths/（兜底行为，非生产路径）。
	fallback, err := Load(path, "")
	if err != nil {
		t.Fatal(err)
	}
	if fallback.AuthDir == authDir {
		t.Error("不传默认目录时不应知道根 auths/ 的位置")
	}
	if len(fallback.Accounts) != 0 {
		t.Errorf("兜底目录里没有账号，得到 %+v", fallback.Accounts)
	}
}

// TestMigrateUsesExplicitAuthDir 迁移也必须落到调用方指定的目录。
func TestMigrateUsesExplicitAuthDir(t *testing.T) {
	root := t.TempDir()
	configDir := filepath.Join(root, "config")
	authDir := filepath.Join(root, "auths")
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(configDir, "trae.json")
	raw := `{"accounts":[{"uid":"trae-u1","access_token":"t1"}]}`
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	n, err := MigrateLegacyAccounts(path, authDir)
	if err != nil || n != 1 {
		t.Fatalf("n=%d err=%v", n, err)
	}
	if _, err := os.Stat(AccountPath(authDir, "trae-u1")); err != nil {
		t.Errorf("账号未迁移到指定目录: %v", err)
	}
	// 不应在 config/ 下建 auths/。
	if _, err := os.Stat(filepath.Join(configDir, "auths")); !os.IsNotExist(err) {
		t.Error("不应在配置目录下创建 auths/")
	}
}
