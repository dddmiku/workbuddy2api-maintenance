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

// 2026-10-18 改：限流只写模型级台账，账号级 until 不再承载 429。
// 本用例验证两条模型级入口的首轮都被 softRateMax 封顶。
func TestAuditFirstSoftCooldownHonorsCap(t *testing.T) {
	for _, entry := range []string{"soft_rate_for_model", "soft_for_model"} {
		t.Run(entry, func(t *testing.T) {
			p := New("")
			p.Add(&auth.Auth{UID: "u1"})
			p.SetSoftRateMax(time.Minute)
			if entry == "soft_for_model" {
				p.CooldownSoftForModel("u1", time.Hour, time.Time{}, "model", "429")
			} else {
				p.CooldownSoftRateForModel("u1", time.Hour, time.Time{}, CooldownTestModel, "429")
			}
			if st, _ := p.Status("u1"); st.Cooling || !st.Until.IsZero() {
				t.Fatalf("限流不得写账号级冷却: cooling=%t until=%v", st.Cooling, st.Until)
			}
			model := "model"
			if entry == "soft_rate_for_model" {
				model = CooldownTestModel
			}
			p.mu.RLock()
			mc := p.byUID["u1"].modelCooldowns[model]
			p.mu.RUnlock()
			if mc.Until.IsZero() {
				t.Fatal("首轮冷却应写入模型级台账")
			}
			if remaining := time.Until(mc.Until); remaining > time.Minute {
				t.Fatalf("first cooldown must honor one-minute cap, remaining=%s", remaining)
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

// 2026-10-18 改：账号级只有积分不足（CoolHard）一种状态；限流全在模型级。
// 本用例验证签到续期只清「积分不足」，模型限流原样保留。
func TestAuditCreditRefreshPreservesUnrelatedLimits(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	p := New(path)
	t.Cleanup(p.Close)
	p.Add(&auth.Auth{UID: "u1"})
	p.Cooldown("u1", CoolHard, time.Hour, "余额不足")
	p.CooldownSoftForModel("u1", time.Minute, time.Now().Add(time.Hour), "daily-quota", "6004")
	p.BlockModelBackoff("u1", "unavailable", "11102 model not available")
	p.ReenableIfCredits("u1", 500)
	check := func(current *Pool) {
		t.Helper()
		st, _ := current.Status("u1")
		if st.Credits != 500 {
			t.Errorf("credit refresh was not retained: %d", st.Credits)
		}
		if st.Cooling || st.CreditsExhausted {
			t.Errorf("credit refresh must clear balance exhaustion: cooling=%t exhausted=%t", st.Cooling, st.CreditsExhausted)
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
}

// 2026-10-18 改：账号级限流已取消，这里改为验证「积分不足」与模型限制正交——
// 两者互不覆盖，积分恢复后模型限制仍在，且不同模型各自独立。
func TestAuditAccountCooldownPreservesModelLimits(t *testing.T) {
	for _, source := range []string{"hard_credit", "soft_rate", "model_without_reset"} {
		t.Run(source, func(t *testing.T) {
			p := New("")
			// 非空倍率表：other-model 记为免费，积分不足不该拦它。
			p.SetPaidModels(map[string]bool{"paid-model": true})
			p.Add(&auth.Auth{UID: "u1"})
			p.CooldownSoftForModel("u1", time.Minute, time.Now().Add(time.Hour), "daily-quota", "6004")
			p.BlockModelBackoff("u1", "unavailable", "11102 model not available")
			switch source {
			case "hard_credit":
				p.Cooldown("u1", CoolHard, time.Minute, "余额不足")
			case "soft_rate":
				p.CooldownSoftRateForModel("u1", time.Minute, time.Time{}, CooldownTestModel, "429")
			default:
				p.CooldownSoftForModel("u1", time.Minute, time.Time{}, "different-model", "429")
			}
			for _, model := range []string{"daily-quota", "unavailable"} {
				p.mu.RLock()
				mc, ok := p.byUID["u1"].modelCooldowns[model]
				p.mu.RUnlock()
				if !ok || !time.Now().Before(mc.Until) {
					t.Errorf("短冷却/积分不足不得抹掉更长的模型限制 %s", model)
				}
			}
			if got := p.PickExcludingForRealm(nil, "other-model", "cn"); got == nil {
				t.Fatal("无关模型必须可用（账号未被整体冷却）")
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
	// 11102 不进限流台账（2026-10-18：模型不存在 ≠ 账号被限流），但退避本身仍要
	// 按指数累积——这是「别再立刻重试」的节流，与台账展示是两件事。
	st, _ := p.Status("u1")
	if len(st.RateLimitedModels) != 0 {
		t.Fatalf("11102 不应进限流台账，got %+v", st.RateLimitedModels)
	}
	p.mu.Lock()
	mc = p.byUID["u1"].modelCooldowns["unavailable"]
	remaining := time.Until(mc.Until)
	p.mu.Unlock()
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
