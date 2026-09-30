// ═══ 更新日志 ═══
// 2026-09-25：复现并发在途请求的迟到冷却覆盖余额不足或管理员禁用状态，锁定错误不能解除已有更强限制。
package pool

import (
	"path/filepath"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
)

type lateCooldownCase struct {
	name  string
	apply func(*Pool)
}

func lateCooldownCases() []lateCooldownCase {
	return []lateCooldownCase{
		{"fixed_soft", func(p *Pool) { p.Cooldown("u1", CoolSoft, time.Minute, "late fault") }},
		{"account_rate", func(p *Pool) { p.CooldownSoftRate("u1", time.Minute, time.Time{}, "late 429") }},
		{"account_reset", func(p *Pool) { p.CooldownSoftRate("u1", time.Minute, time.Now().Add(time.Minute), "late reset") }},
		{"model_without_reset", func(p *Pool) { p.CooldownSoftForModel("u1", time.Minute, time.Time{}, "model", "late 429") }},
	}
}

func TestLateSoftCooldownCannotReleaseBalanceRestriction(t *testing.T) {
	for _, tc := range lateCooldownCases() {
		t.Run(tc.name, func(t *testing.T) {
			p := New("")
			p.Add(&auth.Auth{UID: "u1"})
			p.Cooldown("u1", CoolHard, time.Hour, "balance depleted")
			before, _ := p.Status("u1")
			// Two in-flight requests finish in this order. Another error is not
			// evidence that the balance recovered.
			tc.apply(p)
			after, _ := p.Status("u1")
			if after.CoolKind != before.CoolKind || !after.Until.Equal(before.Until) || after.Reason != before.Reason {
				t.Errorf("late error replaced balance restriction: before=%+v after=%+v", before, after)
			}
			if got := p.PickExcludingForRealm(nil, "model", "cn"); got != nil {
				t.Fatal("late soft error made the exhausted account selectable through fallback")
			}
		})
	}
}

func TestLateCooldownPreservesDisabledState(t *testing.T) {
	cases := lateCooldownCases()
	cases = append(cases,
		lateCooldownCase{"hard", func(p *Pool) { p.Cooldown("u1", CoolHard, time.Hour, "late balance") }},
		lateCooldownCase{"model_reset", func(p *Pool) {
			p.CooldownSoftForModel("u1", time.Minute, time.Now().Add(time.Hour), "model", "late 6004")
		}},
		lateCooldownCase{"model_block", func(p *Pool) { p.BlockModelBackoff("u1", "model", "11102 model not available") }},
	)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := New("")
			p.Add(&auth.Auth{UID: "u1"})
			p.Disable("u1", "disabled by administrator")
			tc.apply(p)
			st, _ := p.Status("u1")
			if !st.Disabled || st.DisabledReason != "disabled by administrator" || !st.Until.IsZero() || len(st.RateLimitedModels) != 0 {
				t.Fatalf("late in-flight failure mutated disabled state: %+v", st)
			}
			if st.LastErrTime.IsZero() {
				t.Fatal("late upstream error must still update the last-activity observation")
			}
		})
	}
}

func TestLateCooldownMergePreservesStrongerRestriction(t *testing.T) {
	for _, disabled := range []bool{false, true} {
		for _, tc := range lateCooldownCases() {
			name := tc.name
			if disabled {
				name += "/disabled"
			} else {
				name += "/balance"
			}
			t.Run(name, func(t *testing.T) {
				file := filepath.Join(t.TempDir(), "state.json")
				seed := New(file)
				seed.Add(&auth.Auth{UID: "u1"})
				seed.SetCredits("u1", 100)
				seed.Close()
				current, stale := New(file), New(file)
				t.Cleanup(current.Close)
				t.Cleanup(stale.Close)
				if disabled {
					current.Disable("u1", "operator disabled")
				} else {
					current.Cooldown("u1", CoolHard, time.Hour, "balance depleted")
				}
				before, _ := current.Status("u1")
				current.Flush()
				// An old draining process still has the pre-error in-memory view.
				tc.apply(stale)
				stale.Flush()
				after := runtimeReopenStatus(t, file, "u1")
				if after.Disabled != disabled || after.CoolKind != before.CoolKind || !after.Until.Equal(before.Until) || after.Reason != before.Reason || after.DisabledReason != before.DisabledReason {
					t.Fatalf("old process cooldown overwrote stronger persisted restriction: before=%+v after=%+v", before, after)
				}
				if after.LastErrTime.IsZero() {
					t.Fatal("late error observation was lost during merge")
				}
			})
		}
	}
}

// TestLateReviveKeepsNewerSoftCooldown 热更新期间，旧（排水中）实例仍以为账号是
// 余额不足（CoolHard）并完成签到解冻；新实例同时刚记下 429 软冷却（CoolSoft）并落盘。
// 迟到的解冻不是「限流已恢复」的证据，不得抹掉新实例记的软冷却——否则下次重启/采用
// 快照后会在被限流的账号上恢复选号（2026-09-30 深度体检发现）。
func TestLateReviveKeepsNewerSoftCooldown(t *testing.T) {
	file := filepath.Join(t.TempDir(), "state.json")
	seed := New(file)
	seed.Add(&auth.Auth{UID: "u1"})
	seed.SetCredits("u1", 100)
	// 短硬冷却：盘上留 CoolHard 记录，等它按墙钟过期（真实场景是余额冷却到期后
	// 账号仍在服务，此时旧实例的内存视图还是 CoolHard）。
	seed.Cooldown("u1", CoolHard, 50*time.Millisecond, "balance depleted")
	seed.Close()
	time.Sleep(80 * time.Millisecond)

	// 旧实例仍持有 CoolHard 的内存视图；新实例接管并记下 429 软冷却。
	old, fresh := New(file), New(file)
	t.Cleanup(old.Close)
	t.Cleanup(fresh.Close)
	fresh.CooldownSoftRate("u1", time.Minute, time.Now().Add(30*time.Minute), "429 rate limit")
	fresh.Flush()

	// 旧实例的签到完成得晚：余额恢复 → 只解冻它以为的余额不足冷却。
	old.ReenableIfCredits("u1", 500)
	old.Flush()

	after := runtimeReopenStatus(t, file, "u1")
	if !after.Cooling || after.Until.IsZero() || after.CoolKind != CoolSoft.String() {
		t.Fatalf("迟到的解冻抹掉了新实例的 429 软冷却: %+v", after)
	}
}
