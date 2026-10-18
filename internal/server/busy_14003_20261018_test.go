// 14003 Model busy 不计入来源闸门（2026-10-18）。
//
// 背景：pool/sourcerate.go 的闸门按「60 秒内 3 个不同账号撞无重置时刻的 429」判定
// 来源级限流，触发后暂停该 realm 选号 30 秒。14003 的 Model busy 形态也符合
// 「无重置时刻的 429」，但它表示**该模型后端此刻拥塞**（displayMsg 明写
// "Please switch models"），与出口来源无关——计入闸门会让一个模型忙把整个域的
// 选号暂停，连带误伤其他模型。
//
// 本文件锁定：Model busy 不触发闸门，真来源限流照常触发。
package server

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
)

// busyBody 生产日志原文（14003 Model busy 形态）。
const busy14003Body = `{"code":14003,"msg":"too many requests","requestId":"abc",` +
	`"displayMsg":{"en":"Model busy. Please switch models or retry later","zh":"模型繁忙"}}`

// TestModelBusyDoesNotOpenSourceGate 连续多个账号撞 Model busy 时，闸门不得开启。
func TestModelBusyDoesNotOpenSourceGate(t *testing.T) {
	auth.SetGlobalEnabled(true)
	t.Cleanup(func() { auth.SetGlobalEnabled(true) })
	calls := map[string]int{}
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		calls[authz]++
		return http.StatusTooManyRequests, busy14003Body, false
	})
	p := testPoolWith(
		&auth.Auth{UID: "g1", Domain: "www.workbuddy.ai", AccessToken: "at-1", ExpiresAt: 9999999999},
		&auth.Auth{UID: "g2", Domain: "www.workbuddy.ai", AccessToken: "at-2", ExpiresAt: 9999999999},
		&auth.Auth{UID: "g3", Domain: "www.workbuddy.ai", AccessToken: "at-3", ExpiresAt: 9999999999},
		&auth.Auth{UID: "g4", Domain: "www.workbuddy.ai", AccessToken: "at-4", ExpiresAt: 9999999999},
	)
	h := NewHandler(Config{Pool: p, Upstream: up, SoftCooldown: 30 * time.Second})
	h.cfg.MaxRotate = 5

	h.ServeHTTP(httptest.NewRecorder(), sourceRateRequest())

	total := 0
	for _, count := range calls {
		total += count
	}
	// 闸门未开 → 轮转把所有号都试一遍（池里 4 个号，MaxRotate=5 也用不出第 5 次）。
	// 若闸门误开，会在第 3 个不同账号命中后就收手，total 只有 3。
	if total != 4 {
		t.Fatalf("upstream calls=%d; Model busy 不得开来源闸门，应把 4 个号各试一次", total)
	}
	if limited, _ := p.SourceRateGate("global"); limited {
		t.Fatal("Model busy 不得开启来源级闸门")
	}
	// 也不得写任何账号/模型冷却（用户口径：没有精确恢复时刻的不算真限流）。
	for _, uid := range []string{"g1", "g2", "g3", "g4"} {
		st, _ := p.Status(uid)
		if st.Cooling {
			t.Errorf("%s 不得被账号级冷却: %+v", uid, st)
		}
		if len(st.RateLimitedModels) != 0 {
			t.Errorf("%s 无恢复时刻不得写模型台账: %+v", uid, st.RateLimitedModels)
		}
	}
}

// TestRealSourceRateStillOpensGate 真来源限流（同 code、不同 displayMsg）仍按原逻辑
// 触发闸门——本次修复不能把真限流的保护一起关掉。
func TestRealSourceRateStillOpensGate(t *testing.T) {
	h, calls := sourceRatePool(t)
	h.ServeHTTP(httptest.NewRecorder(), sourceRateRequest())

	total := 0
	for _, count := range calls {
		total += count
	}
	if total >= 5 {
		t.Fatalf("upstream calls=%d; 真来源限流应在 MaxRotate=5 前开闸门收手", total)
	}
	if limited, _ := h.cfg.Pool.SourceRateGate("global"); !limited {
		t.Fatal("真来源限流应开启闸门")
	}
}
