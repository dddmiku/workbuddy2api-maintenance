// ═══ 更新日志 ═══
// 2026-10-13：新增。锁住「免费模型不得被回落成付费的同名模型」——回落只按可用性决定，
// 而同名模型两域倍率可以不同，静默改道会产生调用方没有要求的真实扣费。
package server

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"workbuddy2api/internal/apikeys"
	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/pool"
)

// paidModelPool 造一个 global 全限流、CN 可用的池，并把付费表注入成
// 「cn 侧该模型付费、global 侧免费」——正是线上实测的那一对。
func paidModelPool(t *testing.T, cnPaid, globalPaid bool) (*pool.Pool, apikeys.Info) {
	t.Helper()
	auth.SetGlobalEnabled(true)
	t.Cleanup(func() { auth.SetGlobalEnabled(true) })
	p := testPoolWith(
		&auth.Auth{UID: "g1", Domain: "www.workbuddy.ai", AccessToken: "at-g1", ExpiresAt: 9999999999},
		&auth.Auth{UID: "c1", Domain: "www.codebuddy.cn", AccessToken: "at-c1", ExpiresAt: 9999999999},
	)
	p.CooldownSoftRate("g1", time.Minute, time.Time{}, "429 rate limit")
	paid := map[string]bool{}
	if cnPaid {
		paid["cn\x00deepseek-v4.1-flash"] = true
	}
	if globalPaid {
		paid["global\x00deepseek-v4.1-flash"] = true
	}
	p.SetPaidModels(paid)
	value := true
	return p, apikeys.Info{ID: "key_test", Name: "fallback", GlobalFallbackToCN: &value}
}

// TestGlobalFallbackRefusesFreeToPaidSubstitution 免费模型不得被回落成付费的同名模型。
//
// 线上实测（2026-10-13）：密钥开了 global_fallback_to_cn，请求点名
// global:deepseek-v4.1-flash（目录倍率 x0.00，免费），global 域限流后被静默回落到
// cn:deepseek-v4.1-flash（x0.11，付费），单次真实扣费最高 10.03，而这笔钱按
// 「账本记调用方请求的名字」的约定记在了免费模型名下——面板上就出现「免费模型用了
// 260.89 积分」这种自相矛盾的数字。
//
// 判据：global 免费 + cn 付费 → 不回落（如实报 global 无可用号，由调用方决定）。
func TestGlobalFallbackRefusesFreeToPaidSubstitution(t *testing.T) {
	p, info := paidModelPool(t, true, false)
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
			t.Fatalf("免费模型被回落到付费的 CN 同名模型，产生计划外扣费；used=%v", used)
		}
	}
}

// TestGlobalFallbackAllowsPaidToFreeSubstitution 反向（global 付费 / cn 免费）不受影响：
// 那样回落只会更省，不违背调用方预期，也不该被这条判据挡住。
func TestGlobalFallbackAllowsPaidToFreeSubstitution(t *testing.T) {
	p, info := paidModelPool(t, false, true)
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
		t.Fatalf("付费→免费应当回落，used=%v", used)
	}
}

// TestGlobalFallbackAllowsEqualPriceSubstitution 两域都免费（或都付费）时照旧回落：
// 判据只拦「免费被换成付费」，不是把回落整体关掉。
func TestGlobalFallbackAllowsEqualPriceSubstitution(t *testing.T) {
	p, info := paidModelPool(t, false, false)
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
		t.Fatalf("同价应当回落，used=%v", used)
	}
}

// TestModelIsPaidReadsRealmScopedKeys 付费判据必须按 realm 分键。
//
// 同名模型两域倍率不同（cn:deepseek-v4.1-flash=x0.11 付费 / global 同名 x0.00 免费），
// 若判据只按裸名查表，CN 的付费判定会误伤 global 的免费模型——回落就会在
// 「global 免费」的请求上被错误放行，正是这次要修的 bug。
func TestModelIsPaidReadsRealmScopedKeys(t *testing.T) {
	p := testPoolWith(&auth.Auth{UID: "c1", Domain: "www.codebuddy.cn", AccessToken: "at-c1", ExpiresAt: 9999999999})
	p.SetPaidModels(map[string]bool{"cn\x00deepseek-v4.1-flash": true})
	if !p.ModelIsPaid("cn", "deepseek-v4.1-flash") {
		t.Error("cn 侧应判为付费")
	}
	if p.ModelIsPaid("global", "deepseek-v4.1-flash") {
		t.Error("global 侧不应因 CN 的登记被判为付费")
	}
	if p.ModelIsPaid("cn", "hy3") {
		t.Error("未登记的模型不应判为付费")
	}
}

// TestModelIsPaidWithoutTableIsFalse 未注入付费表时恒 false，回落行为保持不变。
func TestModelIsPaidWithoutTableIsFalse(t *testing.T) {
	p := testPoolWith(&auth.Auth{UID: "c1", Domain: "www.codebuddy.cn", AccessToken: "at-c1", ExpiresAt: 9999999999})
	if p.ModelIsPaid("cn", "glm-5.3") {
		t.Error("未注入付费表时不应判为付费")
	}
	var nilPool *pool.Pool
	if nilPool.ModelIsPaid("cn", "glm-5.3") {
		t.Error("nil 池不应 panic 或判为付费")
	}
}
