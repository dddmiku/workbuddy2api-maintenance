// ═══ 更新日志 ═══
// 2026-10-11：余额不足的账号在免费模型上必须仍可选（用户实测反馈），
// 并锁定「付费模型照旧拦截 / 无倍率表照旧拦截」两条安全边界。
package pool

import (
	"testing"
	"time"

	"workbuddy2api/internal/auth"
)

// TestBalanceExhaustedStillUsableOnFreeModel 余额不足只拦扣积分的模型。
//
// 2026-10-11 用户反馈：面板上「余额不足」的号整体显示冷却中，但**免费模型本来
// 就能用**——余额耗尽的账号只是调不了付费模型。旧行为把整个号摘出候选池，
// 于是这些号在免费模型上也完全闲置。
func TestBalanceExhaustedStillUsableOnFreeModel(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "poor"})
	p.SetPaidModels(map[string]bool{"paid-model": true})
	// 余额耗尽 → 硬冷却到次日。
	p.CooldownUntilTomorrow4AM("poor", "余额不足")

	if got := p.PickExcludingForRealm(nil, "free-model", "cn"); got == nil {
		t.Fatal("余额不足的号在免费模型上必须仍可选（用户实测反馈）")
	} else if got.UID != "poor" {
		t.Fatalf("选中 %s，期望 poor", got.UID)
	}
	// 付费模型照旧拦截：调了必 402，白烧一轮上游。
	if got := p.PickExcludingForRealm(nil, "paid-model", "cn"); got != nil {
		t.Fatalf("余额不足的号不该在付费模型上被选中，实际 %s", got.UID)
	}
}

// TestBalanceExhaustedBlockedWhenNoRateTable 倍率表未注入时退回旧行为。
//
// 拿不到倍率就无从判断「这个模型免不免费」；此时宁可少放行，也不能把余额不足的号
// 放到扣积分的模型上白撞 402。
func TestBalanceExhaustedBlockedWhenNoRateTable(t *testing.T) {
	p := New("") // 不调用 SetPaidModels
	p.Add(&auth.Auth{UID: "poor"})
	p.CooldownUntilTomorrow4AM("poor", "余额不足")
	if got := p.PickExcludingForRealm(nil, "free-model", "cn"); got != nil {
		t.Fatalf("无倍率表时应退回旧行为（拦截），实际选中 %s", got.UID)
	}
}

// TestBalanceExhaustedBlockedWithoutModel 未指定模型的调用路径保持旧语义。
func TestBalanceExhaustedBlockedWithoutModel(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "poor"})
	p.SetPaidModels(map[string]bool{"paid-model": true})
	p.CooldownUntilTomorrow4AM("poor", "余额不足")
	if got := p.PickExcludingForRealm(nil, "", "cn"); got != nil {
		t.Fatalf("model 为空时应拦截（旧语义），实际选中 %s", got.UID)
	}
}

// TestFreeModelExemptionDoesNotLeakOtherLimits 放行的只是「余额冷却」这一条。
//
// 熔断、模型级 6004、禁用都必须照旧拦截——否则免费模型会变成绕过一切保护的旁路。
func TestFreeModelExemptionDoesNotLeakOtherLimits(t *testing.T) {
	// 熔断：直接断言「健康口径」而不是选号结果——选号在全冷却时有兜底路径
	// （pickEarliestExpiryWithDecisionLocked 允许熔断号参与，那是既有设计：
	// 熔断可能已恢复，失败成本仅一轮换）。这里要锁的是「免费模型豁免不会把
	// 熔断从健康判定里抹掉」。
	p := New("")
	p.Add(&auth.Auth{UID: "u"})
	p.SetPaidModels(map[string]bool{"paid-model": true})
	p.SetBreaker(1, time.Hour, time.Hour)
	p.NoteError("u")
	p.mu.RLock()
	broken := p.byUID["u"].breakerUntil.After(time.Now())
	p.mu.RUnlock()
	if !broken {
		t.Fatal("前置条件：NoteError 应已触发熔断")
	}
	if got := p.byUID["u"].unavailableReason(time.Now(), "free-model"); got != "circuit_breaker" {
		t.Fatalf("熔断不该被免费模型豁免，unavailableReason=%q", got)
	}

	// 禁用：永远不参与。
	p2 := New("")
	p2.Add(&auth.Auth{UID: "d"})
	p2.SetPaidModels(map[string]bool{"paid-model": true})
	p2.Disable("d", "manual")
	if got := p2.PickExcludingForRealm(nil, "free-model", "cn"); got != nil {
		t.Fatalf("禁用的号不该因免费模型被放行，实际 %s", got.UID)
	}

	// 模型级 6004：只在被限的那个模型上拦，免费模型不该被它波及——
	// 但它本身对免费模型也不该构成拦截（模型不同）。
	p3 := New("")
	p3.Add(&auth.Auth{UID: "m"})
	p3.SetPaidModels(map[string]bool{"paid-model": true})
	// resetAt 非零才写模型级台账（零值走账号级软冷却分支）。
	p3.CooldownSoftForModel("m", time.Hour, time.Now().Add(time.Hour), "paid-model", "6004")
	now := time.Now()
	if got := p3.byUID["m"].unavailableReason(now, "paid-model"); got != "model_cooldown" {
		t.Fatalf("被 6004 限流的模型应判 model_cooldown，实际 %q", got)
	}
	if got := p3.byUID["m"].unavailableReason(now, "free-model"); got != "" {
		t.Fatalf("6004 是模型级限流，不该波及另一个模型，实际 %q", got)
	}
}
