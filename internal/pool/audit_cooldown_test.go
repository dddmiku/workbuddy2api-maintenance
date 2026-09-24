// ═══ 更新日志 ═══
// 2026-09-24：复现首轮冷却越过封顶、复合冷却探活误报及余额刷新抹掉模型限制。
// 2026-09-24：验证模型不可用的半开退避保留计数且过期历史有界清理。
package pool

import (
	"path/filepath"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
)

func TestAuditFirstSoftCooldownHonorsCap(t *testing.T) {
	for _, modelFallback := range []bool{false, true} {
		t.Run(map[bool]string{false: "account", true: "model_without_reset"}[modelFallback], func(t *testing.T) {
			p := New("")
			p.Add(&auth.Auth{UID: "u1"})
			p.SetSoftRateMax(time.Minute)
			if modelFallback {
				p.CooldownSoftForModel("u1", time.Hour, time.Time{}, "model", "429")
			} else {
				p.CooldownSoftRate("u1", time.Hour, time.Time{}, "429")
			}
			st, _ := p.Status("u1")
			if !st.Cooling || time.Until(st.Until) > time.Minute {
				t.Fatalf("first cooldown must honor one-minute cap, cooling=%t remaining=%s", st.Cooling, time.Until(st.Until))
			}
		})
	}
}

func TestAuditAccountCooldownCannotBeExemptedByLateModelLimit(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.Cooldown("u1", CoolHard, time.Hour, "balance depleted")
	// 两条在途请求的错误按此顺序抵达，无需直接写 entry 内部状态。
	p.CooldownSoftForModel("u1", time.Minute, time.Now().Add(time.Hour), "model", "6004")
	if got := p.PickExcludingForRealm(nil, "other-model", "cn"); got != nil {
		t.Fatal("hard-cooled account must not be selectable for any model")
	}
	if p.ServableNow() || p.ServableForRealm("cn") {
		t.Fatal("health check reports ready although the only account remains hard-cooled")
	}
}

func TestAuditCreditRefreshPreservesUnrelatedLimits(t *testing.T) {
	for _, kind := range []CoolKind{CoolHard, CoolSoft} {
		t.Run(kind.String(), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "state.json")
			p := New(path)
			t.Cleanup(p.Close)
			p.Add(&auth.Auth{UID: "u1"})
			p.Cooldown("u1", kind, time.Hour, kind.String())
			p.CooldownSoftForModel("u1", time.Minute, time.Now().Add(time.Hour), "daily-quota", "6004")
			p.BlockModelBackoff("u1", "unavailable", "11102 model not available")
			p.ReenableIfCredits("u1", 500)
			check := func(current *Pool) {
				t.Helper()
				st, _ := current.Status("u1")
				if st.Credits != 500 {
					t.Errorf("credit refresh was not retained: %d", st.Credits)
				}
				if st.Cooling != (kind == CoolSoft) {
					t.Errorf("credit refresh must only clear balance cooldown, kind=%s cooling=%t", kind.String(), st.Cooling)
				}
				for _, model := range []string{"daily-quota", "unavailable"} {
					if got := current.PickExcludingForRealm(nil, model, "cn"); got != nil {
						t.Errorf("credit refresh made %s selectable despite independent upstream restriction", model)
					}
				}
			}
			check(p)
			p.Flush()
			reloaded := New(path)
			t.Cleanup(reloaded.Close)
			reloaded.Add(&auth.Auth{UID: "u1"})
			check(reloaded)
		})
	}
}

func TestAuditAccountCooldownPreservesModelLimits(t *testing.T) {
	for _, source := range []string{"fixed", "soft_rate", "model_without_reset"} {
		t.Run(source, func(t *testing.T) {
			p := New("")
			p.Add(&auth.Auth{UID: "u1"})
			p.CooldownSoftForModel("u1", time.Minute, time.Now().Add(time.Hour), "daily-quota", "6004")
			p.BlockModelBackoff("u1", "unavailable", "11102 model not available")
			switch source {
			case "fixed":
				p.Cooldown("u1", CoolSoft, time.Minute, "429")
			case "soft_rate":
				p.CooldownSoftRate("u1", time.Minute, time.Time{}, "429")
			default:
				p.CooldownSoftForModel("u1", time.Minute, time.Time{}, "different-model", "429")
			}
			p.forceSoftExpired("u1")
			for _, model := range []string{"daily-quota", "unavailable"} {
				if got := p.PickExcludingForRealm(nil, model, "cn"); got != nil {
					t.Errorf("short account cooldown erased longer restriction for %s", model)
				}
			}
			if got := p.PickExcludingForRealm(nil, "other-model", "cn"); got == nil {
				t.Fatal("unrelated models must recover after the account cooldown expires")
			}
		})
	}
}

func TestAuditBlockedModelBackoffSurvivesHalfOpenPick(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.BlockModelBackoff("u1", "unavailable", "11102 model not available")
	p.mu.Lock()
	mc := p.byUID["u1"].modelCooldowns["unavailable"]
	mc.Until = time.Now().Add(-time.Second)
	p.byUID["u1"].modelCooldowns["unavailable"] = mc
	p.mu.Unlock()
	if got := p.PickExcludingForRealm(nil, "unavailable", "cn"); got == nil {
		t.Fatal("expired model restriction must allow a recovery probe")
	}
	p.BlockModelBackoff("u1", "unavailable", "11102 model not available")
	st, _ := p.Status("u1")
	if len(st.RateLimitedModels) != 1 {
		t.Fatal("repeated model restriction missing")
	}
	remaining := time.Until(st.RateLimitedModels[0].Until)
	if remaining < 12*time.Hour-time.Second || remaining > 12*time.Hour {
		t.Fatalf("failed half-open probe lost its backoff history: remaining=%s want 12h", remaining)
	}
}

func TestAuditExpiredModelBlockHistoryRemainsBounded(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.BlockModelBackoff("u1", "unavailable", "11102 model not available")
	p.mu.Lock()
	mc := p.byUID["u1"].modelCooldowns["unavailable"]
	mc.Until = time.Now().Add(-modelBlockMaxTTL - time.Second)
	p.byUID["u1"].modelCooldowns["unavailable"] = mc
	p.mu.Unlock()
	if got := p.PickExcludingForRealm(nil, "other-model", "cn"); got == nil {
		t.Fatal("expired history must not block other models")
	}
	p.mu.RLock()
	_, retained := p.byUID["u1"].modelCooldowns["unavailable"]
	p.mu.RUnlock()
	if retained {
		t.Fatal("expired 11102 history was retained beyond the bounded recovery window")
	}
}
