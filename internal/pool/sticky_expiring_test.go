package pool

// 快过期积分优先的作用范围用例。
//
// ═══ 决策沿革（两次改向，别再翻烧饼）═══
// 2026-11-01 v3.15.1：粘性路径也让路（用户要求「快过期先被用」）。
// 2026-11-01 v3.15.2：撤销，改成「只在新建会话时优先」——担心换号破坏会话连续性。
// 2026-11-04 v3.15.3：再改回「粘性也让路」。现场数据推翻了「新建会话足够」的假设：
//   9862 次决策里 9822 次是 sticky_hit，全池 5 个带快过期积分的号一分没动，
//   流量全压在同一个无快过期积分的号上，已花 2000+ 积分（用户报障）。
//   粘性主导的负载下，「只在新建会话时优先」等于该特性不存在。
//
// 因此现在钉住三件事：
//   1. 粘性命中时，若别的号有快过期积分而本号没有 → 让路（返回 nil）；
//   2. 本号自己有快过期积分、或全池都没有 → 保持粘性；
//   3. 新建会话（无粘性）时付费模型只从有快过期积分的号里选。

import (
	"testing"
	"time"

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

// TestStickyYieldsToExpiring 已绑定会话在付费模型上，若别的号有快过期积分而
// 本号没有，粘性必须让路——否则快过期积分永远轮不到被消耗。
//
// 现场依据（2026-11-04）：9822/9862 次决策是 sticky_hit，5 个带快过期积分的号
// 闲置，单一无快过期积分的号被消耗 2000+。
func TestStickyYieldsToExpiring(t *testing.T) {
	p := stickyExpiringPool()
	got, d := p.PickByUIDForModelRealmWithDecision("sticky-no-expiring", "paid-model", "")
	if got != nil {
		t.Fatalf("粘性号无快过期、别的号有 → 应让路返回 nil，实际选中 %s", got.UID)
	}
	if d.ReasonCode != "expiring_deferred" {
		t.Errorf("reason_code = %q want expiring_deferred", d.ReasonCode)
	}
}

// TestStickyYieldThenReselect 让路后回落普通选号，必须落到有快过期积分的号上。
// 这两步合起来才是 handler 的真实链路：解绑 → PickExcludingForRealm。
func TestStickyYieldThenReselect(t *testing.T) {
	p := stickyExpiringPool()
	if got, _ := p.PickByUIDForModelRealmWithDecision("sticky-no-expiring", "paid-model", ""); got != nil {
		t.Fatalf("应让路，实际 %s", got.UID)
	}
	for i := 0; i < 20; i++ {
		got := p.PickExcludingForRealm(nil, "paid-model", "")
		if got == nil {
			t.Fatal("回落选号返回 nil")
		}
		if got.UID != "other-with-expiring" {
			t.Fatalf("第 %d 次回落选中 %s，应只选有快过期积分的号", i, got.UID)
		}
	}
}

// TestNewSessionPrefersExpiring 新建会话（无粘性）时，付费模型仍只从
// 有快过期积分的号里选。
func TestNewSessionPrefersExpiring(t *testing.T) {
	p := stickyExpiringPool()
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

// TestStickyFreeModelNeverYields 免费模型上粘性永不让路（不扣积分，无收益）。
func TestStickyFreeModelNeverYields(t *testing.T) {
	p := stickyExpiringPool()
	got, _ := p.PickByUIDForModelRealmWithDecision("sticky-no-expiring", "free-model", "")
	if got == nil || got.UID != "sticky-no-expiring" {
		t.Fatalf("免费模型应保持粘性，got=%v", got)
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

// TestStickyDoesNotYieldToUnavailableExpiringAccount 让路的对象必须此刻真能服务
// 这个模型：让给一个正在冷却的号等于白切一次、还把会话粘性弄丢。
func TestStickyDoesNotYieldToUnavailableExpiringAccount(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "sticky"})
	p.Add(&auth.Auth{UID: "cooled"})
	p.SetCreditsDetailed("sticky", 350, 0)
	p.SetCreditsDetailed("cooled", 350, 250)
	p.SetPaidModels(map[string]bool{"paid-model": true})
	// 给有快过期积分的号打上「该模型限流」冷却。
	p.CooldownSoftForModel("cooled", time.Minute, time.Time{}, "paid-model", "test")

	got, d := p.PickByUIDForModelRealmWithDecision("sticky", "paid-model", "")
	if got == nil || got.UID != "sticky" {
		t.Fatalf("有快过期积分的号不可用时不该让路，got=%v rc=%q", got, d.ReasonCode)
	}
}
