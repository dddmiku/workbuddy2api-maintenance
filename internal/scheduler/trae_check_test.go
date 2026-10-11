// ═══ 更新日志 ═══
// 2026-10-11：Trae 签到拆成独立任务（独立站点、独立排程、独立开关），
// 并锁定「9074 限流当天不再重试」这一核心修复。
package scheduler

import (
	"testing"
	"time"
)

// TestTraeCheckIsSeparateTask Trae 签到必须是独立任务，而不是挂在通用签到下。
//
// 用户要求：「trae 的签到走 trae 的定时，workbuddy 的走 workbuddy 的，他们不是
// 同一个站点」。旧实现把 trae 塞在 TaskKeyCheckin 里，导致三个后果：
//   - 面板把 trae 归到 WorkBuddy 的「签到」分组下；
//   - 无法单独关掉其中一个；
//   - 每次通用签到调度都会对 trae 号重打 claim（9074 限流被反复延长）。
func TestTraeCheckIsSeparateTask(t *testing.T) {
	s := New(Config{})
	snap := s.TaskSnapshot()
	byKey := map[string]TaskInfo{}
	for _, info := range snap {
		byKey[info.Key] = info
	}
	tc, ok := byKey["trae_check"]
	if !ok {
		t.Fatal("快照里应有独立的 trae_check 任务")
	}
	// 域必须只声明 trae：它是 Trae 站点的任务。
	if len(tc.Realms) != 1 || tc.Realms[0] != "trae" {
		t.Errorf("trae_check 的 realms=%v，应只有 trae", tc.Realms)
	}
	// 通用签到不再声明 trae（已剥离）。
	ci := byKey["checkin"]
	for _, r := range ci.Realms {
		if r == "trae" {
			t.Errorf("checkin 的 realms 仍含 trae（%v），应已剥离", ci.Realms)
		}
	}
	if len(ci.Realms) != 2 {
		t.Errorf("checkin 的 realms=%v，应为 cn/global 两域", ci.Realms)
	}
}

// TestTraeCheckScheduleIndependent Trae 签到有自己的排程小时与开关。
func TestTraeCheckScheduleIndependent(t *testing.T) {
	s := New(Config{TraeCheckHours: []int{7}})
	// 关掉 WorkBuddy 签到，Trae 签到照跑（两个站点互不影响）。
	s2 := New(Config{
		CheckinDisabled:   true,
		TraeCheckHours:    []int{7},
		TravelDisabled:    true,
		ActivityDisabled:  true,
		KeepaliveDisabled: true,
		SchoolDisabled:    true,
		CatDisabled:       true,
		RedeemDisabled:    true,
		LotteryDisabled:   true,
		MakeupDisabled:    true,
	})
	at, kinds := s2.nextWake(time.Date(2026, 9, 11, 6, 0, 0, 0, time.Local))
	if want := time.Date(2026, 9, 11, 7, 0, 0, 0, time.Local); !at.Equal(want) {
		t.Errorf("next=%v want %v（WorkBuddy 签到禁用，Trae 签到 07:00 照跑）", at, want)
	}
	if !hasKind(kinds, taskTraeCheck) {
		t.Errorf("kinds=%v 应含 trae_check", kinds)
	}
	if hasKind(kinds, taskCheckin) {
		t.Errorf("kinds=%v 不应含 checkin（已禁用）", kinds)
	}
	// 默认小时与快照一致。
	if got := s.TaskSnapshot()[1]; got.Key != "trae_check" || len(got.Hours) != 1 || got.Hours[0] != 7 {
		t.Errorf("trae_check 小时应为 [7]，实际 %v", got.Hours)
	}
}

// TestTraeRateLimitSkipsRestOfDay 9074 一旦出现，当天不再对该账号重试。
//
// 这是本轮修复的核心。上游对 claim 有账号维度限流，旧实现每轮调度都无差别重打，
// 限流窗口被一轮轮延长（2026-10-11 实测：同一对号 17:34 / 19:31 / 21:00 / 21:23
// 四轮全吃 9074，一天一个都没签上）。
func TestTraeRateLimitSkipsRestOfDay(t *testing.T) {
	s := New(Config{})
	day := travelDay(time.Now())
	if s.traeRateLimitedToday("u1", day) {
		t.Fatal("初始不该被标记为限流")
	}
	s.noteTraeRateLimited("u1", day)
	if !s.traeRateLimitedToday("u1", day) {
		t.Fatal("记下之后当天应判定为限流")
	}
	// 别的账号不受影响。
	if s.traeRateLimitedToday("u2", day) {
		t.Fatal("限流按账号维度，不该波及其它号")
	}
	// 次日自动失效（自然日切换）。
	if s.traeRateLimitedToday("u1", travelDay(time.Now().Add(24*time.Hour))) {
		t.Fatal("跨自然日后应重新可试")
	}
}
