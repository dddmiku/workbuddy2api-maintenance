// ═══ 更新日志 ═══
// 2026-09-28：锁定停用号在保活周期里的自动复活（refresh 成功即复活，失败保持停用）。
package scheduler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/upstream"
)

func TestKeepaliveRevivesDisabledAccountOnSuccessfulRefresh(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path != "/v2/plugin/auth/token/refresh" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"code": 0, "data": map[string]any{
			"accessToken": "at-new", "refreshToken": "rt-new", "expiresIn": 3600,
		}})
	}))
	defer srv.Close()

	p := pool.New("")
	p.Add(&auth.Auth{UID: "revivable", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	p.Add(&auth.Auth{UID: "healthy", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	p.Disable("revivable", "误判为账号封禁")
	s := New(Config{Pool: p, Upstream: &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}})

	s.RunKeepaliveNow()

	state, _ := p.Status("revivable")
	if state.Disabled {
		t.Fatalf("refresh 成功后停用号应自动复活: %+v", state)
	}
	if state.DisabledReason != "" {
		t.Fatalf("复活后不应残留停用原因: %q", state.DisabledReason)
	}
}

func TestKeepaliveKeepsDisabledAccountWhenRefreshFails(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"code":12153,"msg":"Offline user session not found"}`))
	}))
	defer srv.Close()

	p := pool.New("")
	p.Add(&auth.Auth{UID: "banned", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	p.Disable("banned", "账号封禁")
	s := New(Config{Pool: p, Upstream: &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}})

	s.RunKeepaliveNow()

	state, _ := p.Status("banned")
	if !state.Disabled {
		t.Fatalf("refresh 失败不应复活停用号: %+v", state)
	}
}
