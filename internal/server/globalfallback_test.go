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
	"workbuddy2api/internal/session"
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
	// 两个 global 号的该模型都进入模型级软冷却（无重置时间的 429 口径）→ 满足回落前提。
	for _, uid := range []string{"g1", "g2"} {
		p.CooldownSoftRateForModel(uid, time.Minute, time.Time{}, "deepseek-v4.1-flash", "429 rate limit")
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

// TestGlobalFallbackOnCreditsExhaustedAndModelLimit 线上真实形态：global 域没有
// 一个号真的在被「账号级限流」——全部是积分不足 + 部分模型级限流。回落判据必须
// 在这种混合态下仍然成立。
//
// 这是 2026-10-18 重构后最容易漏的一条：账号级冷却已删除，若判据仍要求
// 「全部因限流不可用」，余额不足的号会把等式打破（历史上 VPS 实测 13+7≠23），
// CN 回落将永不触发。
func TestGlobalFallbackOnCreditsExhaustedAndModelLimit(t *testing.T) {
	auth.SetGlobalEnabled(true)
	t.Cleanup(func() { auth.SetGlobalEnabled(true) })
	p := testPoolWith(
		&auth.Auth{UID: "g1", Domain: "www.workbuddy.ai", AccessToken: "at-g1", ExpiresAt: 9999999999},
		&auth.Auth{UID: "g2", Domain: "www.workbuddy.ai", AccessToken: "at-g2", ExpiresAt: 9999999999},
		&auth.Auth{UID: "c1", Domain: "www.codebuddy.cn", AccessToken: "at-c1", ExpiresAt: 9999999999},
	)
	p.SetPaidModels(map[string]bool{"global\x00deepseek-v4.1-flash": true, "cn\x00deepseek-v4.1-flash": true})
	// g1：积分不足（付费模型被拦）；g2：该模型被限流。两种都不可用。
	p.Cooldown("g1", pool.CoolHard, time.Hour, "余额不足")
	p.CooldownSoftRateForModel("g2", time.Minute, time.Time{}, "deepseek-v4.1-flash", "429 rate limit")

	value := true
	info := apikeys.Info{ID: "key_test", Name: "fallback", GlobalFallbackToCN: &value}
	var used []string
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		used = append(used, authz)
		return 200, sseOK, true
	})
	h := NewHandler(Config{Pool: p, Upstream: up})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, fallbackRequest(info))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body)
	}
	if len(used) == 0 || used[0] != "Bearer at-c1" {
		t.Fatalf("积分不足 + 模型限流的混合态必须回落 CN，used=%v", used)
	}
}

// TestGlobalFallbackStopsOnceGlobalRecovers 回落不是粘性的：global 域一恢复可用，
// 下一个请求必须立刻切回 global，而不是继续留在 CN。
//
// 用户 2026-10-18 的质疑：「global 如果有解除限流的号，还会继续路由吗，继续路由的话
// 不是太不合理了吗」。判据必须是**每次请求实时重算**——只要 global.Available > 0
// 就不回落；而模型级限流到期后 healthyForModel 自然为真，配额也回到 global 的号上。
func TestGlobalFallbackStopsOnceGlobalRecovers(t *testing.T) {
	p, info := fallbackPool(t, true)
	var used []string
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		used = append(used, authz)
		return 200, sseOK, true
	})
	h := NewHandler(Config{Pool: p, Upstream: up})

	// 第一次：global 两个号都被限流 → 回落 CN。
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, fallbackRequest(info))
	if rec.Code != http.StatusOK {
		t.Fatalf("first status=%d body=%s", rec.Code, rec.Body)
	}
	if len(used) != 1 || used[0] != "Bearer at-c1" {
		t.Fatalf("global 全限流时应回落 CN，used=%v", used)
	}

	// global 的限流解除（模拟上游重置，或面板「检测」探测成功后 ClearModelLimit）
	// → 下一个请求必须回 global。
	for _, uid := range []string{"g1", "g2"} {
		p.ClearModelLimit(uid, "deepseek-v4.1-flash")
	}

	used = nil
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, fallbackRequest(info))
	if rec2.Code != http.StatusOK {
		t.Fatalf("second status=%d body=%s", rec2.Code, rec2.Body)
	}
	if len(used) == 0 || used[0] == "Bearer at-c1" {
		t.Fatalf("global 恢复后必须切回 global，实际仍用 CN：used=%v", used)
	}
}

// TestGlobalFallbackNotStuckBySessionBinding 回落成功后会把会话粘到**实际服务的
// CN 账号**。这不能变成「一旦回落就永远回落」：下一次请求先按完整模型名
// （带 global: 前缀）解析粘性号，realm 过滤会把它限定在 global 集合；global 恢复
// 后解析不到 CN 号，于是重新走 normal 选号并选回 global。
func TestGlobalFallbackNotStuckBySessionBinding(t *testing.T) {
	auth.SetGlobalEnabled(true)
	t.Cleanup(func() { auth.SetGlobalEnabled(true) })
	p := testPoolWith(
		&auth.Auth{UID: "g1", Domain: "www.workbuddy.ai", AccessToken: "at-g1", ExpiresAt: 9999999999},
		&auth.Auth{UID: "c1", Domain: "www.codebuddy.cn", AccessToken: "at-c1", ExpiresAt: 9999999999},
	)
	p.CooldownSoftRateForModel("g1", time.Minute, time.Time{}, "deepseek-v4.1-flash", "429 rate limit")

	// 可用集合按 realm 过滤——与 cmd/server/wiring.go 的 realmAwareAvailableForModel 同口径。
	available := func(model string) []string {
		realm, bare := resolveModel(model)
		return p.AvailableUIDsForModelRealm(bare, realm)
	}
	sess := session.New(session.Config{
		TTL:       time.Minute,
		Store:     newBindStore(),
		Available: func() []string { return []string{"g1", "c1"} },
		AvailableForModel: available,
	})
	var used []string
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		used = append(used, authz)
		return 200, sseOK, true
	})
	value := true
	info := apikeys.Info{ID: "key_test", Name: "fallback", GlobalFallbackToCN: &value}
	h := NewHandler(Config{Pool: p, Upstream: up, Session: sess})

	body := func() string {
		return `{"model":"global:deepseek-v4.1-flash","stream":false,"conversationId":"conv-1","messages":[{"role":"user","content":"hi"}]}`
	}
	req := func() *http.Request {
		r := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body()))
		return r.WithContext(context.WithValue(r.Context(), apiKeyContextKey{}, info))
	}

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req())
	if rec.Code != http.StatusOK || len(used) != 1 || used[0] != "Bearer at-c1" {
		t.Fatalf("首次应回落 CN：code=%d used=%v", rec.Code, used)
	}

	// global 恢复（限流解除）。
	p.ClearModelLimit("g1", "deepseek-v4.1-flash")

	used = nil
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, req())
	if rec2.Code != http.StatusOK {
		t.Fatalf("second code=%d body=%s", rec2.Code, rec2.Body)
	}
	if len(used) == 0 || used[0] == "Bearer at-c1" {
		t.Fatalf("会话粘性不得把请求钉在 CN：used=%v", used)
	}
}
