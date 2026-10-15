// ═══ 更新日志 ═══
// 2026-09-23：新增「global 全部限流时回落同名 CN 模型」的 handler 级回归：
//
//	开关开启且 global 号全被限流时走 CN 账号；开关关闭或 global 仍有
//	可用号（或存在熔断等其它原因）时不得改道。
//
// 2026-10-15 改：判据从「全部因限流不可用」放宽为「没有可服务号」。旧口径让
//
//	余额不足的号把等式打破（13+7≠23），回落恒不触发——用户实测「所有 global 的
//	glm5.3 都冷却了却没有回落 CN」。「仍有可用号则不改道」这条守卫不变。
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

// TestGlobalFallbackOnBreakerWhenNoServableAccount 该域**一个能用的号都没有**时改道 CN。
//
// ⚠️ 2026-10-15 改语义：此前这里断言「熔断不得触发回落」（熔断与限流无关，静默改道会
// 掩盖真实状态）。但那条口径有致命缺陷——它把「不可用」窄化成「限流」，于是任何非限流
// 原因的不可用（熔断、余额不足）都会让回落**永不触发**。用户实测：
//
//	global 23 号 = 7 禁用 + 3 余额不足 + 13 模型级冷却 → 13+7=20 ≠ 23
//	「所有 global 的 glm5.3 都冷却了，却没有路由到 cn 号里的同名模型」
//
// 回落的目的是「这个域现在没号能用，把请求交给另一个域」——熔断的号同样不可用，
// 让请求失败的方式与限流没有区别。回落本身是**调用方按 key 显式开启**的
// （GlobalFallbackToCN），不是静默行为；「不该静默改道」由两道闸门守住：
// 只在 Available==0 时触发，且 CN 必须真有可用号。
func TestGlobalFallbackOnBreakerWhenNoServableAccount(t *testing.T) {
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
	// global 唯一的号熔断了 → 该域无可用号 → 应改道 CN。
	fellBack := false
	for _, authz := range used {
		if authz == "Bearer at-c1" {
			fellBack = true
		}
	}
	if !fellBack {
		t.Fatalf("global 无可用号时应回落 CN：used=%v", used)
	}
}

// TestGlobalFallbackBlockedWhenGlobalStillServable 反向守卫：global 还有能用的号时
// **绝不**改道。这是「不该静默改道」真正要守的东西，换成「不可用即算」的口径后必须
// 仍然成立——否则一个健康号会被别人的冷却连累着一起被绕过。
func TestGlobalFallbackBlockedWhenGlobalStillServable(t *testing.T) {
	auth.SetGlobalEnabled(true)
	t.Cleanup(func() { auth.SetGlobalEnabled(true) })
	p := testPoolWith(
		// g1 余额不足（非限流），g2 健康。
		&auth.Auth{UID: "g1", Domain: "www.workbuddy.ai", AccessToken: "at-g1", ExpiresAt: 9999999999},
		&auth.Auth{UID: "g2", Domain: "www.workbuddy.ai", AccessToken: "at-g2", ExpiresAt: 9999999999},
		&auth.Auth{UID: "c1", Domain: "www.codebuddy.cn", AccessToken: "at-c1", ExpiresAt: 9999999999},
	)
	p.Cooldown("g1", pool.CoolHard, time.Hour, "余额不足")
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
			t.Fatalf("global 还有可用号时不得改道 CN：used=%v", used)
		}
	}
}
