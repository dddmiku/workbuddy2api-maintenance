// ═══ 更新日志 ═══
// 2026-10-13：新增。锁住「免费模型不得被回落成付费的同名模型」——回落只按可用性决定，
// 而同名模型两域倍率可以不同，静默改道会产生调用方没有要求的真实扣费。
//
// 2026-10-15 改判据：从「拒绝回落」改为「回落但账本记实际服务的域」。
//
//	旧口径拦得太死，让整个模型在 global 全冷却时彻底不可用——用户实测：
//	「所有 global 4.1 冷却了，却无法回落到 cn 4.1」。账本归错名是**记账问题**，
//	不该拿「拒绝服务」来治。见 handler.go 的同名回落注释。
package server

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"workbuddy2api/internal/apikeys"
	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/usage"
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
	p.CooldownSoftRateForModel("g1", time.Minute, time.Time{}, "deepseek-v4.1-flash", "429 rate limit")
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

// TestGlobalFallbackSubstitutesFreeToPaid 免费模型在 global 全冷却时**照常**回落到
// 付费的同名 CN 模型——调用方开了回落开关，要的就是「这个模型还能用」。
//
// ⚠️ 2026-10-15 改语义（原 TestGlobalFallbackRefusesFreeToPaidSubstitution）。
// 旧口径在 global 免费 / cn 付费时**拒绝回落**，理由是会产生计划外扣费、且那笔钱会
// 被记到免费模型名下。但拒绝的代价是整个模型在 global 全冷却时不可用（用户实测：
// 「所有 global 4.1 冷却了，却无法回落到 cn 4.1」）。
//
// 现在回落发生，账本改记**实际服务的域**（cn:deepseek-v4.1-flash）——那笔积分确实
// 花在 cn 的模型上，记到免费的 global 名下才是矛盾的来源。用量归属由 handler 的
// fallbackModel 负责（见同名回落分支）。
func TestGlobalFallbackSubstitutesFreeToPaid(t *testing.T) {
	p, info := paidModelPool(t, true, false)
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
		t.Fatalf("global 无可用号时免费模型应回落 CN：used=%v", used)
	}
}

// newUsageLedger 开一本临时账本，返回 Store 与「已记账的模型名」读取器。
func newUsageLedger(t *testing.T) (*usage.Store, func() []string) {
	t.Helper()
	ledger, err := usage.Open(filepath.Join(t.TempDir(), "usage.json"), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ledger.Close() })
	models := func() []string {
		var out []string
		for _, key := range ledger.Snapshot().Keys {
			for _, m := range key.Models {
				out = append(out, m.Model)
			}
		}
		sort.Strings(out)
		return out
	}
	return ledger, models
}

// TestFallbackUsageAttributedToServedRealm 回落产生的用量必须记在**实际服务的域**名下。
//
// 这是替代旧「拒绝回落」判据的那道闸门：面板上不能再出现「免费模型用了 N 积分」
// 这种自相矛盾的数字——那笔钱要记到 cn 的名下。
func TestFallbackUsageAttributedToServedRealm(t *testing.T) {
	p, info := paidModelPool(t, true, false)
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 200, sseOK, true
	})
	ledger, recordedModels := newUsageLedger(t)
	h := NewHandler(Config{Pool: p, Upstream: up, Usage: ledger})

	h.ServeHTTP(httptest.NewRecorder(), fallbackRequest(info))

	models := recordedModels()
	if len(models) != 1 {
		t.Fatalf("应记录一次用量，实际 %v", models)
	}
	if models[0] != "cn:deepseek-v4.1-flash" {
		t.Fatalf("回落后的用量应记在实际服务的域名下，实际 %q（免费→付费回落若记 global 会造出免费模型花了积分的假账）", models[0])
	}
}

// TestFallbackUsageKeepsRequestedModelWhenPriceEqual 同价回落不改写账本模型名。
//
// 只在「真的换了域且两域价不同」时改写：老部署里两域同价的模型占绝大多数，
// 无条件改写会让它们的账本口径凭空漂移。
func TestFallbackUsageKeepsRequestedModelWhenPriceEqual(t *testing.T) {
	p, info := paidModelPool(t, false, false)
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 200, sseOK, true
	})
	ledger, recordedModels := newUsageLedger(t)
	h := NewHandler(Config{Pool: p, Upstream: up, Usage: ledger})

	h.ServeHTTP(httptest.NewRecorder(), fallbackRequest(info))

	models := recordedModels()
	if len(models) != 1 {
		t.Fatalf("应记录一次用量，实际 %v", models)
	}
	if models[0] != "global:deepseek-v4.1-flash" {
		t.Fatalf("同价回落的账本应保持调用方请求的名字，实际 %q", models[0])
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
