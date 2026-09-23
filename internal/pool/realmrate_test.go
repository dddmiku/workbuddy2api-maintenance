// ═══ 更新日志 ═══
// 2026-09-23：新增 RealmRateStateForModel 回归：只有「全部因限流/禁用不可用」才
//
//	满足 AllRateLimited；存在熔断或在途占满时不满足。
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
	if state.Accounts != 2 || state.Available != 0 || state.RateLimited != 2 {
		t.Fatalf("unexpected state: %+v", state)
	}
	// CN 仍有可用号，回落目标有效。
	if cn := p.RealmRateStateForModel("cn", "deepseek-v4.1-flash"); cn.Available == 0 {
		t.Fatalf("cn should still have a healthy account: %+v", cn)
	}
}

func TestRealmRateStateBlocksFallbackOnBreaker(t *testing.T) {
	p, accounts := newRealmTestPool(t, "global", "global", "cn")
	// 一个软冷却、一个熔断：熔断不是限流，不该被静默改道到 CN。
	p.CooldownSoftRate(accounts[0].UID, time.Minute, time.Time{}, "429 rate limit")
	p.NoteError(accounts[1].UID)
	p.NoteError(accounts[1].UID)
	p.NoteError(accounts[1].UID) // 达 breakerThreshold 触发熔断

	state := p.RealmRateStateForModel("global", "deepseek-v4.1-flash")
	if state.AllRateLimited() {
		t.Fatalf("a tripped breaker must block fallback: %+v", state)
	}
}

func TestRealmRateStateNoAccounts(t *testing.T) {
	p, _ := newRealmTestPool(t, "cn")
	state := p.RealmRateStateForModel("global", "deepseek-v4.1-flash")
	if state.AllRateLimited() {
		t.Fatal("a realm with no accounts must not report all-rate-limited")
	}
}
