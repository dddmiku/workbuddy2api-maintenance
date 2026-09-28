// ═══ 更新日志 ═══
// 2026-09-18：复现交接失败仍改写重启指针的问题，防止热更新失败后重启到坏版本。
package hotupdate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestManagerFailedHandoverPreservesRestartBinary(t *testing.T) {
	requireListenerInheritance(t)
	dir := t.TempDir()
	oldBinary := filepath.Join(dir, "working-version")
	if err := os.WriteFile(oldBinary, []byte("old binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := writeCurrentPointer(dir, oldBinary); err != nil {
		t.Fatal(err)
	}
	running := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("old-ok"))
	}))
	defer running.Close()

	// 摘要正确但不可执行：实际下载链路成功，真实 exec 交接失败。
	content := []byte("not an executable image\n")
	sum := sha256.Sum256(content)
	name, err := assetName()
	if err != nil {
		t.Fatal(err)
	}
	var releaseServer *httptest.Server
	releaseServer = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/asset" {
			_, _ = w.Write(content)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"tag_name": "v99.0.0",
			"assets": []map[string]any{{
				"name": name, "size": len(content),
				"browser_download_url": releaseServer.URL + "/asset",
				"digest":               "sha256:" + hex.EncodeToString(sum[:]),
			}},
		})
	}))
	defer releaseServer.Close()

	switched := false
	manager := NewManager(Options{
		Enabled: true, Dir: dir, Listener: running.Listener,
		OnSwitched: func() { switched = true },
	})
	manager.client.APIBase = releaseServer.URL
	status, err := manager.Apply(context.Background(), "")
	if err == nil || status.State != StateFailed {
		t.Fatalf("failed executable must fail the update: status=%+v err=%v", status, err)
	}
	if switched {
		t.Fatal("failed handover must not drain the running instance")
	}
	if got := CurrentBinary(dir); got != oldBinary {
		t.Fatalf("failed handover changed restart binary to %q; want %q", got, oldBinary)
	}
	if got := dialBody(t, running.Listener.Addr().String()); got != "old-ok" {
		t.Fatalf("old instance stopped serving after failed handover: %q", got)
	}
}

// TestAvailabilityFollowsRepoVisibility 热更新的可用性由「发布仓库是否开放」决定，
// 而不是一个需要人工同步的开关。四种组合都要如实反映，不能只报 enabled。
func TestAvailabilityFollowsRepoVisibility(t *testing.T) {
	cases := []struct {
		name       string
		enabled    bool
		visibility Visibility
		token      string
		wantAvail  bool
	}{
		{"公开仓库可用", true, VisibilityPublic, "", true},
		{"公开仓库带令牌仍可用", true, VisibilityPublic, "t", true},
		{"私有仓库无令牌不可用", true, VisibilityPrivate, "", false},
		{"私有仓库配了令牌可用", true, VisibilityPrivate, "t", true},
		{"可见性未确认时先不声称可用", true, VisibilityUnknown, "", false},
		{"配置关掉则一律不可用", false, VisibilityPublic, "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := NewManager(Options{Enabled: tc.enabled, Token: tc.token, Dir: t.TempDir()})
			m.visibility = tc.visibility
			m.mu.Lock()
			available, reason := m.availabilityLocked()
			status := m.statusLocked()
			m.mu.Unlock()
			if available != tc.wantAvail {
				t.Fatalf("available=%v want %v (reason=%q)", available, tc.wantAvail, reason)
			}
			if status.Visibility != tc.visibility {
				t.Fatalf("status visibility=%q want %q", status.Visibility, tc.visibility)
			}
			if !available && strings.TrimSpace(reason) == "" {
				t.Fatal("不可用时必须给出原因，否则面板只能显示一个无法解释的失败")
			}
			if available && strings.TrimSpace(reason) != "" {
				t.Fatalf("可用时不应带不可用原因: %q", reason)
			}
			// UpdateReady 必须服从可用性：不可用时绝不能显示"可升级"。
			if !available && status.UpdateReady {
				t.Fatal("不可用时不得报告 update_ready")
			}
		})
	}
}

// TestRepoVisibilityProbe 探测语义：匿名 200 = 公开；匿名 404 + 令牌 200 = 私有；
// 匿名 404 且无令牌 = 私有（无需再问）；非 200/404 = 未知。
func TestRepoVisibilityProbe(t *testing.T) {
	cases := []struct {
		name        string
		anonStatus  int
		authedStatus int
		token       string
		want        Visibility
	}{
		{"匿名可读即公开", 200, 0, "", VisibilityPublic},
		{"匿名 404 且无令牌判为私有", 404, 0, "", VisibilityPrivate},
		{"匿名 404 令牌 200 判为私有", 404, 200, "t", VisibilityPrivate},
		{"匿名 404 令牌也 404 判为未知", 404, 404, "t", VisibilityUnknown},
		{"服务器错误判为未知", 500, 0, "", VisibilityUnknown},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			requests := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests++
				status := tc.anonStatus
				if r.Header.Get("Authorization") != "" && tc.authedStatus != 0 {
					status = tc.authedStatus
				}
				w.WriteHeader(status)
				_, _ = w.Write([]byte(`{}`))
			}))
			defer server.Close()
			c := NewClient("owner/repo", tc.token)
			c.HTTP = server.Client()
			c.APIBase = server.URL
			if got := c.RepoVisibility(context.Background()); got != tc.want {
				t.Fatalf("visibility=%q want %q", got, tc.want)
			}
		})
	}
}
