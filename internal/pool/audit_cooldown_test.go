// ═══ 更新日志 ═══
// 2026-09-24：复现首轮冷却越过封顶、复合冷却探活误报及余额刷新抹掉模型限制。
// 2026-09-24：验证模型不可用的半开退避保留计数且过期历史有界清理。
package pool

import (
	"path/filepath"
	"strconv"
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

// TestAuditModelCooldownsAreBounded 模型名是客户端自选字段，模型级冷却条目必须有上限：
// 无上限时每个新名字都会留下一条 6-24h 的条目并写进 state.json（2026-09-30 深度体检发现）。
// 超限时先丢最早到期的，正在生效的冷却保留优先权。
func TestAuditModelCooldownsAreBounded(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})

	// 一条到期最晚的冷却（11102 连打到 24h 封顶）与大量 6h 短退避混排：
	// 裁剪丢最早到期的，24h 的条目必须仍在。
	for i := 0; i < 8; i++ {
		p.BlockModelBackoff("u1", "keep-me", "11102 model not available")
	}
	for i := 0; i < maxModelCooldowns*3; i++ {
		p.BlockModelBackoff("u1", "filler-"+strconv.Itoa(i), "11102 model not available")
	}

	p.mu.RLock()
	n := len(p.byUID["u1"].modelCooldowns)
	_, kept := p.byUID["u1"].modelCooldowns["keep-me"]
	p.mu.RUnlock()
	if n > maxModelCooldowns {
		t.Fatalf("模型级冷却条目=%d 超过上限 %d（客户端自选模型名可把 state.json 撑大）", n, maxModelCooldowns)
	}
	if !kept {
		t.Fatal("裁剪应丢最早到期的条目，24h 封顶的 keep-me 不得被 6h 退避挤掉")
	}
}

// TestAuditModelCooldownsTrimmedOnLoad 旧版本写下的 state.json 可能带任意多条模型级冷却，
// 读入时必须裁剪，否则重启后立刻又占住一份无界内存（2026-09-30 深度体检发现）。
func TestAuditModelCooldownsTrimmedOnLoad(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "state.json")
	seed := New(fp)
	seed.Add(&auth.Auth{UID: "u1"})
	for i := 0; i < maxModelCooldowns*2; i++ {
		seed.BlockModelBackoff("u1", "m-"+strconv.Itoa(i), "11102 model not available")
	}
	seed.Close()

	reopened := New(fp)
	t.Cleanup(reopened.Close)
	reopened.Add(&auth.Auth{UID: "u1"})
	reopened.mu.RLock()
	n := len(reopened.byUID["u1"].modelCooldowns)
	reopened.mu.RUnlock()
	if n > maxModelCooldowns {
		t.Fatalf("读入后模型级冷却=%d 超过上限 %d", n, maxModelCooldowns)
	}
}
