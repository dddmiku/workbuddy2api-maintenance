package pool

// 2026-11-01：粘性命中绕过「快过期积分优先」的回归用例。
//
// 现场（用户报障，global 域）：6 个账号里 xire2tqc(3309d8e3) 有 250 快过期、
// 余额 350，却没被优先使用，流量一直落在别的号上。日志显示 8913 次决策里
// 8913 次是 sticky_hit——粘性命中发生在 pick() 之前，而快过期硬过滤写在
// pick() 里，于是该特性在有会话绑定的场景下**完全不生效**。

import (
	"testing"

	"workbuddy2api/internal/auth"
)

// stickyPool 复刻现场：sticky 号无快过期积分，另一个号有。
func stickyPool() *Pool {
	p := New("")
	p.Add(&auth.Auth{UID: "sticky-no-expiring"})
	p.Add(&auth.Auth{UID: "other-with-expiring"})
	p.SetCreditsDetailed("sticky-no-expiring", 350, 0)     // 无快过期
	p.SetCreditsDetailed("other-with-expiring", 350, 250)  // 250 快过期
	p.SetPaidModels(map[string]bool{"paid-model": true})
	return p
}

// TestStickyMustNotBypassExpiringFilter 付费模型上，粘性号没有快过期积分而
// 别的号有时，粘性命中必须被拒——否则快过期积分永远消耗不掉。
func TestStickyMustNotBypassExpiringFilter(t *testing.T) {
	p := stickyPool()
	got, d := p.PickByUIDForModelRealmWithDecision("sticky-no-expiring", "paid-model", "")
	if got != nil {
		t.Fatalf("粘性命中 %s，但它没有快过期积分；应被拒并回落到有快过期积分的号（decision=%+v）",
			got.UID, d.ReasonCode)
	}
	if d.ReasonCode != "expiring_deferred" {
		t.Errorf("拒绝原因 = %q want expiring_deferred", d.ReasonCode)
	}
	// 被拒后走普通选号，应落到有快过期积分的号。
	fallback := p.PickExcludingForRealm(nil, "paid-model", "")
	if fallback == nil || fallback.UID != "other-with-expiring" {
		t.Fatalf("回落选号 = %v want other-with-expiring", fallback)
	}
}

// TestStickyAllowedWhenNoExpiringAnywhere 全池都没有快过期积分时，
// 粘性照常生效（过滤无差别，不能因此打断会话）。
func TestStickyAllowedWhenNoExpiringAnywhere(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "a"})
	p.Add(&auth.Auth{UID: "b"})
	p.SetCreditsDetailed("a", 350, 0)
	p.SetCreditsDetailed("b", 350, 0)
	p.SetPaidModels(map[string]bool{"paid-model": true})

	got, _ := p.PickByUIDForModelRealmWithDecision("a", "paid-model", "")
	if got == nil || got.UID != "a" {
		t.Fatalf("全池无快过期时应保持粘性，got=%v", got)
	}
}

// TestStickyAllowedForFreeModel 免费模型不做该过滤，粘性照常生效
// （与 pick() 里「只在付费模型上做」的口径一致）。
func TestStickyAllowedForFreeModel(t *testing.T) {
	p := stickyPool()
	got, _ := p.PickByUIDForModelRealmWithDecision("sticky-no-expiring", "free-model", "")
	if got == nil || got.UID != "sticky-no-expiring" {
		t.Fatalf("免费模型不该被快过期过滤打断粘性，got=%v", got)
	}
}

// TestStickyAllowedWhenItHasExpiring 粘性号自己就有快过期积分 → 照常命中。
func TestStickyAllowedWhenItHasExpiring(t *testing.T) {
	p := stickyPool()
	got, _ := p.PickByUIDForModelRealmWithDecision("other-with-expiring", "paid-model", "")
	if got == nil || got.UID != "other-with-expiring" {
		t.Fatalf("粘性号有快过期积分时应命中，got=%v", got)
	}
}
