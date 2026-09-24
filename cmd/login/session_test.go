// ═══ 更新日志 ═══
// 2026-09-25：锁定并行扫码登录状态隔离和非法流程ID拒绝。
package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoginFlowStateIsolation(t *testing.T) {
	t.Setenv("WB2API_LOGIN_STATE_DIR", t.TempDir())
	one, _ := loginStatePath("cn", strings.Repeat("a", 32))
	two, _ := loginStatePath("cn", strings.Repeat("b", 32))
	global, _ := loginStatePath("global", strings.Repeat("a", 32))
	if one == two || one == global {
		t.Fatal("parallel login flows share state")
	}
	if err := writeLoginState(one, []byte(`{"state":"one"}`)); err != nil {
		t.Fatal(err)
	}
	if err := writeLoginState(two, []byte(`{"state":"two"}`)); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(one)
	if !strings.Contains(string(raw), "one") {
		t.Fatal("parallel flow overwrote state")
	}
	for _, id := range []string{"../escape", "short", strings.Repeat("a", 31) + "/"} {
		if _, _, err := parseSessionArgs([]string{"--realm=cn", "--session=" + id, "poll"}); err == nil {
			t.Fatalf("invalid id accepted: %s", id)
		}
	}
	args, flow, err := parseSessionArgs([]string{"--realm=global", "--session=" + strings.Repeat("c", 32), "url"})
	if err != nil || flow != strings.Repeat("c", 32) || strings.Join(args, " ") != "--realm=global url" {
		t.Fatal("valid session flags rejected")
	}
	if filepath.Ext(one) != ".json" {
		t.Fatal("unexpected state filename")
	}
}
