// ═══ 更新日志 ═══
// 2026-09-23：新增 RealmRateStateForModel 回归：只有「全部因限流/禁用不可用」才
//
//	满足 AllRateLimited；存在熔断或在途占满时不满足。
//
// 2026-10-15 改口径：**不可用即可**，不再要求「必须全部因限流」。原口径让余额不足
//
//	（CoolHard）的账号把等式打破，CN 回落永不触发（用户实测：所有 global 的
//	glm-5.3 都冷却了却没回落）。见 state.go 的 RealmRateState 注释。
package pool

import (
	"testing"
	"time"

	"workbuddy2api/internal/auth"
)

// newRealmTestPool 造一个含指定域账号的池。域由 Domain 决定（与 realm_test.go 同口径）：
// www.workbuddy.ai → global，其它/空 → cn。
func newRealmTestPool(t *testing.T, realms ...string) (*Pool, []*auth.Auth) {
	t.Helper()
	withNoPickGap(t)
	auth.SetGlobalEnabled(true)
	t.Cleanup(func() { auth.SetGlobalEnabled(true) })
	p := New("")
	accounts := make([]*auth.Auth, 0, len(realms))
	for index, realm := range realms {
		account := &auth.Auth{UID: realm + "-" + string(rune('a'+index))}
		if realm == "global" {
			account.Domain = "www.workbuddy.ai"
		}
		p.Add(account)
		accounts = append(accounts, account)
	}
	return p, accounts
}

func TestRealmRateStateAllRateLimited(t *testing.T) {
	p, accounts := newRealmTestPool(t, "global", "global", "cn")
	// 两个 global 都进入软冷却（无重置时间的 429 口径）。
	for _, account := range accounts[:2] {
		p.CooldownSoftRate(account.UID, time.Minute, time.Time{}, "429 rate limit")
	}
	state := p.RealmRateStateForModel("global", "deepseek-v4.1-flash")
	if !state.AllRateLimited() {
		t.Fatalf("all global accounts rate limited should allow fallback: %+v", state)
	}
	if state.Accounts != 2 || state.Available != 0 || state.RateLimited != 2 || state.Unavailable != 2 {
		t.Fatalf("unexpected state: %+v", state)
	}
	// CN 仍有可用号，回落目标有效。
	if cn := p.RealmRateStateForModel("cn", "deepseek-v4.1-flash"); cn.Available == 0 {
		t.Fatalf("cn should still have a healthy account: %+v", cn)
	}
}

// 2026-10-15 改：熔断**不再**阻止回落。回落的判据是「这个域还有没有能用的号」，
// 熔断的号同样不可用（调了大概率再挂一轮），它让请求失败的方式与限流没有区别。
// 旧口径（熔断阻止回落）会让「一个软冷却 + 一个熔断」的域既不回落又全都失败。
func TestRealmRateStateAllowsFallbackOnBreaker(t *testing.T) {
	p, accounts := newRealmTestPool(t, "global", "global", "cn")
	// 一个软冷却、一个熔断：两个都不可用 → 该域没有可服务号 → 应允许回落。
	p.CooldownSoftRate(accounts[0].UID, time.Minute, time.Time{}, "429 rate limit")
	p.NoteError(accounts[1].UID)
	p.NoteError(accounts[1].UID)
	p.NoteError(accounts[1].UID) // 达 breakerThreshold 触发熔断

	state := p.RealmRateStateForModel("global", "deepseek-v4.1-flash")
	if state.Available != 0 {
		t.Fatalf("both accounts are unusable, none should be available: %+v", state)
	}
	if state.RateLimited != 1 {
		t.Fatalf("only the soft-cooldown account is rate limited: %+v", state)
	}
	if state.Unavailable != 2 {
		t.Fatalf("breaker counts as unavailable too: %+v", state)
	}
	if !state.AllRateLimited() {
		t.Fatalf("a realm with no servable account should allow fallback: %+v", state)
	}
}

// 余额不足（CoolHard）的号是本次修复的直接触发点：它们既不计 Available 也不计
// RateLimited，旧口径下会把等式打破、让回落恒不触发（VPS 实测 13+7=20≠23）。
func TestRealmRateStateCountsHardCreditAsUnavailable(t *testing.T) {
	p, accounts := newRealmTestPool(t, "global", "global", "cn")
	// 一个模型级冷却（限流），一个余额不足（非限流）。
	p.BlockModelBackoff(accounts[0].UID, "deepseek-v4.1-flash", "6004 model rate limit")
	p.Cooldown(accounts[1].UID, CoolHard, time.Hour, "余额不足")

	state := p.RealmRateStateForModel("global", "deepseek-v4.1-flash")
	if state.Available != 0 {
		t.Fatalf("both accounts are unusable: %+v", state)
	}
	if state.RateLimited != 1 {
		t.Fatalf("only the model-cooldown account is rate limited: %+v", state)
	}
	if state.Unavailable != 2 {
		t.Fatalf("hard-credit exhaustion must count as unavailable: %+v", state)
	}
	if !state.AllRateLimited() {
		t.Fatalf("hard-credit accounts must not block CN fallback: %+v", state)
	}
}

// 反向守卫：只要还有一个真能用的号，就绝不回落。这是旧口径真正要守的东西
// （「不该把请求静默改道到另一个域」），换成 Unavailable 口径后必须仍然成立。
func TestRealmRateStateKeepsFallbackBlockedWhenOneAccountWorks(t *testing.T) {
	p, accounts := newRealmTestPool(t, "global", "global", "global", "global", "cn")
	p.Cooldown(accounts[0].UID, CoolHard, time.Hour, "余额不足")
	p.NoteError(accounts[1].UID)
	p.NoteError(accounts[1].UID)
	p.NoteError(accounts[1].UID) // 熔断
	p.CooldownSoftRate(accounts[2].UID, time.Minute, time.Time{}, "429 rate limit")

	state := p.RealmRateStateForModel("global", "deepseek-v4.1-flash")
	if state.Available != 1 {
		t.Fatalf("the fourth account is healthy: %+v", state)
	}
	if state.AllRateLimited() {
		t.Fatalf("a servable account must block fallback: %+v", state)
	}
}

func TestRealmRateStateNoAccounts(t *testing.T) {
	p, _ := newRealmTestPool(t, "cn")
	state := p.RealmRateStateForModel("global", "deepseek-v4.1-flash")
	if state.AllRateLimited() {
		t.Fatal("a realm with no accounts must not report all-rate-limited")
	}
}
