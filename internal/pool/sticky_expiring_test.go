package pool

// 2026-11-01：快过期积分优先的作用范围用例。
//
// 用户决策：快过期优先**只在新建会话时**生效，已绑定会话保持原号。
// 理由：同一对话在多个号之间跳会破坏会话连续性（上游上下文迁移、缓存失效、
// 风控关联），省下的积分不值得。
//
// 因此两件事都要被钉住：
//   1. 粘性命中不做快过期让路（即使别的号有快过期积分）；
//   2. 新建会话（无粘性）时，付费模型仍只从有快过期积分的号里选。

import (
	"testing"

	"workbuddy2api/internal/auth"
)

// stickyExpiringPool 复刻现场：sticky 号无快过期积分，另一个号有 250。
func stickyExpiringPool() *Pool {
	p := New("")
	p.Add(&auth.Auth{UID: "sticky-no-expiring"})
	p.Add(&auth.Auth{UID: "other-with-expiring"})
	p.SetCreditsDetailed("sticky-no-expiring", 350, 0)    // 无快过期
	p.SetCreditsDetailed("other-with-expiring", 350, 250) // 250 快过期
	p.SetPaidModels(map[string]bool{"paid-model": true})
	return p
}

// TestStickyKeepsSessionOnPaidModel 已绑定会话即使别的号有快过期积分，
// 也必须保持原号——会话连续性优先。
func TestStickyKeepsSessionOnPaidModel(t *testing.T) {
	p := stickyExpiringPool()
	got, d := p.PickByUIDForModelRealmWithDecision("sticky-no-expiring", "paid-model", "")
	if got == nil {
		t.Fatalf("粘性命中应成功，却返回 nil（decision=%+v）", d)
	}
	if got.UID != "sticky-no-expiring" {
		t.Fatalf("粘性命中 = %s，应保持原号（不做快过期让路）", got.UID)
	}
	if d.ReasonCode != "sticky_hit" {
		t.Errorf("reason_code = %q want sticky_hit", d.ReasonCode)
	}
}

// TestNewSessionPrefersExpiring 新建会话（无粘性）时，付费模型仍只从
// 有快过期积分的号里选——这是「只在新建会话时优先」的正面断言。
func TestNewSessionPrefersExpiring(t *testing.T) {
	p := stickyExpiringPool()
	// PickExcludingForRealm 就是新建会话走的路径（stickyUID 为空时 handler 调它）。
	for i := 0; i < 50; i++ {
		got := p.PickExcludingForRealm(nil, "paid-model", "")
		if got == nil {
			t.Fatal("新建会话选号返回 nil")
		}
		if got.UID != "other-with-expiring" {
			t.Fatalf("第 %d 次新建会话选中 %s，应只选有快过期积分的号", i, got.UID)
		}
	}
}

// TestNewSessionFreeModelIgnoresExpiring 免费模型不做该优先（不扣积分，无收益）。
func TestNewSessionFreeModelIgnoresExpiring(t *testing.T) {
	p := stickyExpiringPool()
	seen := map[string]int{}
	for i := 0; i < 300; i++ {
		got := p.PickExcludingForRealm(nil, "free-model", "")
		if got == nil {
			t.Fatal("选号返回 nil")
		}
		seen[got.UID]++
	}
	if seen["sticky-no-expiring"] == 0 {
		t.Errorf("免费模型不该被快过期过滤：300 次里无快过期的号一次没被选中（seen=%v）", seen)
	}
}

// TestStickyStillWorksWhenAllHaveExpiring 全池都有快过期时，粘性照常。
func TestStickyStillWorksWhenAllHaveExpiring(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "a"})
	p.Add(&auth.Auth{UID: "b"})
	p.SetCreditsDetailed("a", 350, 100)
	p.SetCreditsDetailed("b", 350, 250)
	p.SetPaidModels(map[string]bool{"paid-model": true})

	got, _ := p.PickByUIDForModelRealmWithDecision("a", "paid-model", "")
	if got == nil || got.UID != "a" {
		t.Fatalf("全池都有快过期时应保持粘性，got=%v", got)
	}
}

// TestStickyWorksWhenNoExpiringAnywhere 全池都无快过期时，粘性照常。
func TestStickyWorksWhenNoExpiringAnywhere(t *testing.T) {
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
