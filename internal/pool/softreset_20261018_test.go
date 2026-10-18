// ═══ 更新日志 ═══
// 2026-10-18：上游重置墙钟不再被 softRateMax 截断。
//
// 旧实现把 min(resetAt, now+softRateMax) 写进冷却，soft_rate_max 默认 2h。
// 实测（prod, global:deepseek-v4.1-flash）上游要求冷却到次日 02:30（约 18h），
// 本地却只冷 2h：到期放回池子 → 立刻再撞 6004 → 再冷 2h，无限循环。
// 用户看到的现象是「只有几个号显示冷却，但整个模型用不了」。
//
// 本文件锁住新口径：有上游时间就按上游时间；无上游时间才走有界退避。
package pool

import (
	"testing"
	"time"

	"workbuddy2api/internal/auth"
)

// newTestPoolForCooldown 用真实构造函数建一个空池（不落盘：stateFp=""）。
func newTestPoolForCooldown(t *testing.T) *Pool {
	t.Helper()
	return New("")
}

// mustAddAccount 往池里加一个账号并返回 uid。
func mustAddAccount(t *testing.T, p *Pool, uid string) string {
	t.Helper()
	p.Add(&auth.Auth{UID: uid})
	return uid
}

// TestSoftResetHonorsUpstreamWindow 上游给 18 小时就冷 18 小时，不截断到 softRateMax。
func TestSoftResetHonorsUpstreamWindow(t *testing.T) {
	p := newTestPoolForCooldown(t)
	uid := mustAddAccount(t, p, "acc-upstream-window")

	resetAt := time.Now().Add(18 * time.Hour)
	p.CooldownSoftForModel(uid, time.Minute, resetAt, "deepseek-v4.1-flash", "6004 model rate limit")

	e := p.byUID[uid]
	mc, ok := e.modelCooldowns["deepseek-v4.1-flash"]
	if !ok {
		t.Fatal("应写入模型级冷却")
	}
	// 允许 1 秒误差（now 在函数内部重新取过）。
	if d := mc.Until.Sub(resetAt); d > time.Second || d < -time.Second {
		t.Errorf("模型级冷却应为上游墙钟 %v，实际 %v（被截断了？softRateMax=%v）",
			resetAt, mc.Until, p.softRateMaxOr())
	}
	if mc.ResetAt.IsZero() || !mc.ResetAt.Equal(resetAt) {
		t.Errorf("ResetAt 应保留上游原始墙钟，实际 %v", mc.ResetAt)
	}
}

// TestAccountSoftResetHonorsUpstreamWindow 限流只落在模型上：resetAt 不被 2h 封顶砍掉，
// 且**不得**写账号级 until（2026-10-18 起账号级限流冷却已取消）。
func TestAccountSoftResetHonorsUpstreamWindow(t *testing.T) {
	p := newTestPoolForCooldown(t)
	uid := mustAddAccount(t, p, "acc-account-window")

	resetAt := time.Now().Add(9 * time.Hour)
	p.CooldownSoftRateForModel(uid, time.Minute, resetAt, CooldownTestModel, "429 rate limit")

	e := p.byUID[uid]
	mc, ok := e.modelCooldowns[CooldownTestModel]
	if !ok {
		t.Fatal("限流应记在该模型上")
	}
	if d := mc.Until.Sub(resetAt); d > time.Second || d < -time.Second {
		t.Errorf("模型级冷却应为上游墙钟 %v，实际 %v（softRateMax=%v）", resetAt, mc.Until, p.softRateMaxOr())
	}
	if !e.until.IsZero() {
		t.Errorf("限流不得写账号级 until（会摘掉整号），实际 %v", e.until)
	}
}

// TestSoftResetStillBoundedForAbsurdTimes 超出 maxSoftResetWindow 的离谱时间要钳住。
//
// 这条不是业务规则，是防呆：一个解析错误/时钟漂移不该把账号锁死一周以上。
func TestSoftResetStillBoundedForAbsurdTimes(t *testing.T) {
	p := newTestPoolForCooldown(t)
	uid := mustAddAccount(t, p, "acc-absurd")

	resetAt := time.Now().Add(90 * 24 * time.Hour)
	p.CooldownSoftForModel(uid, time.Minute, resetAt, "m", "6004 model rate limit")

	mc := p.byUID[uid].modelCooldowns["m"]
	if mc.Until.After(time.Now().Add(maxSoftResetWindow + time.Minute)) {
		t.Errorf("离谱时间应钳到 maxSoftResetWindow(%v)，实际 %v（now+%v）",
			maxSoftResetWindow, mc.Until, time.Until(mc.Until))
	}
}

// TestSoftResetPastImmediate 上游时间已过期 → 立即恢复（now+1ms），不把号钉死。
func TestSoftResetPastImmediate(t *testing.T) {
	p := newTestPoolForCooldown(t)
	uid := mustAddAccount(t, p, "acc-past")

	p.CooldownSoftForModel(uid, time.Minute, time.Now().Add(-time.Hour), "m", "6004 model rate limit")

	mc := p.byUID[uid].modelCooldowns["m"]
	if time.Until(mc.Until) > time.Second {
		t.Errorf("已过期的上游时间应立即恢复，实际还要等 %v", time.Until(mc.Until))
	}
}

// TestNoResetTimesStillUseBackoff 没有上游时间时仍走有界退避——退避落在**模型**上，
// 不写账号级 until。
func TestNoResetTimesStillUseBackoff(t *testing.T) {
	p := newTestPoolForCooldown(t)
	uid := mustAddAccount(t, p, "acc-no-reset")

	p.CooldownSoftForModel(uid, 10*time.Minute, time.Time{}, "m", "429 rate limit")

	e := p.byUID[uid]
	if !e.until.IsZero() {
		t.Errorf("无 resetAt 时也不得写账号级 until，实际 %v", e.until)
	}
	mc, ok := e.modelCooldowns["m"]
	if !ok {
		t.Fatal("无 resetAt 时应把退避写在该模型上")
	}
	if !time.Now().Before(mc.Until) {
		t.Fatalf("模型级退避应仍在生效，实际 until=%v", mc.Until)
	}
	if d := time.Until(mc.Until); d > p.softRateMaxOr()+time.Second {
		t.Errorf("有界退避不得超过 softRateMax(%v)，实际 %v", p.softRateMaxOr(), d)
	}
}
