package pool

import (
	"testing"
	"time"

	"workbuddy2api/internal/auth"
)

// TestCountsDetailedForRealm 混合池（cn+global 账号，部分不可用/禁用）各域计数独立正确，
// 且全池总计数与分域之和一致。
//
// 2026-10-18 改：账号级限流冷却已取消（用户要求「不要账号冷却，只要模型冷却和积分不足」），
// 账号级 until 现在只表示积分不足（CoolHard）。本用例用 CoolHard 构造不可用账号，
// 并断言新增的 creditsExhausted 维度：total = healthy + cooling + creditsExhausted + disabled。
func TestCountsDetailedForRealm(t *testing.T) {
	withNoPickGap(t)
	auth.SetGlobalEnabled(true)
	t.Cleanup(func() { auth.SetGlobalEnabled(true) })

	p := New("")
	// 倍率表非空：empty 表会被 SetPaidModels 归成 nil，而 hardCreditBlocksModel
	// 对 nil 一律拦截（保守设计）。这里要的只是「积分不足的那一格被计数」。
	p.SetPaidModels(map[string]bool{"cn\x00paid-only": true})
	// cn 域：cn1 healthy、cn2 积分不足
	p.Add(&auth.Auth{UID: "cn1", Domain: "www.codebuddy.cn", AccessToken: "at"})
	p.Add(&auth.Auth{UID: "cn2", Domain: "www.codebuddy.cn", AccessToken: "at"})
	// global 域：g1 healthy、g2 disabled
	p.Add(&auth.Auth{UID: "g1", Domain: "www.workbuddy.ai", AccessToken: "at"})
	p.Add(&auth.Auth{UID: "g2", Domain: "www.workbuddy.ai", AccessToken: "at"})

	p.Cooldown("cn2", CoolHard, time.Hour, "余额不足")
	p.Disable("g2", "session dead")

	total, healthy, cooling, exhausted, disabled, inFlightFull := p.CountsDetailedForRealm("cn")
	if total != 2 || healthy != 1 || cooling != 0 || exhausted != 1 || disabled != 0 || inFlightFull != 0 {
		t.Errorf("CountsDetailedForRealm(cn) = (%d,%d,%d,%d,%d,%d) want (2,1,0,1,0,0)",
			total, healthy, cooling, exhausted, disabled, inFlightFull)
	}
	total, healthy, cooling, exhausted, disabled, _ = p.CountsDetailedForRealm("global")
	if total != 2 || healthy != 1 || cooling != 0 || exhausted != 0 || disabled != 1 {
		t.Errorf("CountsDetailedForRealm(global) = (%d,%d,%d,%d,%d) want (2,1,0,0,1)",
			total, healthy, cooling, exhausted, disabled)
	}
	// 全池汇总 = 分域之和。
	gt, gh, gc, ge, gd, _ := p.CountsDetailed()
	if gt != 4 || gh != 2 || gc != 0 || ge != 1 || gd != 1 {
		t.Errorf("CountsDetailed() = (%d,%d,%d,%d,%d) want (4,2,0,1,1)", gt, gh, gc, ge, gd)
	}
}

// TestCountsDetailedForRealmInFlightFull 在途占满维度同样按域分桶：
// cn 占满不计入 global 的 in_flight_full。
func TestCountsDetailedForRealmInFlightFull(t *testing.T) {
	withNoPickGap(t)
	auth.SetGlobalEnabled(true)
	t.Cleanup(func() { auth.SetGlobalEnabled(true) })

	p := New("")
	p.Add(&auth.Auth{UID: "cn1", Domain: "www.codebuddy.cn", AccessToken: "at"})
	p.Add(&auth.Auth{UID: "g1", Domain: "www.workbuddy.ai", AccessToken: "at"})
	p.SetMaxInFlight(1)
	if !p.Acquire("cn1") {
		t.Fatal("acquire cn1")
	}
	defer p.Release("cn1")

	_, _, _, _, _, cnF := p.CountsDetailedForRealm("cn")
	if cnF != 1 {
		t.Errorf("CountsDetailedForRealm(cn).in_flight_full = %d want 1 (healthy 且占满)", cnF)
	}
	_, _, _, _, _, gF := p.CountsDetailedForRealm("global")
	if gF != 0 {
		t.Errorf("CountsDetailedForRealm(global).in_flight_full = %d want 0", gF)
	}
	// 全池口径：healthy=2、in_flight_full=1（占满仍 healthy，状态机语义）。
	_, h, _, _, _, f := p.CountsDetailed()
	if h != 2 || f != 1 {
		t.Errorf("CountsDetailed() = healthy=%d in_flight_full=%d want 2/1", h, f)
	}
}

// TestCreditsExhaustedNotCountedAsCooling 余额不足不得被算进 cooling 桶。
//
// 2026-10-18 用户截图指出面板把「余额不足」显示成「冷却中」。根因在分类顺序：
// healthy() 用空模型名走 hardCreditBlocksModel("", …)，那里恒返回 true（「未指定
// 模型时保守拦截」），于是积分不足的号被判成 unhealthy、落进 cooling 桶。
// 分类必须先看 until+coolKind，再落到 healthy()。
func TestCreditsExhaustedNotCountedAsCooling(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.Cooldown("u1", CoolHard, time.Hour, "余额不足")

	total, healthy, cooling, exhausted, disabled, _ := p.CountsDetailed()
	if total != 1 || healthy != 0 || cooling != 0 || exhausted != 1 || disabled != 0 {
		t.Fatalf("counts = total=%d healthy=%d cooling=%d exhausted=%d disabled=%d want 1/0/0/1/0",
			total, healthy, cooling, exhausted, disabled)
	}
	if _, _, realmCooling, realmExhausted, _, _ := p.CountsDetailedForRealm("cn"); realmCooling != 0 || realmExhausted != 1 {
		t.Fatalf("realm counts: cooling=%d exhausted=%d want 0/1", realmCooling, realmExhausted)
	}
}
