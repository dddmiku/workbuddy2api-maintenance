// ═══ 更新日志 ═══
// 2026-09-23：新增来源级限流的 handler 级回归：上游对多个不同账号连续回无重置时间的
//
//	429 时，网关必须提前收手并如实回 429，而不是轮满 MaxRotate 才失败；
//	新请求仍要正常发出第一次尝试；带重置时间的 6004 不参与闸门。
package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
)

// sourceRateBody 无重置时间的 14003 原文（实测形态）。
const sourceRateBody = `{"code":14003,"msg":"too many requests","requestId":"abc",` +
	`"displayMsg":{"en":"Too many requests. Please retry later.","zh":"请求过于频繁，请稍后重试。"}}`

// modelRateBody 带重置时间的 6004（模型级限额，不是来源级）。
func modelRateBody() string {
	reset := time.Now().Add(2 * time.Hour).Format("2006-01-02 15:04:05")
	return `{"code":6004,"msg":"usage exceeds frequency limit, but don't worry, your usage resets at ` +
		reset + ` UTC+8, alternatively, you can switch to the other models to continue using it."}`
}

// sourceRatePool 造一个全是 global 账号的池，让轮转必然撞满。
func sourceRatePool(t *testing.T) (*Handler, map[string]int) {
	t.Helper()
	auth.SetGlobalEnabled(true)
	t.Cleanup(func() { auth.SetGlobalEnabled(true) })
	calls := map[string]int{}
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		calls[authz]++
		return http.StatusTooManyRequests, sourceRateBody, false
	})
	p := testPoolWith(
		&auth.Auth{UID: "g1", Domain: "www.workbuddy.ai", AccessToken: "at-1", ExpiresAt: 9999999999},
		&auth.Auth{UID: "g2", Domain: "www.workbuddy.ai", AccessToken: "at-2", ExpiresAt: 9999999999},
		&auth.Auth{UID: "g3", Domain: "www.workbuddy.ai", AccessToken: "at-3", ExpiresAt: 9999999999},
		&auth.Auth{UID: "g4", Domain: "www.workbuddy.ai", AccessToken: "at-4", ExpiresAt: 9999999999},
	)
	h := NewHandler(Config{Pool: p, Upstream: up, SoftCooldown: 30 * time.Second})
	h.cfg.MaxRotate = 5
	return h, calls
}

func sourceRateRequest() *http.Request {
	return httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"global:deepseek-v4.1-flash","stream":true,"messages":[]}`))
}

// TestSourceRateLimitStopsRotationEarly 上游连续对多个账号回 14003 时，网关应在
// 闸门触发后停止换号，而不是轮满 MaxRotate。
func TestSourceRateLimitStopsRotationEarly(t *testing.T) {
	h, calls := sourceRatePool(t)

	recorder := httptest.NewRecorder()
	h.ServeHTTP(recorder, sourceRateRequest())

	if recorder.Code != http.StatusTooManyRequests {
		t.Fatalf("status=%d want 429", recorder.Code)
	}
	if !strings.Contains(recorder.Body.String(), "rate_limit_exceeded") {
		t.Fatalf("body should carry the rate limit code: %s", recorder.Body.String())
	}
	// 闸门在第 3 个不同账号命中后开启，因此上游尝试次数必须少于 MaxRotate=5。
	total := 0
	for _, count := range calls {
		total += count
	}
	if total >= 5 {
		t.Fatalf("upstream calls=%d; the gate should stop rotation before MaxRotate=5", total)
	}
}

// TestSourceRateLimitDoesNotBlockFreshRequests 闸门只作用于同一请求内的轮转：
// 新请求仍要发出第一次尝试（半开探测），不能被闸门直接拒绝。
func TestSourceRateLimitDoesNotBlockFreshRequests(t *testing.T) {
	h, calls := sourceRatePool(t)

	h.ServeHTTP(httptest.NewRecorder(), sourceRateRequest())
	afterFirst := 0
	for _, count := range calls {
		afterFirst += count
	}

	h.ServeHTTP(httptest.NewRecorder(), sourceRateRequest())
	afterSecond := 0
	for _, count := range calls {
		afterSecond += count
	}
	if afterSecond <= afterFirst {
		t.Fatalf("fresh request must still probe upstream: calls=%d -> %d", afterFirst, afterSecond)
	}
}

// TestModelRateLimitWithResetSkipsSourceGate 带重置时间的 6004 是模型级限额，
// 不该被当成来源级限流：它仍按既有逻辑轮换账号。
func TestModelRateLimitWithResetSkipsSourceGate(t *testing.T) {
	auth.SetGlobalEnabled(true)
	t.Cleanup(func() { auth.SetGlobalEnabled(true) })
	calls := map[string]int{}
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		calls[authz]++
		return http.StatusTooManyRequests, modelRateBody(), false
	})
	p := testPoolWith(
		&auth.Auth{UID: "g1", Domain: "www.workbuddy.ai", AccessToken: "at-1", ExpiresAt: 9999999999},
		&auth.Auth{UID: "g2", Domain: "www.workbuddy.ai", AccessToken: "at-2", ExpiresAt: 9999999999},
		&auth.Auth{UID: "g3", Domain: "www.workbuddy.ai", AccessToken: "at-3", ExpiresAt: 9999999999},
	)
	h := NewHandler(Config{Pool: p, Upstream: up, SoftCooldown: 30 * time.Second})
	h.cfg.MaxRotate = 3

	recorder := httptest.NewRecorder()
	h.ServeHTTP(recorder, sourceRateRequest())
	if recorder.Code != http.StatusTooManyRequests {
		t.Fatalf("status=%d want 429", recorder.Code)
	}
	total := 0
	for _, count := range calls {
		total += count
	}
	if total < 2 {
		t.Fatalf("6004 must keep rotating accounts instead of opening the source gate: calls=%d", total)
	}
}
