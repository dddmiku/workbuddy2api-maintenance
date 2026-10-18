package pool

import (
	"testing"
	"time"

	"workbuddy2api/internal/auth"
)

// P1-5（发现 5）：CoolRemaining 口径补 breakerUntil。
// statusOf 的 Cooling 判定含熔断期（now.Before(e.breakerUntil)），但 CoolRemaining
// 只算 time.Until(e.until)——熔断冷却的号显示「cooling=true 却剩余 0 秒」。
// 修正口径：CoolRemaining = max(time.Until(e.until), time.Until(e.breakerUntil))。

// setCoolWindows 直接写入 until/breakerUntil（绕过触发路径，隔离测试口径本身）。
//
// 2026-10-18：账号级 until 现在只承载积分不足（CoolHard），不再有 soft_rate 账号冷却。
// 直接写 until 而不设 coolKind 时零值即 CoolHard，因此这些用例验证的是
// 「余额不足 + 熔断并存时 CoolRemaining 取谁」这一口径。
func setCoolWindows(p *Pool, uid string, until, breakerUntil time.Time) {
	p.mu.Lock()
	e := p.byUID[uid]
	e.until = until
	e.coolKind = CoolHard // 显式：until 只表示积分不足
	e.breakerUntil = breakerUntil
	p.mu.Unlock()
}

// breakerCoolRemainingAt 断言 CoolRemaining ≈ want（±tol，容忍测试内 tick 漂移）。
func breakerCoolRemainingAt(t *testing.T, p *Pool, uid string, want int64, tol int64) {
	t.Helper()
	st, ok := p.Status(uid)
	if !ok {
		t.Fatalf("status(%s) missing", uid)
	}
	// 余额不足不计入 Cooling（不是冷却），但仍要有剩余时长可显示。
	if !st.Cooling && !st.CreditsExhausted {
		t.Fatalf("%s should be cooling or credits-exhausted: %+v", uid, st)
	}
	if got := st.CoolRemaining; got < want-tol || got > want+tol {
		t.Errorf("cool_remaining_sec=%d want ~%d (±%d)", got, want, tol)
	}
}

// 熔断冷却（breakerUntil 未来、until 已过期）→ CoolRemaining > 0（原 bug：0 秒）。
func TestCoolRemainingBreakerOnly(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	setCoolWindows(p, "u1", time.Now().Add(-time.Minute), time.Now().Add(30*time.Minute))
	breakerCoolRemainingAt(t, p, "u1", 30*60, 5)
}

// 积分不足（until 未来、breakerUntil 已过期）→ CoolRemaining > 0（原行为不变）。
func TestCoolRemainingUntilOnly(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	setCoolWindows(p, "u1", time.Now().Add(10*time.Minute), time.Now().Add(-time.Minute))
	breakerCoolRemainingAt(t, p, "u1", 10*60, 5)
}

// 两者都在未来 → 取较大者（熔断更远）。
func TestCoolRemainingBothFutureTakesMaxBreaker(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	setCoolWindows(p, "u1", time.Now().Add(5*time.Minute), time.Now().Add(45*time.Minute))
	breakerCoolRemainingAt(t, p, "u1", 45*60, 5)
}

// 两者都在未来 → 取较大者（until 更远）。
func TestCoolRemainingBothFutureTakesMaxUntil(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	setCoolWindows(p, "u1", time.Now().Add(50*time.Minute), time.Now().Add(8*time.Minute))
	breakerCoolRemainingAt(t, p, "u1", 50*60, 5)
}

// 两者都过期 → 不进入 Cooling 分支，CoolRemaining 保持 0。
func TestCoolRemainingBothExpired(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	setCoolWindows(p, "u1", time.Now().Add(-time.Minute), time.Now().Add(-time.Minute))
	st, _ := p.Status("u1")
	if st.Cooling || st.CoolRemaining != 0 {
		t.Errorf("both expired: cooling=%v cool_remaining=%d want false/0", st.Cooling, st.CoolRemaining)
	}
}
