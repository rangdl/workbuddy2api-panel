package trae

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestConfigNormalize 缺省值补齐。
func TestConfigNormalize(t *testing.T) {
	c := Config{ChatBase: "https://host.example/", Accounts: []Account{{UID: " u1 ", AccessToken: " t "}}}
	c.Normalize()
	if c.ChatBase != "https://host.example" {
		t.Errorf("ChatBase = %q（应去尾斜杠）", c.ChatBase)
	}
	if c.IDEVersion != DefaultIDEVersion || c.IDEVersionCode != DefaultIDEVersionCode {
		t.Errorf("IDE 版本缺省未补齐: %q/%q", c.IDEVersion, c.IDEVersionCode)
	}
	if c.AppID != DefaultAppID {
		t.Errorf("AppID = %q", c.AppID)
	}
	if c.TimeoutSeconds != DefaultTimeoutSeconds {
		t.Errorf("TimeoutSeconds = %d", c.TimeoutSeconds)
	}
	if c.Accounts[0].UID != "u1" || c.Accounts[0].AccessToken != "t" {
		t.Errorf("账号字段未 trim: %+v", c.Accounts[0])
	}
}

// TestConfigValidate 配置错误必须被拒（fail fast），且给出可定位的信息。
func TestConfigValidate(t *testing.T) {
	valid := Account{UID: "u1", AccessToken: "Cloud-IDE-JWT abc"}
	cases := []struct {
		name    string
		cfg     Config
		wantErr string
	}{
		{"合法", Config{ChatBase: "https://h.example", Accounts: []Account{valid}}, ""},
		{"无账号也合法", Config{ChatBase: "https://h.example"}, ""},
		{"chat_base 无 scheme", Config{ChatBase: "h.example"}, "chat_base"},
		{"uid 为空", Config{ChatBase: "https://h.example", Accounts: []Account{{AccessToken: "t"}}}, "uid"},
		{"uid 重复", Config{ChatBase: "https://h.example", Accounts: []Account{valid, valid}}, "重复"},
		{"无任何 token", Config{ChatBase: "https://h.example", Accounts: []Account{{UID: "u2"}}}, "至少要有一个"},
		{"仅 refresh_token 合法", Config{ChatBase: "https://h.example", Accounts: []Account{{UID: "u3", RefreshToken: "rt"}}}, ""},
		{"私钥无法解析", Config{ChatBase: "https://h.example", Accounts: []Account{
			{UID: "u4", AccessToken: "t", PrivateKeyPEM: "-----BEGIN PRIVATE KEY-----\nzzz\n-----END PRIVATE KEY-----"},
		}}, "设备私钥"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := c.cfg.Validate()
			if c.wantErr == "" {
				if err != nil {
					t.Fatalf("应通过校验，得到 %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("应报错（含 %q）", c.wantErr)
			}
			if !strings.Contains(err.Error(), c.wantErr) {
				t.Fatalf("错误信息 %q 不含 %q", err.Error(), c.wantErr)
			}
		})
	}
}

// TestConfigValidateAcceptsRealKey 合法私钥应通过校验（与 proof 测试的密钥生成对齐）。
func TestConfigValidateAcceptsRealKey(t *testing.T) {
	k := newTestKey(t)
	cfg := Config{ChatBase: "https://h.example", Accounts: []Account{{
		UID: "u1", AccessToken: "t", PrivateKeyPEM: pkcs8PEM(t, k),
	}}}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("合法私钥应通过: %v", err)
	}
}

// TestConfigLoad 文件缺失返回 (nil, nil)；损坏返回 error；合法返回归一后的配置。
func TestConfigLoad(t *testing.T) {
	dir := t.TempDir()

	// 缺失
	cfg, err := Load(filepath.Join(dir, "nope.json"))
	if err != nil || cfg != nil {
		t.Fatalf("文件缺失应返回 (nil,nil)，得到 %v/%v", cfg, err)
	}

	// 损坏
	bad := filepath.Join(dir, "bad.json")
	if err := os.WriteFile(bad, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(bad); err == nil {
		t.Error("损坏文件应报错")
	}

	// 非法（uid 重复）
	invalid := filepath.Join(dir, "invalid.json")
	raw := `{"enabled":true,"chat_base":"https://h.example","accounts":[
		{"uid":"u1","access_token":"t"},{"uid":"u1","access_token":"t"}]}`
	if err := os.WriteFile(invalid, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(invalid); err == nil {
		t.Error("uid 重复应报错")
	}

	// 合法 + 缺省补齐
	good := filepath.Join(dir, "trae.json")
	ok := `{"enabled":true,"accounts":[{"uid":"u1","access_token":"t"}]}`
	if err := os.WriteFile(good, []byte(ok), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err = Load(good)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ChatBase != DefaultChatBase {
		t.Errorf("ChatBase = %q, want %q", cfg.ChatBase, DefaultChatBase)
	}
	if !cfg.Enabled || len(cfg.Accounts) != 1 {
		t.Errorf("加载结果不符: %+v", cfg)
	}
}

// TestAccessTokenHeader 裸 JWT 自动补前缀；已带前缀不重复补。
func TestAccessTokenHeader(t *testing.T) {
	cases := map[string]string{
		"eyJhbGciOi":               "Cloud-IDE-JWT eyJhbGciOi",
		"Cloud-IDE-JWT eyJhbGciOi": "Cloud-IDE-JWT eyJhbGciOi",
		"  eyJhbGciOi  ":           "Cloud-IDE-JWT eyJhbGciOi",
		"":                         "",
	}
	for in, want := range cases {
		a := &Account{AccessToken: in}
		if got := a.AccessTokenHeader(); got != want {
			t.Errorf("AccessTokenHeader(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestAccountByUID 查账号返回副本（改副本不影响配置）。
func TestAccountByUID(t *testing.T) {
	cfg := &Config{Accounts: []Account{{UID: "u1", Name: "a"}, {UID: "u2", Name: "b"}}}
	got := cfg.AccountByUID("u2")
	if got == nil || got.Name != "b" {
		t.Fatalf("AccountByUID(u2) = %+v", got)
	}
	got.Name = "changed"
	if cfg.Accounts[1].Name != "b" {
		t.Error("返回的应是副本，修改不应影响配置")
	}
	if cfg.AccountByUID("nope") != nil {
		t.Error("不存在的 uid 应返回 nil")
	}
}
