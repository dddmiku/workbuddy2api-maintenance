// Trae 账号展示名：优先手机号（2026-11-09 用户要求）。
//
// ═══ 需求 ═══
//
// 面板上 Trae 账号显示的是「用户99028827821」——上游自动生成的昵称，配上 15 位
// 内部数字 uid，运维完全看不出这是哪个号。用户要求换成手机号显示。
//
// ═══ 为什么手机号能对得上 ═══
//
// GetUserInfo 返回的 NonPlainTextMobile 是掩码手机号（157****3894），这是唯一
// 能把账号与「人」直接对应的标识。
//
// ═══ 边界 ═══
//
// 只对 trae 域生效。WorkBuddy 账号的 nickname 是用户真实设置的（如 "rmo43oa5wx"），
// 不该被手机号顶掉；没有 mobile 时保持原有回退链（nickname → uid → uid 前 8 位）。
package panel

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func writeTraeAuth(t *testing.T, dir, uid, nickname, mobile string) string {
	t.Helper()
	acct := map[string]any{"uid": uid, "nickname": nickname}
	if mobile != "" {
		acct["mobile"] = mobile
	}
	doc := map[string]any{
		"account": acct,
		"auth": map[string]any{
			"accessToken": "fake-token-not-a-credential",
			"realm":       "trae",
			"domain":      "api.trae.cn",
			"expiresAt":   9999999999,
		},
	}
	raw, _ := json.MarshalIndent(doc, "", "  ")
	path := filepath.Join(dir, "workbuddy-trae-"+uid+".json")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// 有手机号的 Trae 账号：主标题必须是手机号。
func TestTraeSummaryPrefersMobile(t *testing.T) {
	dir := t.TempDir()
	path := writeTraeAuth(t, dir, "411685466945619", "用户6126656924", "157****3894")

	got := Summary(AuthEntry{UID: "411685466945619", Path: path})
	if got.Nickname != "157****3894" {
		t.Fatalf("Nickname = %q want 手机号 157****3894（不该是上游自动昵称）", got.Nickname)
	}
	if got.Mobile != "157****3894" {
		t.Fatalf("Mobile 字段应保留原值，got %q", got.Mobile)
	}
}

// 没有手机号的 Trae 账号：保持原有回退链，不显示空。
func TestTraeSummaryWithoutMobileFallsBack(t *testing.T) {
	dir := t.TempDir()
	path := writeTraeAuth(t, dir, "411685466945619", "用户6126656924", "")

	got := Summary(AuthEntry{UID: "411685466945619", Path: path})
	if got.Nickname != "用户6126656924" {
		t.Fatalf("无手机号时应回落到上游昵称，got %q", got.Nickname)
	}
}

// WorkBuddy 账号即便有 mobile 字段也不该被顶掉（只有 trae 域走这条规则）。
func TestWorkBuddySummaryKeepsNickname(t *testing.T) {
	dir := t.TempDir()
	doc := map[string]any{
		"account": map[string]any{"uid": "0da1240d-7666", "nickname": "rmo43oa5wx", "mobile": "138****0000"},
		"auth":    map[string]any{"accessToken": "fake", "realm": "cn", "expiresAt": 9999999999},
	}
	raw, _ := json.MarshalIndent(doc, "", "  ")
	path := filepath.Join(dir, "workbuddy-0da1240d.json")
	os.WriteFile(path, raw, 0o600)

	got := Summary(AuthEntry{UID: "0da1240d-7666", Path: path})
	if got.Nickname != "rmo43oa5wx" {
		t.Fatalf("WorkBuddy 账号的昵称被手机号顶掉了: %q", got.Nickname)
	}
}
