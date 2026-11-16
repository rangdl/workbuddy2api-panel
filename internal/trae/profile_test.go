package trae

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestGetUserInfo 按 bluechonk 实测规格解析 Result.{UserID, ScreenName}。
func TestGetUserInfo(t *testing.T) {
	var gotPath, gotToken, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotToken = r.Header.Get("x-cloudide-token")
		b, _ := readAll(r)
		gotBody = string(b)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"Result": map[string]any{"UserID": "4175318908939818", "ScreenName": "猫猫"},
		})
	}))
	defer srv.Close()

	cfg := &Config{ChatBase: srv.URL, OAuthHost: srv.URL, IDEVersion: "0.1.52"}
	client := NewClient()
	info, err := client.GetUserInfo(context.Background(), cfg, &Account{
		UID: "u1", AccessToken: "Cloud-IDE-JWT abc.def.ghi",
	})
	if err != nil {
		t.Fatalf("GetUserInfo: %v", err)
	}
	if info.Nickname != "猫猫" || info.UID != "4175318908939818" {
		t.Errorf("解析不符: %+v", info)
	}
	// 端点与头：必须走 OAuth 域的 GetUserInfo，token 不带前缀。
	if gotPath != EndpointGetUserInfo {
		t.Errorf("path = %q, want %q", gotPath, EndpointGetUserInfo)
	}
	if gotToken != "abc.def.ghi" {
		t.Errorf("x-cloudide-token = %q（应为裸 token，不带 Cloud-IDE-JWT 前缀）", gotToken)
	}
	// 请求体要带 ReqSource 与 IDEVersion。
	var body map[string]any
	_ = json.Unmarshal([]byte(gotBody), &body)
	if body["ReqSource"] != "IDE" {
		t.Errorf("ReqSource = %v", body["ReqSource"])
	}
	if body["IDEVersion"] != "0.1.52" {
		t.Errorf("IDEVersion = %v", body["IDEVersion"])
	}
}

// TestGetUserInfoLowercaseResult 兼容小写 result 键。
func TestGetUserInfoLowercaseResult(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"result":{"userId":"9","nickname":"小号"}}`))
	}))
	defer srv.Close()
	cfg := &Config{ChatBase: srv.URL, OAuthHost: srv.URL}
	info, err := NewClient().GetUserInfo(context.Background(), cfg, &Account{UID: "u", AccessToken: "t"})
	if err != nil {
		t.Fatal(err)
	}
	if info.UID != "9" || info.Nickname != "小号" {
		t.Errorf("小写键解析不符: %+v", info)
	}
}

// TestGetUserInfoBusinessError ResponseMetadata.Error 要当错误返回，
// 而不是静默解出空昵称（那会让上层以为"这个号没昵称"）。
func TestGetUserInfoBusinessError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ResponseMetadata":{"Error":{"Message":"token invalid"}}}`))
	}))
	defer srv.Close()
	cfg := &Config{ChatBase: srv.URL, OAuthHost: srv.URL}
	if _, err := NewClient().GetUserInfo(context.Background(), cfg, &Account{UID: "u", AccessToken: "t"}); err == nil {
		t.Fatal("业务错误应返回 error")
	}
}

// TestGetUserInfoHTTPError HTTP 4xx 要报错。
func TestGetUserInfoHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()
	cfg := &Config{ChatBase: srv.URL, OAuthHost: srv.URL}
	if _, err := NewClient().GetUserInfo(context.Background(), cfg, &Account{UID: "u", AccessToken: "t"}); err == nil {
		t.Fatal("401 应返回 error")
	}
}

// TestGetUserInfoNoToken 无 token 直接报错，不打上游。
func TestGetUserInfoNoToken(t *testing.T) {
	cfg := &Config{ChatBase: "http://127.0.0.1:1"}
	if _, err := NewClient().GetUserInfo(context.Background(), cfg, &Account{UID: "u"}); err == nil {
		t.Fatal("无 token 应报错")
	}
}

func readAll(r *http.Request) ([]byte, error) {
	defer r.Body.Close()
	buf := make([]byte, 0, 512)
	tmp := make([]byte, 512)
	for {
		n, err := r.Body.Read(tmp)
		buf = append(buf, tmp[:n]...)
		if err != nil {
			return buf, nil
		}
	}
}
