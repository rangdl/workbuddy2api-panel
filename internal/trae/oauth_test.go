package trae

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// TestPKCEPair PKCE：verifier 长度合规（43–128），challenge = BASE64URL-NOPAD(SHA256(verifier))。
func TestPKCEPair(t *testing.T) {
	verifier, challenge, err := PKCEPair()
	if err != nil {
		t.Fatal(err)
	}
	if len(verifier) < 43 || len(verifier) > 128 {
		t.Fatalf("verifier 长度 %d 超出 RFC 7636 范围", len(verifier))
	}
	sum := sha256.Sum256([]byte(verifier))
	want := base64.RawURLEncoding.EncodeToString(sum[:])
	if challenge != want {
		t.Fatalf("challenge 不匹配:\n got=%s\nwant=%s", challenge, want)
	}
	if strings.ContainsAny(challenge, "+/=") {
		t.Errorf("challenge 必须是 base64url 无填充: %s", challenge)
	}
	// 两次调用必须不同（随机性）
	v2, _, _ := PKCEPair()
	if v2 == verifier {
		t.Error("两次 PKCE 不应相同")
	}
}

// TestBuildLoginURL 授权 URL 参数齐备（缺项会让授权页不回跳）。
func TestBuildLoginURL(t *testing.T) {
	urlStr, sess, err := BuildLoginURL(LoginURLOptions{DeviceID: "123456789012345", MachineID: "m-1"})
	if err != nil {
		t.Fatal(err)
	}
	if sess.State == "" || sess.PKCEVerifier == "" {
		t.Fatalf("会话字段缺失: %+v", sess)
	}
	u, err := url.Parse(urlStr)
	if err != nil {
		t.Fatal(err)
	}
	if u.Host != "www.trae.cn" || u.Path != "/authorization" {
		t.Fatalf("授权页地址不符: %s", urlStr)
	}
	q := u.Query()
	checks := map[string]string{
		"login_channel":         "native_ide",
		"auth_type":             "local",
		"client_id":             DefaultClientID,
		"device_id":             "123456789012345",
		"machine_id":            "m-1",
		"code_challenge_method": "S256",
		"login_trace_id":        sess.State,
	}
	for k, want := range checks {
		if got := q.Get(k); got != want {
			t.Errorf("参数 %s = %q, want %q", k, got, want)
		}
	}
	if q.Get("code_challenge") == "" {
		t.Error("缺少 code_challenge")
	}
	if q.Get("auth_callback_url") == "" {
		t.Error("缺少 auth_callback_url")
	}
	// 缺设备标识必须报错（否则上游 20403）
	if _, _, err := BuildLoginURL(LoginURLOptions{}); err == nil {
		t.Error("缺设备标识应报错")
	}
}

// TestParseAuthCallback 回调解析：authCodeInfo（JSON）/ 裸串 / code 参数三种形态。
func TestParseAuthCallback(t *testing.T) {
	cases := []struct {
		name      string
		raw       string
		wantCode  string
		wantTrace string
		wantErr   bool
	}{
		{
			name:      "authCodeInfo JSON",
			raw:       `http://127.0.0.1:17388/authorize?authCodeInfo=%7B%22AuthCode%22%3A%22AC-1%22%7D&loginTraceID=trace-1`,
			wantCode:  "AC-1",
			wantTrace: "trace-1",
		},
		{
			name:     "authCodeInfo 裸串",
			raw:      `http://127.0.0.1:17388/authorize?authCodeInfo=AC-2`,
			wantCode: "AC-2",
		},
		{
			name:     "标准 code 参数",
			raw:      `http://127.0.0.1:17388/authorize?code=AC-3`,
			wantCode: "AC-3",
		},
		{
			name:    "无任何 code",
			raw:     `http://127.0.0.1:17388/authorize?foo=bar`,
			wantErr: true,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			code, trace, err := ParseAuthCallback(c.raw)
			if c.wantErr {
				if err == nil {
					t.Fatalf("应报错，得到 code=%q", code)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if code != c.wantCode {
				t.Errorf("authCode = %q, want %q", code, c.wantCode)
			}
			if trace != c.wantTrace {
				t.Errorf("traceID = %q, want %q", trace, c.wantTrace)
			}
		})
	}
}

// newExchangeServer 假 ExchangeToken 上游：按请求体里的字段决定成功/失败。
func newExchangeServer(t *testing.T, ok func(body map[string]any) bool) (*httptest.Server, *[]map[string]any) {
	t.Helper()
	var seen []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		seen = append(seen, body)
		w.Header().Set("Content-Type", "application/json")
		if ok(body) {
			_, _ = w.Write([]byte(`{"ResponseMetadata":{},"Result":{"Token":"jwt-new","RefreshToken":"rt-new","ExpiresAt":1753600000}}`))
			return
		}
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"ResponseMetadata":{"Error":{"Code":"20405","Message":"Device proof required"}}}`))
	}))
	t.Cleanup(srv.Close)
	return srv, &seen
}

// TestExchangeAuthCodeWithDeviceInfo 有设备凭证时走主变体（DeviceInfo + 公钥，无 proof）。
func TestExchangeAuthCodeWithDeviceInfo(t *testing.T) {
	srv, seen := newExchangeServer(t, func(body map[string]any) bool {
		return body["DeviceInfo"] != nil
	})
	key := newTestKey(t)
	cred := &DeviceCredential{DeviceID: "dev-1", MachineID: "m-1", PrivateKeyPEM: pkcs8PEM(t, key)}
	cfg := &Config{ChatBase: srv.URL, OAuthHost: srv.URL, AppID: DefaultAppID}

	pair, err := NewClient().ExchangeAuthCode(context.Background(), cfg, "AC-1", "verifier-1", cred, "dev-1", "m-1")
	if err != nil {
		t.Fatal(err)
	}
	if pair.AccessToken != "jwt-new" || pair.RefreshToken != "rt-new" {
		t.Fatalf("token 提取不符: %+v", pair)
	}
	if pair.ExpiresAt != 1753600000 {
		t.Errorf("ExpiresAt = %d", pair.ExpiresAt)
	}
	first := (*seen)[0]
	if first["AuthCode"] != "AC-1" || first["CodeVerifier"] != "verifier-1" {
		t.Errorf("请求体不符: %v", first)
	}
	di, _ := first["DeviceInfo"].(map[string]any)
	if di == nil || di["DevicePublicKey"] == nil || di["DevicePublicKey"] == "" {
		t.Errorf("DeviceInfo 缺 DevicePublicKey: %v", first["DeviceInfo"])
	}
	if first["DeviceProof"] != nil {
		t.Error("AuthCode 场景不应携带 DeviceProof（那是 refresh 场景专属）")
	}
}

// TestExchangeAuthCodeFallbackVariant 无设备凭证时回落兜底变体（只带 DeviceID）。
func TestExchangeAuthCodeFallbackVariant(t *testing.T) {
	srv, seen := newExchangeServer(t, func(body map[string]any) bool {
		return body["DeviceInfo"] == nil // 只接受兜底变体
	})
	cfg := &Config{ChatBase: srv.URL, OAuthHost: srv.URL, AppID: DefaultAppID}

	pair, err := NewClient().ExchangeAuthCode(context.Background(), cfg, "AC-2", "v", nil, "dev-9", "m-9")
	if err != nil {
		t.Fatal(err)
	}
	if pair.AccessToken != "jwt-new" {
		t.Fatalf("token = %q", pair.AccessToken)
	}
	if len(*seen) == 0 {
		t.Fatal("未发起请求")
	}
	last := (*seen)[len(*seen)-1]
	if last["DeviceID"] != "dev-9" {
		t.Errorf("兜底变体应带 DeviceID: %v", last)
	}
}

// TestExchangeAuthCodeAllVariantsFail 全部变体失败 → 聚合错误含各变体原因。
func TestExchangeAuthCodeAllVariantsFail(t *testing.T) {
	srv, _ := newExchangeServer(t, func(map[string]any) bool { return false })
	cfg := &Config{ChatBase: srv.URL, OAuthHost: srv.URL, AppID: DefaultAppID}
	_, err := NewClient().ExchangeAuthCode(context.Background(), cfg, "AC", "v", nil, "d", "m")
	if err == nil {
		t.Fatal("应返回错误")
	}
	if !strings.Contains(err.Error(), "20405") {
		t.Errorf("错误应含上游 code: %v", err)
	}
}

// TestExchangeRefreshWithProof 有私钥时首个变体即 DeviceProof（含 Signature/Timestamp/Nonce）。
func TestExchangeRefreshWithProof(t *testing.T) {
	srv, seen := newExchangeServer(t, func(body map[string]any) bool {
		return body["DeviceProof"] != nil
	})
	key := newTestKey(t)
	acc := &Account{UID: "u1", RefreshToken: "rt-old", DeviceID: "dev-1", PrivateKeyPEM: pkcs8PEM(t, key)}
	cfg := &Config{ChatBase: srv.URL, OAuthHost: srv.URL, AppID: DefaultAppID}

	pair, err := NewClient().ExchangeRefresh(context.Background(), cfg, acc)
	if err != nil {
		t.Fatal(err)
	}
	if pair.AccessToken != "jwt-new" || pair.RefreshToken != "rt-new" {
		t.Fatalf("token 不符: %+v", pair)
	}
	proof, _ := (*seen)[0]["DeviceProof"].(map[string]any)
	if proof == nil {
		t.Fatalf("首变体应带 DeviceProof: %v", (*seen)[0])
	}
	for _, k := range []string{"Signature", "Timestamp", "Nonce"} {
		if proof[k] == nil {
			t.Errorf("DeviceProof 缺字段 %s: %v", k, proof)
		}
	}
	if _, ok := proof["Timestamp"].(float64); !ok {
		t.Errorf("Timestamp 必须是数值: %T", proof["Timestamp"])
	}
}

// TestExchangeRefreshLegacyFallback 无私钥时回落 Legacy 变体（ClientSecret，无 proof）。
func TestExchangeRefreshLegacyFallback(t *testing.T) {
	srv, seen := newExchangeServer(t, func(body map[string]any) bool {
		return body["DeviceProof"] == nil && body["ClientSecret"] != nil
	})
	acc := &Account{UID: "u1", RefreshToken: "rt-old", DeviceID: "dev-1"}
	cfg := &Config{ChatBase: srv.URL, OAuthHost: srv.URL, AppID: DefaultAppID}

	pair, err := NewClient().ExchangeRefresh(context.Background(), cfg, acc)
	if err != nil {
		t.Fatal(err)
	}
	if pair.AccessToken != "jwt-new" {
		t.Fatalf("token = %q", pair.AccessToken)
	}
	last := (*seen)[len(*seen)-1]
	if last["RefreshToken"] != "rt-old" {
		t.Errorf("Legacy 变体应带 RefreshToken: %v", last)
	}
}

// TestExchangeRefreshNoToken 无 refresh_token 直接报错。
func TestExchangeRefreshNoToken(t *testing.T) {
	_, err := NewClient().ExchangeRefresh(context.Background(), &Config{OAuthHost: "http://127.0.0.1:1"}, &Account{UID: "u1"})
	if err == nil {
		t.Error("无 refresh_token 应报错")
	}
}

// makeJWT 构造一个仅含 payload 的假 JWT（测试用，不校验签名）。
func makeJWT(payload map[string]any) string {
	b, _ := json.Marshal(payload)
	head := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none"}`))
	body := base64.RawURLEncoding.EncodeToString(b)
	return head + "." + body + ".sig"
}

// TestExpiryFromJWT exp 解析（含前缀、带填充、自定义字段三种形态）。
func TestExpiryFromJWT(t *testing.T) {
	tok := makeJWT(map[string]any{"exp": 1753600000})
	if got := ExpiryFromJWT(tok); got != 1753600000 {
		t.Errorf("exp 解析 = %d", got)
	}
	if got := ExpiryFromJWT("Cloud-IDE-JWT " + tok); got != 1753600000 {
		t.Errorf("带前缀解析 = %d", got)
	}
	if got := ExpiryFromJWT(makeJWT(map[string]any{"expires_at_ms": 1753600000000})); got != 1753600000 {
		t.Errorf("毫秒字段解析 = %d", got)
	}
	for _, bad := range []string{"", "not-a-jwt", "a.b", "x.y.z"} {
		if got := ExpiryFromJWT(bad); got != 0 {
			t.Errorf("非法 token %q 应返回 0，得到 %d", bad, got)
		}
	}
}

// TestJWTUserID user_id 提取（顶层键 + data 嵌套）。
func TestJWTUserID(t *testing.T) {
	if got := JWTUserID(makeJWT(map[string]any{"user_id": "u-42"})); got != "u-42" {
		t.Errorf("顶层 user_id = %q", got)
	}
	if got := JWTUserID(makeJWT(map[string]any{"data": map[string]any{"id": "u-99"}})); got != "u-99" {
		t.Errorf("data.id = %q", got)
	}
	if got := JWTUserID("garbage"); got != "" {
		t.Errorf("非法 token 应返回空串，得到 %q", got)
	}
}

// TestNeedsRefresh 过期判定：未知过期时间宁可多刷一次。
func TestNeedsRefresh(t *testing.T) {
	now := time.Now()
	cases := []struct {
		name string
		acc  Account
		want bool
	}{
		{"无 token", Account{}, true},
		{"未知过期", Account{AccessToken: "t"}, true},
		{"即将过期", Account{AccessToken: "t", ExpiresAt: now.Add(time.Minute).Unix()}, true},
		{"还很久", Account{AccessToken: "t", ExpiresAt: now.Add(time.Hour).Unix()}, false},
		{"已过期", Account{AccessToken: "t", ExpiresAt: now.Add(-time.Hour).Unix()}, true},
	}
	for _, c := range cases {
		if got := c.acc.NeedsRefresh(10 * time.Minute); got != c.want {
			t.Errorf("%s: NeedsRefresh = %v, want %v", c.name, got, c.want)
		}
	}
}
