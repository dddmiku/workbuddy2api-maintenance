package pool

import (
	"testing"
	"time"

	"workbuddy2api/internal/auth"
)

// TestCountsDetailedForRealm 混合池（cn+global 账号，部分余额不足/禁用）各域计数独立正确，
// 且全池总计数与分域之和一致。
//
// 2026-10-18 口径（用户：「可不可用是针对账号的，冷却和积分余额是模型使用方面的」）：
// 分桶只按**账号级**事实 —— total = healthy + cooling + disabled。
// 余额不足（CoolHard）的账号仍然计入 healthy，它只作为 creditsExhausted 子集另记。
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

	// cn：cn1 健康、cn2 余额不足 → 两个都算 healthy（账号都可用），余额不足记 1。
	total, healthy, cooling, exhausted, disabled, inFlightFull := p.CountsDetailedForRealm("cn")
	if total != 2 || healthy != 2 || cooling != 0 || exhausted != 1 || disabled != 0 || inFlightFull != 0 {
		t.Errorf("CountsDetailedForRealm(cn) = (%d,%d,%d,%d,%d,%d) want (2,2,0,1,0,0)",
			total, healthy, cooling, exhausted, disabled, inFlightFull)
	}
	// global：g1 健康、g2 停用 → 1 健康 1 停用。
	total, healthy, cooling, exhausted, disabled, _ = p.CountsDetailedForRealm("global")
	if total != 2 || healthy != 1 || cooling != 0 || exhausted != 0 || disabled != 1 {
		t.Errorf("CountsDetailedForRealm(global) = (%d,%d,%d,%d,%d) want (2,1,0,0,1)",
			total, healthy, cooling, exhausted, disabled)
	}
	// 全池汇总 = 分域之和；等式 total = healthy + cooling + disabled。
	gt, gh, gc, ge, gd, _ := p.CountsDetailed()
	if gt != 4 || gh != 3 || gc != 0 || ge != 1 || gd != 1 {
		t.Errorf("CountsDetailed() = (%d,%d,%d,%d,%d) want (4,3,0,1,1)", gt, gh, gc, ge, gd)
	}
	if gt != gh+gc+gd {
		t.Errorf("账号级分桶必须自洽：total=%d != healthy(%d)+cooling(%d)+disabled(%d)", gt, gh, gc, gd)
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

// TestCreditsExhaustedCountsAsHealthy 余额不足的账号仍然「可用」。
//
// 2026-10-18 用户口径：「可不可用是针对账号的，冷却和积分余额是模型使用方面的」。
// 余额不足只说明这个号此刻调不动扣积分的模型，账号本身没有被上游限制、也没有熔断，
// 因此必须计入 healthy，只作为 creditsExhausted 子集另记。
//
// 此前它被算成不可用（先落进 cooling 桶、后落进「不可用」），面板于是显示
// 「11 个不可用」——用户截图指出这是错的。
func TestCreditsExhaustedCountsAsHealthy(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.Cooldown("u1", CoolHard, time.Hour, "余额不足")

	total, healthy, cooling, exhausted, disabled, _ := p.CountsDetailed()
	if total != 1 || healthy != 1 || cooling != 0 || exhausted != 1 || disabled != 0 {
		t.Fatalf("counts = total=%d healthy=%d cooling=%d exhausted=%d disabled=%d want 1/1/0/1/0",
			total, healthy, cooling, exhausted, disabled)
	}
	_, realmHealthy, realmCooling, realmExhausted, _, _ := p.CountsDetailedForRealm("cn")
	if realmHealthy != 1 || realmCooling != 0 || realmExhausted != 1 {
		t.Fatalf("realm counts: healthy=%d cooling=%d exhausted=%d want 1/0/1", realmHealthy, realmCooling, realmExhausted)
	}
	// 账号级可用 ≠ 探活可用：倍率表未注入时无从判断是否存在免费模型，
	// 该号（唯一的号）不能服务任何请求，/healthz 必须仍是 503。
	if p.ServableNow() {
		t.Error("余额不足且无免费模型可判定时，探活不应报可服务")
	}
}

// TestServableWhenFreeModelAvailable 同上，但倍率表注入了免费模型：
// 余额不足的号仍能调免费模型，探活应报可服务（账号可用 + 有模型可调）。
func TestServableWhenFreeModelAvailable(t *testing.T) {
	p := New("")
	// 显式登记一个免费模型（false）与一个付费模型（true）。
	p.SetPaidModels(map[string]bool{"cn\x00free-model": false, "cn\x00paid-model": true})
	p.Add(&auth.Auth{UID: "u1"})
	p.Cooldown("u1", CoolHard, time.Hour, "余额不足")

	if !p.ServableNow() {
		t.Fatal("存在免费模型时，余额不足的号仍可服务，探活应为 true")
	}
}
