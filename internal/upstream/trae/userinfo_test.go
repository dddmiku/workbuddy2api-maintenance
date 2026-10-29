// ═══ 更新日志 ═══
// 2026-10-29：新增。锁住 Trae 账号手机号的取用与展示口径。
package trae

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"workbuddy2api/internal/auth"
)

// TestGetUserInfoParsesMaskedMobile GetUserInfo 的真实响应形态（2026-10-29 生产实测）。
//
// 手机号在 NonPlainTextMobile 里，已是掩码形态（138******00）——上游不下发明文，
// 这是 passport 侧的隐私策略。面板只需要「能区分账号」，掩码足够。
func TestGetUserInfoParsesMaskedMobile(t *testing.T) {
	const body = `{"ResponseMetadata":{"Action":"","OID":null,"Region":""},
 "Result":{"AIRegion":"CN","Gender":0,"LastLoginTime":"2026-10-29T12:44:57+08:00",
 "LastLoginType":"sms","NonPlainTextEmail":"","NonPlainTextMobile":"138******00",
 "Region":"CN","RegisterTime":"2026-10-11T13:48:38.121+08:00",
 "ScreenName":"用户6126656924","TenantID":"7o2d894p7dr0o4","UserID":"411685466945619"}}`

	var gotPath, gotAuth, gotUID string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		gotUID = r.Header.Get("x-uid")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	defer server.Close()

	prev := TraeAPIHost
	TraeAPIHost = server.URL
	defer func() { TraeAPIHost = prev }()

	info, err := GetUserInfo(context.Background(), &auth.Auth{
		UID: "411685466945619", AccessToken: "tok-abc", Domain: "api.trae.cn",
	})
	if err != nil {
		t.Fatalf("GetUserInfo: %v", err)
	}
	if gotPath != GetUserInfoPath {
		t.Fatalf("path=%s want=%s", gotPath, GetUserInfoPath)
	}
	if gotAuth != "Cloud-IDE-JWT tok-abc" {
		t.Fatalf("Authorization=%q", gotAuth)
	}
	if gotUID != "411685466945619" {
		t.Fatalf("x-uid=%q", gotUID)
	}
	if info.Mobile != "138******00" {
		t.Fatalf("Mobile=%q", info.Mobile)
	}
	if info.ScreenName != "用户6126656924" {
		t.Fatalf("ScreenName=%q", info.ScreenName)
	}
	if info.UserID != "411685466945619" {
		t.Fatalf("UserID=%q", info.UserID)
	}
}

// TestGetUserInfoHTTPErrorIsReported 上游非 200 时要把状态码与片段带出来，
// 否则面板只能看到一句「取资料失败」，排障时无从下手。
func TestGetUserInfoHTTPErrorIsReported(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"ResponseMetadata":{"Error":{"Code":"10303","Message":"Api Scope Forbidden."}}}`))
	}))
	defer server.Close()
	prev := TraeAPIHost
	TraeAPIHost = server.URL
	defer func() { TraeAPIHost = prev }()

	_, err := GetUserInfo(context.Background(), &auth.Auth{UID: "u", AccessToken: "t"})
	if err == nil {
		t.Fatal("want error on 403")
	}
	if !strings.Contains(err.Error(), "403") {
		t.Fatalf("error should carry status code: %v", err)
	}
}

// TestGetUserInfoNilAuth 空账号要报错而不是 panic（面板批量渲染时会遇到）。
func TestGetUserInfoNilAuth(t *testing.T) {
	if _, err := GetUserInfo(context.Background(), nil); err == nil {
		t.Fatal("nil auth must error")
	}
}

// TestGetUserInfoToleratesMissingFields 上游少字段时不能整体失败：
// 只拿到 UserID 也要能返回，手机号留空由前端回落。
func TestGetUserInfoToleratesMissingFields(t *testing.T) {
	payload := map[string]any{"Result": map[string]any{"UserID": "123"}}
	raw, _ := json.Marshal(payload)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(raw)
	}))
	defer server.Close()
	prev := TraeAPIHost
	TraeAPIHost = server.URL
	defer func() { TraeAPIHost = prev }()

	info, err := GetUserInfo(context.Background(), &auth.Auth{UID: "123", AccessToken: "t"})
	if err != nil {
		t.Fatalf("partial payload must not fail: %v", err)
	}
	if info.UserID != "123" || info.Mobile != "" {
		t.Fatalf("unexpected: %+v", info)
	}
}
