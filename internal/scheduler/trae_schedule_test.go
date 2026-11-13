// Trae 签到排程必须真正承担「首签」，不能靠 9074 重试循环顺手做（2026-11-14）。
//
// ═══ 线上实测的现象 ═══
//
// 连续四天日志（2026-11-10 起）：
//
//	11:00:00 trae_check: 开始执行
//	11:00:03 trae 签到完成：共 6，成功 0，已签 0，失败 0，跳过 6
//
//	00:00:15 trae 9074 重试循环：2 个账号退避已到期，开始重试
//	00:05:18 trae 签到完成：共 6，成功 6，已签 0，失败 0，跳过 0
//
// 也就是说：面板上排的 11:00 **一天都没真正签过**；真正签到的是 00:00 的重试
// 循环。根因有两层：
//
//  1. 11:00 本来就晚于当天任何一次签到，只要早于它签过（哪怕前一天残留的
//     9074 痕迹触发重试），到点必然「今天已签」而跳过；
//  2. 9074 痕迹（device_generation / retry_9074_count）是跨天保留的，而
//     「今天已签」在 CST 日切时失效 —— 于是每天 00:00:15 重试循环发现
//     「有痕迹 + 今天没签」，立刻调用 traeCheckAll 把**全部账号**签掉。
//
// 修法：排程点挪到 00:05（日切之后），并给重试循环加闸门——当天排程点没到
// 之前不抢跑，排程点跑过它才接管「撞了 9074 的号」。
package scheduler

import (
	"context"
	"testing"
	"time"

	"workbuddy2api/internal/upstream/trae"
)

// traeLoc 测试固定用 CST，与生产 traeCheckinLocation 同一口径。
var traeLoc = traeCheckinLocation

// TestNextTraeFireUsesMinuteOffset 触发时刻必须是「整点 + 5 分钟」，不是整点。
func TestNextTraeFireUsesMinuteOffset(t *testing.T) {
	now := time.Date(2026, 11, 14, 0, 2, 0, 0, traeLoc)
	at := nextTraeFire(now, []int{0})
	want := time.Date(2026, 11, 14, 0, traeFireMinute, 0, 0, traeLoc)
	if !at.Equal(want) {
		t.Fatalf("nextTraeFire = %v want %v（必须是 00:05 而不是 00:00）", at, want)
	}
	if at.Minute() != 5 {
		t.Fatalf("分钟应为 5，实际 %d", at.Minute())
	}
}

// TestNextTraeFireRollsToNextDay 当天排程点已过 → 顺延到次日同一时刻。
func TestNextTraeFireRollsToNextDay(t *testing.T) {
	now := time.Date(2026, 11, 14, 0, 6, 0, 0, traeLoc) // 00:05 刚过
	at := nextTraeFire(now, []int{0})
	want := time.Date(2026, 11, 15, 0, traeFireMinute, 0, 0, traeLoc)
	if !at.Equal(want) {
		t.Fatalf("nextTraeFire = %v want %v（当天已过应顺延次日）", at, want)
	}
}

// TestLastTraeFireBoundary 排程点前后是「今天还没到」与「今天已过」的分界。
//
// 这个判据是重试循环闸门的核心：它必须严格区分 00:04:59 与 00:05:00。
func TestLastTraeFireBoundary(t *testing.T) {
	hours := []int{0}
	before := time.Date(2026, 11, 14, 0, 4, 59, 0, traeLoc)
	if got := lastTraeFire(before, hours); !got.IsZero() {
		t.Errorf("00:04:59 时今天的签到点还没到，应返回零值，实际 %v", got)
	}
	after := time.Date(2026, 11, 14, 0, 5, 0, 0, traeLoc)
	got := lastTraeFire(after, hours)
	want := time.Date(2026, 11, 14, 0, 5, 0, 0, traeLoc)
	if !got.Equal(want) {
		t.Errorf("00:05:00 时今天的签到点刚到，应返回该时刻，实际 %v", got)
	}
	// 当天更晚的时刻仍然返回今天那个点（不是零、不是明天）。
	late := time.Date(2026, 11, 14, 23, 0, 0, 0, traeLoc)
	if got := lastTraeFire(late, hours); !got.Equal(want) {
		t.Errorf("23:00 时仍应返回今天的 00:05，实际 %v", got)
	}
}

// TestNextWakeTraeUsesMinuteOffset 主循环派发 Trae 时必须用 00:05 这个时刻。
//
// 与 TaskSnapshot 的 NextRun 同源：两边都走 nextTraeFire，面板与实际触发
// 不会出现「00:00 / 00:05」的漂移。
func TestNextWakeTraeUsesMinuteOffset(t *testing.T) {
	s := New(Config{
		TraeCheckHours: []int{0},
		// 其余任务全部禁用，只留 Trae，避免别的整点抢成 earliest。
		CheckinDisabled: true, TravelDisabled: true, ActivityDisabled: true,
		KeepaliveDisabled: true, SchoolDisabled: true, CatDisabled: true,
		GrowthDisabled: true, BalanceDisabled: true,
	})
	now := time.Date(2026, 11, 14, 0, 2, 0, 0, traeLoc)
	at, kinds := s.nextWake(now)
	want := time.Date(2026, 11, 14, 0, traeFireMinute, 0, 0, traeLoc)
	if !at.Equal(want) {
		t.Fatalf("nextWake = %v want %v", at, want)
	}
	if len(kinds) != 1 || kinds[0] != taskTraeCheck {
		t.Fatalf("kinds = %v want [trae_check]", kinds)
	}
}

// TestTaskSnapshotTraeFireMinute 面板拿到的 fire_minute 与实际触发一致。
//
// 面板靠这个字段渲染 "00:05"。如果它没下发（零值），前端会显示 00:00，
// 用户看到的是「排了 00:00 却 00:05 才跑」的假象。
func TestTaskSnapshotTraeFireMinute(t *testing.T) {
	s := New(Config{TraeCheckHours: []int{0}})
	var found *TaskInfo
	for i, ti := range s.TaskSnapshot() {
		if ti.Key == string(TaskKeyTraeCheck) {
			found = &s.TaskSnapshot()[i]
			break
		}
	}
	if found == nil {
		t.Fatal("任务快照里没有 trae_check")
	}
	if found.FireMinute != traeFireMinute {
		t.Fatalf("fire_minute = %d want %d", found.FireMinute, traeFireMinute)
	}
	// 其余任务仍必须是整点语义（0），不能被 Trae 的分钟偏移污染。
	for _, ti := range s.TaskSnapshot() {
		if ti.Key != string(TaskKeyTraeCheck) && ti.FireMinute != 0 {
			t.Errorf("%s 的 fire_minute 应为 0，实际 %d", ti.Key, ti.FireMinute)
		}
	}
}

// TestTraeRetryCycleHoldsBeforeScheduledFire 排程点之前重试循环不得抢跑。
//
// 这是本次线上 bug 的直接回归：9074 痕迹跨天保留 + 「今天已签」日切失效，
// 会让 00:00:15 的重试循环把全部账号签掉，11:00 的排程点因此永远空转。
//
// 用 traeRetryCycleAt 注入固定时刻，不依赖真实墙钟（否则用例只能在
// 特定时段跑，CI 上必然 skip，等于没测）。
func TestTraeRetryCycleHoldsBeforeScheduledFire(t *testing.T) {
	stub := &traeCheckinStub{
		statusBody:  `{"code":0,"enable":true,"checked_in":false}`,
		claimBodies: []string{`{"code":0,"message":"success","credits":100}`},
	}
	s, _ := newTraeS(t, stub, t.TempDir())
	s.cfg.TraeCheckHours = []int{0}
	// 造出「有 9074 痕迹且退避已过期」——这正是每天 00:00 的状态。
	s.noteTraeBackoff("2081842786229369")
	s.traeCheckin.update("2081842786229369", func(st *traeCheckinState) {
		st.RetryUpdatedAt = time.Now().Add(-time.Hour).Unix()
	})

	// 00:04:59：排程点（00:05）还差 1 秒，正是线上 00:00:15 抢跑的时刻。
	before := stub.claimCalls.Load()
	s.traeRetryCycleAt(context.Background(),
		time.Date(2026, 11, 14, 0, 4, 59, 0, traeLoc))
	if got := stub.claimCalls.Load(); got != before {
		t.Fatalf("排程点之前重试循环不该抢跑，claim 次数 %d → %d", before, got)
	}
}

// TestTraeRetryCycleRunsAfterScheduledFire 排程点之后，重试循环恢复正常补签。
//
// 闸门只是「排队」，不是「关掉」：当天首签撞了 9074 的号，仍然必须被重试。
func TestTraeRetryCycleRunsAfterScheduledFire(t *testing.T) {
	stub := &traeCheckinStub{
		statusBody:  `{"code":0,"enable":true,"checked_in":false}`,
		claimBodies: []string{`{"code":0,"message":"success","credits":100}`},
	}
	s, _ := newTraeS(t, stub, t.TempDir())
	s.cfg.TraeCheckHours = []int{0}
	s.noteTraeBackoff("2081842786229369")
	s.traeCheckin.update("2081842786229369", func(st *traeCheckinState) {
		st.RetryUpdatedAt = time.Now().Add(-time.Hour).Unix()
	})

	// 00:05:00 整：闸门刚打开，重试循环应接手补签。
	before := stub.claimCalls.Load()
	s.traeRetryCycleAt(context.Background(),
		time.Date(2026, 11, 14, 0, 5, 0, 0, traeLoc))
	if got := stub.claimCalls.Load(); got <= before {
		t.Fatalf("排程点之后重试循环应继续补签，claim 次数仍为 %d", before)
	}
}

// TestTraeScheduledRunIsNotSkippedByRetryLoop 端到端：排程点之前没有重试
// 抢跑时，到点那轮必须真的签到（而不是只报「跳过 N」）。
//
// 这条用例直接对应用户看到的日志形态：
//
//	共 6，成功 0，已签 0，失败 0，跳过 6   ← bug
//	共 6，成功 6，已签 0，失败 0，跳过 0   ← 期望
func TestTraeScheduledRunIsNotSkippedByRetryLoop(t *testing.T) {
	now := time.Now()
	// 与 TestTraeCheckinPopulatesExpiringCredits 同一套假上游：ent_usage 返回
	// 两个包，签到的 status/claim 也由同一个 httptest 提供。
	stub := &traeCheckinStub{
		statusBody:  `{"code":0,"enable":true,"checked_in":false}`,
		claimBodies: []string{`{"code":0,"message":"success","credits":100}`},
	}
	s, _ := newTraeS(t, stub, t.TempDir())
	// 新账号（无 9074 痕迹）→ 重试循环根本不会看它，模拟「干净的一天」。
	if traeRetryPending(s.traeState("2081842786229369"), now) {
		t.Fatal("新账号不该被重试循环盯上")
	}

	out, err := s.traeCheckAll(context.Background())
	if err != nil {
		t.Fatalf("traeCheckAll: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("outcomes = %d want 1", len(out))
	}
	if out[0].Status != CheckinOK {
		t.Fatalf("首次排程签到应成功，实际 status=%s detail=%s", out[0].Status, out[0].Detail)
	}
	if !traeCheckedInToday(s.traeState("2081842786229369"), now) {
		t.Fatal("签到成功后应记「今天已签」")
	}
}

// TestTraeFireMinuteBelowHour 触发分钟必须小于 60（防御性：常量写错会让
// time.Date 静默进位到下个小时，排程点整体漂移）。
func TestTraeFireMinuteBelowHour(t *testing.T) {
	if traeFireMinute < 0 || traeFireMinute >= 60 {
		t.Fatalf("traeFireMinute = %d 不是合法分钟", traeFireMinute)
	}
}

// TestTraeHoursDefaultIsZero scheduler.New 的默认值必须是 [0]。
//
// 两处默认（config.DefaultSchedule 与 scheduler.New）必须一致，否则直接构造
// Config 的调用方与走 config.json 的调用方会拿到不同排程——这是历史上
// 「两处漂移」的固定失败模式。
func TestTraeHoursDefaultIsZero(t *testing.T) {
	s := New(Config{})
	if len(s.cfg.TraeCheckHours) != 1 || s.cfg.TraeCheckHours[0] != 0 {
		t.Fatalf("scheduler.New 默认 trae_check_hours 应为 [0]，实际 %v", s.cfg.TraeCheckHours)
	}
}

// 编译期护栏：确保测试里用到的 trae 包常量仍存在（Realm 判定依赖它）。
var _ = trae.Realm
