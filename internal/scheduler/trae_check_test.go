// ═══ 更新日志 ═══
// 2026-10-11：Trae 签到拆成独立任务（独立站点、独立排程、独立开关），
// 并锁定「9074 限流当天不再重试」这一核心修复。
package scheduler

import (
	"context"
	"strings"
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
		GrowthDisabled:    true,
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

// TestTraeDeviceGenerationTracksRotation 9074 后设备号代数递增并被记住。
//
// 9074 是**设备维度**限流：换新派生的设备号即可立刻签上（实测同一账号同一 JWT，
// 第 0 代 9074、第 1 代 success）。我最初误判成账号维度、做成「当天放弃」，
// 实测四个账号换设备号后当天全部签上——所以代数必须记住，后续轮次直接用新的，
// 免得每次都被旧的限流设备号挡一道。
func TestTraeDeviceGenerationTracksRotation(t *testing.T) {
	s := New(Config{})
	if got := s.traeDeviceGeneration("u1"); got != 0 {
		t.Fatalf("初始代数应为 0，实际 %d", got)
	}
	s.noteTraeDeviceGeneration("u1", 1)
	if got := s.traeDeviceGeneration("u1"); got != 1 {
		t.Fatalf("记录后应为 1，实际 %d", got)
	}
	// 别的账号不受影响。
	if got := s.traeDeviceGeneration("u2"); got != 0 {
		t.Fatalf("代数按账号维度，不该波及其它号，实际 %d", got)
	}
	// 再递增。
	s.noteTraeDeviceGeneration("u1", 2)
	if got := s.traeDeviceGeneration("u1"); got != 2 {
		t.Fatalf("应可继续递增到 2，实际 %d", got)
	}
}

// TestTaskLogKeepsLastRunWhileIdle 任务空闲时读日志给的是**上一次**那一轮。
//
// 2026-10-11 用户要求「能看到最近一次的日志」。原先 begin() 直接清空环，
// 于是新一轮一开始，上一轮的日志就没了——点开日志正好赶上新一轮启动时，
// 只能看到半截新日志，看不到上次跑成什么样。
func TestTaskLogKeepsLastRunWhileIdle(t *testing.T) {
	s := New(Config{})
	taskSink.mu.Lock()
	taskSink.ring = map[TaskKey][]string{}
	taskSink.prev = map[TaskKey][]string{}
	taskSink.ended = map[TaskKey]time.Time{}
	taskSink.mu.Unlock()

	// 第一轮跑完。
	taskSink.begin(TaskKeyTraeCheck)
	taskSink.emit("第一轮的内容")
	taskSink.end()

	// 空闲时读：应给第一轮。
	res, err := s.TaskLogDetail("trae_check")
	if err != nil {
		t.Fatal(err)
	}
	if res.Running {
		t.Fatal("空闲时 running 应为 false")
	}
	if len(res.Lines) != 1 || !strings.Contains(res.Lines[0], "第一轮的内容") {
		t.Fatalf("空闲时应给上一次那一轮：%v", res.Lines)
	}

	// 第二轮开始（此时第一轮已被顶掉，但第二轮是「当前」，仍能读到）。
	taskSink.begin(TaskKeyTraeCheck)
	taskSink.emit("第二轮的内容")
	res, _ = s.TaskLogDetail("trae_check")
	if !res.Running {
		t.Fatal("begin 之后 running 应为 true")
	}
	if len(res.Lines) != 1 || !strings.Contains(res.Lines[0], "第二轮的内容") {
		t.Fatalf("运行中应给当前这一轮：%v", res.Lines)
	}
	taskSink.end()
	// 跑完后：最近一次 = 第二轮。
	res, _ = s.TaskLogDetail("trae_check")
	if len(res.Lines) != 1 || !strings.Contains(res.Lines[0], "第二轮的内容") {
		t.Fatalf("跑完后最近一次应为第二轮：%v", res.Lines)
	}
	// 未知任务仍报错。
	if _, err := s.TaskLogDetail("nosuch"); err != ErrUnknownTask {
		t.Fatalf("未知 key 应报 ErrUnknownTask，实际 %v", err)
	}
}

// TestTraeBackoffEscalatesAndCaps 9074 的退避要有界：60s 起、翻倍、封顶 8 分钟。
//
// 轮换设备号能绕过大多数 9074，但绕不过上游高峰期（实测 2026-10-12 00:00 跨天
// 边界，两个号 gen0→gen1 都吃 9074）。这层退避就是为那种情况兜底：短时间猛重试
// 只会让「人多」的窗口更长，所以要退，但退太久会白白错过当天可签的时机。
func TestTraeBackoffEscalatesAndCaps(t *testing.T) {
	s := New(Config{})
	if _, waiting := s.traeInBackoff("u1"); waiting {
		t.Fatal("初始不该在退避中")
	}
	// 前四次：60s / 120s / 240s / 480s。
	want := []time.Duration{60 * time.Second, 120 * time.Second, 240 * time.Second, 480 * time.Second}
	for i, w := range want {
		if got := s.noteTraeBackoff("u1"); got != w {
			t.Fatalf("第 %d 次退避应为 %s，实际 %s", i+1, w, got)
		}
	}
	// 第五次起封顶在 8 分钟，不再翻倍。
	for i := 0; i < 3; i++ {
		if got := s.noteTraeBackoff("u1"); got != 8*time.Minute {
			t.Fatalf("封顶后应为 8m，实际 %s", got)
		}
	}
	// 退避窗口内应被判定为等待中。
	if d, waiting := s.traeInBackoff("u1"); !waiting || d <= 0 {
		t.Fatalf("应处于退避中，实际 waiting=%v d=%s", waiting, d)
	}
	// 别的账号不受影响。
	if _, waiting := s.traeInBackoff("u2"); waiting {
		t.Fatal("退避按账号维度，不该波及其它号")
	}
	// 成功后清零，下次从基数重来。
	s.clearTraeBackoff("u1")
	if _, waiting := s.traeInBackoff("u1"); waiting {
		t.Fatal("清退避后不该再判为等待中")
	}
	if got := s.noteTraeBackoff("u1"); got != 60*time.Second {
		t.Fatalf("清零后应回到基数 60s，实际 %s", got)
	}
}

// TestTraeClaimGapThrottlesGlobally claim 之间有全局最小间隔。
//
// 9074 是上游容量信号，会随负载开合。连打不会「造成」9074，但会把所有账号的
// 尝试全烧在**同一个关闭窗口**里：实测 2026-10-12 01:39 四个号 5.4 秒跑完、
// 8 次调用全 9074；01:41 把同样的调用分散在 90 秒里则三次全成功。
// 分散开的收益是各自撞不同的窗口。
func TestTraeClaimGapThrottlesGlobally(t *testing.T) {
	s := New(Config{})
	// 从未 claim 过：不等待。
	if err := s.traeWaitClaimGap(context.Background()); err != nil {
		t.Fatalf("首次不该等待：%v", err)
	}
	s.traeNoteClaim()
	// 刚记过：应等到满 traeClaimGap。用 ctx 取消来验证「确实在等」而不真等 60 秒。
	ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
	defer cancel()
	err := s.traeWaitClaimGap(ctx)
	if err == nil {
		t.Fatal("刚 claim 过就该等待；期望 ctx 超时")
	}
	// 把上次时刻推到过去：不再等待。
	s.mu.Lock()
	s.traeLastClaim = time.Now().Add(-traeClaimGap - time.Second)
	s.mu.Unlock()
	if err := s.traeWaitClaimGap(context.Background()); err != nil {
		t.Fatalf("已过间隔不该等待：%v", err)
	}
}

// TestTraeClaimGapIsNotPerAccount 节流是**全局**的，不分账号。
//
// 按账号节流挡不住「四个号各打一次、彼此挨着」——那正是线上 5.4 秒跑完的场景。
func TestTraeClaimGapIsNotPerAccount(t *testing.T) {
	s := New(Config{})
	s.traeNoteClaim()
	// 换一个「账号」视角：仍应受同一次节流约束（函数签名里根本没有 uid）。
	ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
	defer cancel()
	if err := s.traeWaitClaimGap(ctx); err == nil {
		t.Fatal("节流必须全局生效，不该因为换账号就放行")
	}
}
