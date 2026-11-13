package panel

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/trae"
)

// newTraeTestKeyPEM 生成一把 P-256 私钥的 PKCS#8 PEM（测试用）。
func newTraeTestKeyPEM(t *testing.T) string {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(k)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
}

// writeTraeTestConfig 准备测试前置状态，绕过面板接口。
//
// 账号不再内嵌在 trae.json 里：这里拆成「写全局配置」+「逐个写账号文件」，
// 与生产路径（trae.Load 从 auth_dir 装配）保持一致。
func writeTraeTestConfig(t *testing.T, path string, cfg *trae.Config) {
	t.Helper()
	dir := trae.DefaultAuthDir(path)
	if cfg.AuthDir == "" {
		cfg.AuthDir = dir
	}
	for i := range cfg.Accounts {
		if err := trae.SaveAccount(cfg.AuthDir, &cfg.Accounts[i]); err != nil {
			t.Fatal(err)
		}
	}
	if err := writeTraeConfig(path, cfg); err != nil {
		t.Fatal(err)
	}
}

// newTraePanel 构造带 Trae 配置路径的面板（无登录客户端时登录接口返回 501）。
func newTraePanel(t *testing.T, dir string) (*Panel, string) {
	t.Helper()
	path := filepath.Join(dir, "trae.json")
	reloaded := 0
	p := New(Config{
		Version:       "test",
		APIKey:        "test-key",
		TraePath:      path,
		TraeClient:    trae.NewClient(),
		TraeCooldowns: trae.NewCooldowns(),
		ReloadTrae: func() error {
			reloaded++
			return nil
		},
	})
	return p, path
}

func traeDo(p *Panel, method, path, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, bytes.NewReader([]byte(body)))
	req.Header.Set("Authorization", "Bearer test-key")
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	return rec
}

// TestTraeConfigNeverLeaksCredentials 安全红线：GET 响应不得出现任何凭证原文。
func TestTraeConfigNeverLeaksCredentials(t *testing.T) {
	dir := t.TempDir()
	p, path := newTraePanel(t, dir)
	secret := "eyJhbGciOiJSUPER-SECRET-TOKEN"
	key := newTraeTestKeyPEM(t)
	cfg := trae.Config{
		Enabled:  true,
		ChatBase: trae.DefaultChatBase,
		Accounts: []trae.Account{{
			UID: "u-1", Name: "trae-1", AccessToken: secret, RefreshToken: "rt-SECRET",
			PrivateKeyPEM: key, DeviceID: "dev-1",
		}},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	writeTraeTestConfig(t, path, &cfg)

	rec := traeDo(p, http.MethodGet, "/panel/api/trae_config", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body)
	}
	body := rec.Body.String()
	for _, leak := range []string{secret, "rt-SECRET", "BEGIN PRIVATE KEY"} {
		if strings.Contains(body, leak) {
			t.Fatalf("响应泄漏凭证 %q:\n%s", leak, body)
		}
	}
	// 但必须给出"是否已配置"与尾号，供用户核对。
	var got struct {
		Config struct {
			Enabled  bool `json:"enabled"`
			Accounts []struct {
				UID          string `json:"uid"`
				HasToken     bool   `json:"has_token"`
				HasRefresh   bool   `json:"has_refresh"`
				HasDeviceKey bool   `json:"has_device_key"`
				TokenTail    string `json:"token_tail"`
			} `json:"accounts"`
		} `json:"config"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if !got.Config.Enabled || len(got.Config.Accounts) != 1 {
		t.Fatalf("配置视图不符: %s", body)
	}
	a := got.Config.Accounts[0]
	if !a.HasToken || !a.HasRefresh || !a.HasDeviceKey {
		t.Errorf("has_* 标记不符: %+v", a)
	}
	if a.TokenTail != "OKEN" && !strings.HasSuffix(secret, a.TokenTail) {
		t.Errorf("token_tail 应为末 4 位，得到 %q", a.TokenTail)
	}
}

// TestTraeConfigSavePreservesCredentials 保存非敏感字段时必须保留凭证（面板不回显，也就无法回传）。
func TestTraeConfigSavePreservesCredentials(t *testing.T) {
	dir := t.TempDir()
	p, path := newTraePanel(t, dir)
	cfg := trae.Config{
		Enabled: false, ChatBase: trae.DefaultChatBase,
		Accounts: []trae.Account{
			{UID: "u-1", AccessToken: "tok-1", RefreshToken: "rt-1", DeviceID: "d1"},
			{UID: "u-2", AccessToken: "tok-2", DeviceID: "d2"},
		},
	}
	writeTraeTestConfig(t, path, &cfg)

	// 启用 + 只保留 u-2 + 调整顺序
	body := `{"enabled":true,"chat_base":"https://custom.example","ide_version":"9.9.9","accounts":["u-2"],"disabled":[]}`
	rec := traeDo(p, http.MethodPost, "/panel/api/trae_config", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body)
	}
	saved, err := trae.Load(path, "")
	if err != nil {
		t.Fatal(err)
	}
	if !saved.Enabled || saved.ChatBase != "https://custom.example" || saved.IDEVersion != "9.9.9" {
		t.Fatalf("非敏感字段未生效: %+v", saved)
	}
	if len(saved.Accounts) != 1 || saved.Accounts[0].UID != "u-2" {
		t.Fatalf("账号列表未按提交重建: %+v", saved.Accounts)
	}
	if saved.Accounts[0].AccessToken != "tok-2" {
		t.Errorf("凭证应被保留，得到 %q", saved.Accounts[0].AccessToken)
	}
}

// TestTraeConfigSaveDisableAccount disabled 列表生效。
func TestTraeConfigSaveDisableAccount(t *testing.T) {
	dir := t.TempDir()
	p, path := newTraePanel(t, dir)
	cfg := trae.Config{
		Enabled: true, ChatBase: trae.DefaultChatBase,
		Accounts: []trae.Account{{UID: "u-1", AccessToken: "t1"}},
	}
	writeTraeTestConfig(t, path, &cfg)

	rec := traeDo(p, http.MethodPost, "/panel/api/trae_config",
		`{"enabled":true,"chat_base":"`+trae.DefaultChatBase+`","accounts":["u-1"],"disabled":["u-1"]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body)
	}
	saved, _ := trae.Load(path, "")
	if saved == nil || len(saved.Accounts) != 1 || !saved.Accounts[0].Disabled {
		t.Fatalf("禁用未生效: %+v", saved)
	}
}

// TestTraeConfigSaveRejectsInvalid 非法配置拒绝且不落盘。
func TestTraeConfigSaveRejectsInvalid(t *testing.T) {
	dir := t.TempDir()
	p, path := newTraePanel(t, dir)
	rec := traeDo(p, http.MethodPost, "/panel/api/trae_config",
		`{"enabled":true,"chat_base":"no-scheme","accounts":[]}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("非法配置不应落盘")
	}
}

// TestTraeConfigUnavailable 未注入路径时 501。
func TestTraeConfigUnavailable(t *testing.T) {
	p := New(Config{Version: "test"})
	req := httptest.NewRequest(http.MethodGet, "/panel/api/trae_config", nil)
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("status = %d, want 501", rec.Code)
	}
}

// TestTraeLoginStart 生成授权 URL（含 PKCE 与设备标识），并登记会话。
func TestTraeLoginStart(t *testing.T) {
	dir := t.TempDir()
	p, _ := newTraePanel(t, dir)
	rec := traeDo(p, http.MethodPost, "/panel/api/trae/login/start", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body)
	}
	var got struct {
		URL      string `json:"url"`
		State    string `json:"state"`
		DeviceID string `json:"device_id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(got.URL, trae.OAuthAuthorizeURL) {
		t.Errorf("授权 URL = %q", got.URL)
	}
	if !strings.Contains(got.URL, "code_challenge=") || !strings.Contains(got.URL, "login_channel=native_ide") {
		t.Errorf("授权 URL 缺关键参数: %s", got.URL)
	}
	if got.State == "" || got.DeviceID == "" {
		t.Errorf("state/device_id 缺失: %+v", got)
	}
	// 会话已登记（state 可取出）
	if sess := takeTraeLogin(got.State); sess == nil {
		t.Error("登录会话未登记")
	}
}

// TestTraeLoginFinishRejectsUnknownState 未知/过期的 loginTraceID 必须拒绝（CSRF 防护）。
func TestTraeLoginFinishRejectsUnknownState(t *testing.T) {
	dir := t.TempDir()
	p, _ := newTraePanel(t, dir)
	rec := traeDo(p, http.MethodPost, "/panel/api/trae/login/finish",
		`{"callback_url":"http://127.0.0.1:17388/authorize?authCodeInfo=AC&loginTraceID=not-mine"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400（body=%s）", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "loginTraceID") {
		t.Errorf("错误信息应点明 traceID 不匹配: %s", rec.Body)
	}
}

// TestTraeImportRejectsBadCipher 非法密文必须 400（而不是静默写入坏凭证）。
func TestTraeImportRejectsBadCipher(t *testing.T) {
	dir := t.TempDir()
	p, path := newTraePanel(t, dir)
	cfg := trae.Config{
		Enabled: true, ChatBase: trae.DefaultChatBase,
		Accounts: []trae.Account{{UID: "u-1", AccessToken: "t1"}},
	}
	writeTraeTestConfig(t, path, &cfg)

	rec := traeDo(p, http.MethodPost, "/panel/api/trae/import",
		`{"uid":"u-1","device_id":"123","icube_value":"bm90LWEtdGMtZW52ZWxvcGU="}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400（body=%s）", rec.Code, rec.Body)
	}
	// 账号凭证未被污染
	saved, _ := trae.Load(path, "")
	if saved.Accounts[0].PrivateKeyPEM != "" {
		t.Error("导入失败不应写入凭证")
	}
}

// TestTraeImportRequiresKnownUID 未知 uid 必须拒绝（不凭空造账号）。
func TestTraeImportRequiresKnownUID(t *testing.T) {
	dir := t.TempDir()
	p, path := newTraePanel(t, dir)
	writeTraeTestConfig(t, path, &trae.Config{Enabled: true, ChatBase: trae.DefaultChatBase})
	rec := traeDo(p, http.MethodPost, "/panel/api/trae/import",
		`{"uid":"nope","storage_json":"{}"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

// TestTraeRemoveAccount 删除账号并落盘。
func TestTraeRemoveAccount(t *testing.T) {
	dir := t.TempDir()
	p, path := newTraePanel(t, dir)
	cfg := trae.Config{
		Enabled: true, ChatBase: trae.DefaultChatBase,
		Accounts: []trae.Account{{UID: "u-1", AccessToken: "t1"}, {UID: "u-2", AccessToken: "t2"}},
	}
	writeTraeTestConfig(t, path, &cfg)

	rec := traeDo(p, http.MethodPost, "/panel/api/trae/accounts/u-1/remove", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body)
	}
	saved, _ := trae.Load(path, "")
	if len(saved.Accounts) != 1 || saved.Accounts[0].UID != "u-2" {
		t.Fatalf("删除结果不符: %+v", saved.Accounts)
	}
	// 删除不存在的账号 → 404
	if rec := traeDo(p, http.MethodPost, "/panel/api/trae/accounts/nope/remove", ""); rec.Code != http.StatusNotFound {
		t.Errorf("删除不存在账号 status = %d, want 404", rec.Code)
	}
}

// TestTraeLoginStartExplicitDevice 显式指定设备号：URL 与响应都应带上它，
// device_source=explicit、且不标 generated。
func TestTraeLoginStartExplicitDevice(t *testing.T) {
	dir := t.TempDir()
	p, _ := newTraePanel(t, dir)
	rec := traeDo(p, http.MethodPost, "/panel/api/trae/login/start",
		`{"device_id":"1234567890123456"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body)
	}
	var got struct {
		URL             string `json:"url"`
		DeviceID        string `json:"device_id"`
		DeviceSource    string `json:"device_source"`
		DeviceGenerated bool   `json:"device_generated"`
		Warning         string `json:"warning"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.DeviceID != "1234567890123456" {
		t.Errorf("device_id = %q", got.DeviceID)
	}
	if got.DeviceSource != "explicit" || got.DeviceGenerated {
		t.Errorf("source = %q generated = %v", got.DeviceSource, got.DeviceGenerated)
	}
	if got.Warning != "" {
		t.Errorf("显式设备号不应有警告: %q", got.Warning)
	}
	if !strings.Contains(got.URL, "device_id=1234567890123456") {
		t.Errorf("授权 URL 缺 device_id: %s", got.URL)
	}
}

// TestTraeLoginStartRejectsBadDevice 非法设备号 400（含字母 / 长度越界）。
func TestTraeLoginStartRejectsBadDevice(t *testing.T) {
	dir := t.TempDir()
	p, _ := newTraePanel(t, dir)
	for _, dev := range []string{"abc123456789", "123", "123456789012345678901"} {
		rec := traeDo(p, http.MethodPost, "/panel/api/trae/login/start",
			`{"device_id":"`+dev+`"}`)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("device_id=%q status = %d, want 400", dev, rec.Code)
		}
	}
}

// TestTraeLoginStartGeneratedWarns 未指定且无已导入设备时生成占位号，
// 必须显式告警（占位号签到必被拒成 9074）。
func TestTraeLoginStartGeneratedWarns(t *testing.T) {
	dir := t.TempDir()
	p, _ := newTraePanel(t, dir)
	rec := traeDo(p, http.MethodPost, "/panel/api/trae/login/start", "")
	var got struct {
		DeviceID        string `json:"device_id"`
		DeviceSource    string `json:"device_source"`
		DeviceGenerated bool   `json:"device_generated"`
		Warning         string `json:"warning"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if !got.DeviceGenerated || got.DeviceSource != "generated" {
		t.Errorf("应标记为生成号: %+v", got)
	}
	if !strings.Contains(got.Warning, "9074") {
		t.Errorf("警告应点明 9074: %q", got.Warning)
	}
	if !trae.IsValidDeviceID(got.DeviceID) {
		t.Errorf("占位号形态非法: %q", got.DeviceID)
	}
}

// TestTraeConfigIncludesCheckinStatus 账号视图应带今日签到状态（前端据此显示
// 「已签」而不是始终显示「签到」）。
func TestTraeConfigIncludesCheckinStatus(t *testing.T) {
	dir := t.TempDir()
	p, path := newTraePanel(t, dir)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":0,"checked_in":true}`))
	}))
	defer srv.Close()

	cfg := &trae.Config{
		Enabled: true, ChatBase: trae.DefaultChatBase, APITraeBase: srv.URL,
		Accounts: []trae.Account{{
			UID: "u-1", AccessToken: "t1", DeviceID: "1234567890123456",
		}},
	}
	writeTraeTestConfig(t, path, cfg)

	rec := traeDo(p, http.MethodGet, "/panel/api/trae_config", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body)
	}
	var got struct {
		Config struct {
			Accounts []struct {
				UID                  string `json:"uid"`
				CheckinDone          *bool  `json:"checkin_done"`
				CheckinDeviceAlready bool   `json:"checkin_device_already"`
				CheckinError         string `json:"checkin_error"`
			} `json:"accounts"`
		} `json:"config"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Config.Accounts) != 1 {
		t.Fatalf("账号数 = %d", len(got.Config.Accounts))
	}
	a := got.Config.Accounts[0]
	if a.CheckinDone == nil || !*a.CheckinDone {
		t.Errorf("checkin_done 应为 true: %+v", a)
	}
	if a.CheckinError != "" {
		t.Errorf("不应有查询错误: %s", a.CheckinError)
	}
}

// TestTraeConfigCheckinStatusErrorIsReported 查询失败时必须留下原因，
// 不能静默当作"未签到"（否则用户会反复点签到）。
func TestTraeConfigCheckinStatusErrorIsReported(t *testing.T) {
	dir := t.TempDir()
	p, path := newTraePanel(t, dir)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"message":"token expired"}`))
	}))
	defer srv.Close()

	cfg := &trae.Config{
		Enabled: true, ChatBase: trae.DefaultChatBase, APITraeBase: srv.URL,
		Accounts: []trae.Account{{
			UID: "u-1", AccessToken: "t1", DeviceID: "1234567890123456",
		}},
	}
	writeTraeTestConfig(t, path, cfg)

	rec := traeDo(p, http.MethodGet, "/panel/api/trae_config", "")
	var got struct {
		Config struct {
			Accounts []struct {
				CheckinDone  *bool  `json:"checkin_done"`
				CheckinError string `json:"checkin_error"`
			} `json:"accounts"`
		} `json:"config"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	a := got.Config.Accounts[0]
	if a.CheckinDone != nil {
		t.Error("查询失败时 checkin_done 应为 nil（未知），不能当作未签到")
	}
	if a.CheckinError == "" {
		t.Error("应记录查询失败原因")
	}
}

// TestTraeConfigSkipCheckinQuery no_checkin=1 时不打上游（供测试/离线场景）。
func TestTraeConfigSkipCheckinQuery(t *testing.T) {
	dir := t.TempDir()
	p, path := newTraePanel(t, dir)

	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		_, _ = w.Write([]byte(`{"code":0,"checked_in":true}`))
	}))
	defer srv.Close()

	cfg := &trae.Config{
		Enabled: true, ChatBase: trae.DefaultChatBase, APITraeBase: srv.URL,
		Accounts: []trae.Account{{UID: "u-1", AccessToken: "t1", DeviceID: "1234567890123456"}},
	}
	writeTraeTestConfig(t, path, cfg)

	if rec := traeDo(p, http.MethodGet, "/panel/api/trae_config?no_checkin=1", ""); rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if hits != 0 {
		t.Errorf("no_checkin=1 时不应请求上游，实际 %d 次", hits)
	}
}

// TestTraeCheckinStatusCache 30 秒内重复拉配置不应重复打上游；
// 签到成功后缓存失效，下次必须重新查。
func TestTraeCheckinStatusCache(t *testing.T) {
	dir := t.TempDir()
	p, path := newTraePanel(t, dir)

	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":0,"checked_in":false}`))
	}))
	defer srv.Close()

	cfg := &trae.Config{
		Enabled: true, ChatBase: trae.DefaultChatBase, APITraeBase: srv.URL,
		Accounts: []trae.Account{{UID: "u-1", AccessToken: "t1", DeviceID: "1234567890123456"}},
	}
	writeTraeTestConfig(t, path, cfg)

	for i := 0; i < 3; i++ {
		if rec := traeDo(p, http.MethodGet, "/panel/api/trae_config", ""); rec.Code != http.StatusOK {
			t.Fatalf("第 %d 次状态 = %d", i+1, rec.Code)
		}
	}
	if hits != 1 {
		t.Errorf("30 秒内重复拉配置应复用缓存，实际请求上游 %d 次", hits)
	}

	p.invalidateCheckinCache()
	if rec := traeDo(p, http.MethodGet, "/panel/api/trae_config", ""); rec.Code != http.StatusOK {
		t.Fatalf("失效后状态 = %d", rec.Code)
	}
	if hits != 2 {
		t.Errorf("缓存失效后应重新查询，实际请求 %d 次", hits)
	}
}
