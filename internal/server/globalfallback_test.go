// ═══ 更新日志 ═══
// 2026-09-23：新增「global 全部限流时回落同名 CN 模型」的 handler 级回归：
//
//	开关开启且 global 号全被限流时走 CN 账号；开关关闭或 global 仍有
//	可用号（或存在熔断等其它原因）时不得改道。
package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"workbuddy2api/internal/apikeys"
	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/pool"
)

// fallbackPool 造一个 global 全限流、CN 可用的池，返回池与请求用的密钥信息。
func fallbackPool(t *testing.T, fallback bool) (*pool.Pool, apikeys.Info) {
	t.Helper()
	auth.SetGlobalEnabled(true)
	t.Cleanup(func() { auth.SetGlobalEnabled(true) })
	p := testPoolWith(
		&auth.Auth{UID: "g1", Domain: "www.workbuddy.ai", AccessToken: "at-g1", ExpiresAt: 9999999999},
		&auth.Auth{UID: "g2", Domain: "www.workbuddy.ai", AccessToken: "at-g2", ExpiresAt: 9999999999},
		&auth.Auth{UID: "c1", Domain: "www.codebuddy.cn", AccessToken: "at-c1", ExpiresAt: 9999999999},
	)
	// 两个 global 号都进入账号级软冷却（无重置时间的 429 口径）→ 满足回落前提。
	for _, uid := range []string{"g1", "g2"} {
		p.CooldownSoftRate(uid, time.Minute, time.Time{}, "429 rate limit")
	}
	value := fallback
	return p, apikeys.Info{ID: "key_test", Name: "fallback", GlobalFallbackToCN: &value}
}

// fallbackRequest 构造带密钥上下文的 global: 请求。
func fallbackRequest(info apikeys.Info) *http.Request {
	req := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"global:deepseek-v4.1-flash","stream":false,"messages":[]}`))
	return req.WithContext(context.WithValue(req.Context(), apiKeyContextKey{}, info))
}

// TestGlobalFallbackRoutesToCNWhenAllGlobalRateLimited 开关开启且 global 全限流时，
// 请求必须落到 CN 账号（用 CN 的令牌打上游）。
func TestGlobalFallbackRoutesToCNWhenAllGlobalRateLimited(t *testing.T) {
	p, info := fallbackPool(t, true)
	var used []string
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		used = append(used, authz)
		return 200, sseOK, true
	})
	h := NewHandler(Config{Pool: p, Upstream: up})

	recorder := httptest.NewRecorder()
	h.ServeHTTP(recorder, fallbackRequest(info))

	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body)
	}
	if len(used) == 0 || used[0] != "Bearer at-c1" {
		t.Fatalf("request should fall back to the CN account, used=%v", used)
	}
}

// TestGlobalFallbackDisabledKeepsGlobalRealm 开关关闭时不得改道：global 全限流
// 就应当以 429/503 结束，而不是偷偷用 CN 号。
func TestGlobalFallbackDisabledKeepsGlobalRealm(t *testing.T) {
	p, info := fallbackPool(t, false)
	var used []string
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		used = append(used, authz)
		return 200, sseOK, true
	})
	h := NewHandler(Config{Pool: p, Upstream: up})

	recorder := httptest.NewRecorder()
	h.ServeHTTP(recorder, fallbackRequest(info))

	for _, authz := range used {
		if authz == "Bearer at-c1" {
			t.Fatalf("fallback must stay off when the key did not enable it: used=%v", used)
		}
	}
}

// TestGlobalFallbackSkippedWhenGlobalHealthy global 仍有可用号时不得改道。
func TestGlobalFallbackSkippedWhenGlobalHealthy(t *testing.T) {
	auth.SetGlobalEnabled(true)
	t.Cleanup(func() { auth.SetGlobalEnabled(true) })
	p := testPoolWith(
		&auth.Auth{UID: "g1", Domain: "www.workbuddy.ai", AccessToken: "at-g1", ExpiresAt: 9999999999},
		&auth.Auth{UID: "c1", Domain: "www.codebuddy.cn", AccessToken: "at-c1", ExpiresAt: 9999999999},
	)
	value := true
	info := apikeys.Info{ID: "key_test", Name: "fallback", GlobalFallbackToCN: &value}
	var used []string
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		used = append(used, authz)
		return 200, sseOK, true
	})
	h := NewHandler(Config{Pool: p, Upstream: up})

	recorder := httptest.NewRecorder()
	h.ServeHTTP(recorder, fallbackRequest(info))

	if len(used) == 0 || used[0] != "Bearer at-g1" {
		t.Fatalf("healthy global account must serve the request, used=%v", used)
	}
}

// TestGlobalFallbackSkippedOnBreaker global 号因熔断不可用时不得改道：
// 熔断与限流无关，静默改到 CN 会掩盖真实状态。
func TestGlobalFallbackSkippedOnBreaker(t *testing.T) {
	auth.SetGlobalEnabled(true)
	t.Cleanup(func() { auth.SetGlobalEnabled(true) })
	p := testPoolWith(
		&auth.Auth{UID: "g1", Domain: "www.workbuddy.ai", AccessToken: "at-g1", ExpiresAt: 9999999999},
		&auth.Auth{UID: "c1", Domain: "www.codebuddy.cn", AccessToken: "at-c1", ExpiresAt: 9999999999},
	)
	for i := 0; i < 3; i++ { // 达 breakerThreshold 触发熔断
		p.NoteError("g1")
	}
	value := true
	info := apikeys.Info{ID: "key_test", Name: "fallback", GlobalFallbackToCN: &value}
	var used []string
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		used = append(used, authz)
		return 200, sseOK, true
	})
	h := NewHandler(Config{Pool: p, Upstream: up})

	h.ServeHTTP(httptest.NewRecorder(), fallbackRequest(info))
	for _, authz := range used {
		if authz == "Bearer at-c1" {
			t.Fatalf("a tripped breaker must not trigger CN fallback: used=%v", used)
		}
	}
}
