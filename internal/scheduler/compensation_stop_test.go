// compensation_stop_test.go — 补偿积分任务与任务停止能力的契约测试。
//
// 背景（2026-10-16 用户需求）：
//  1. 「这里可以领 500 积分」→ 做成一次性任务放进任务面板；
//  2. 「任务面板的任务加一个停止按钮」。
package scheduler

import (
	"context"
	"testing"
	"time"
)

// TestCompensationIsOneShotTask 补偿任务必须登记为**一次性任务**。
//
// 判据是「没有排程小时」：它不该进 nextWake 候选。补偿是限时活动
// （实测 end_time 2026-10-22 23:59:59），做成定时任务只会在活动结束后每天空跑。
func TestCompensationIsOneShotTask(t *testing.T) {
	s := New(Config{})
	snap := s.TaskSnapshot()
	var found *TaskInfo
	for index := range snap {
		if snap[index].Key == string(TaskKeyCompensation) {
			found = &snap[index]
			break
		}
	}
	if found == nil {
		t.Fatal("补偿任务未登记进快照（面板看不到它）")
	}
	if len(found.Hours) != 0 {
		t.Errorf("一次性任务不该有排程小时，实际 %v", found.Hours)
	}
	if found.NextRun != nil {
		t.Errorf("一次性任务不该有下次运行时间，实际 %v", found.NextRun)
	}
	if found.Name == "" || found.Detail == "" {
		t.Error("名称/说明为空，面板会显示成空白卡片")
	}
	// 它只面向 CN 站：global/trae 打该端点会拿到 401（实测 36 账号里只有 6 个 CN 可用）。
	if len(found.Realms) != 1 || found.Realms[0] != "cn" {
		t.Errorf("补偿任务应只作用于 cn，实际 %v", found.Realms)
	}
}

// TestCompensationNotScheduled 补偿任务不得进入定时唤醒候选。
func TestCompensationNotScheduled(t *testing.T) {
	s := New(Config{})
	// 取一个很长的窗口，覆盖所有可能的小时点。
	now := time.Date(2026, 10, 16, 0, 0, 0, 0, time.UTC)
	for hour := 0; hour < 24; hour++ {
		at := now.Add(time.Duration(hour) * time.Hour)
		_, kinds := s.nextWake(at)
		for _, k := range kinds {
			if k == taskCompensation {
				t.Fatalf("补偿任务不该被定时唤醒（%v）", at)
			}
		}
	}
}

// TestStopTaskCancelsOnlyThatTask 停止只取消目标任务，不影响其它任务与调度器。
//
// 这是「停止按钮」的核心语义：此前手动触发直接用 s.lifecycle（调度器全局 ctx），
// 取消它等于关停整个排程器——所有任务和定时点一起停。现在每个任务有自己的 ctx。
func TestStopTaskCancelsOnlyThatTask(t *testing.T) {
	s := New(Config{})

	// 占住两个任务。
	if !s.beginTask(taskCheckin) {
		t.Fatal("checkin 应可占住")
	}
	defer s.endTask(taskCheckin)
	if !s.beginTask(taskBalance) {
		t.Fatal("balance 应可占住")
	}
	defer s.endTask(taskBalance)

	checkinCtx, checkinCancel := s.TaskContext(taskCheckin)
	defer checkinCancel()
	balanceCtx, balanceCancel := s.TaskContext(taskBalance)
	defer balanceCancel()

	// 停 checkin。
	if !s.StopTask(string(TaskKeyCheckin)) {
		t.Fatal("StopTask 应报告取消了 checkin")
	}
	select {
	case <-checkinCtx.Done():
	case <-time.After(time.Second):
		t.Fatal("checkin 的 ctx 未被取消")
	}
	// balance 必须还活着——这是「只停一个」的判据。
	select {
	case <-balanceCtx.Done():
		t.Fatal("balance 被误取消（停止不该影响其它任务）")
	default:
	}
	// 调度器本身也必须还活着。
	if err := s.lifecycle.Err(); err != nil {
		t.Fatalf("调度器 lifecycle 被误取消：%v", err)
	}
}

// TestStopTaskNotRunning 没在跑时返回 false（不是错误）。
func TestStopTaskNotRunning(t *testing.T) {
	s := New(Config{})
	if s.StopTask(string(TaskKeyCheckin)) {
		t.Error("任务没在跑时应返回 false（面板可能连点，报错只会让人困惑）")
	}
	if s.StopTask("no_such_task") {
		t.Error("未知任务应返回 false")
	}
}

// TestTaskContextInheritsLifecycle 任务 ctx 必须继承调度器生命周期。
//
// 关停语义不能因为「可单独取消」而丢失：调度器退出时子任务照常被取消。
func TestTaskContextInheritsLifecycle(t *testing.T) {
	s := New(Config{})
	if !s.beginTask(taskCheckin) {
		t.Fatal("checkin 应可占住")
	}
	defer s.endTask(taskCheckin)
	ctx, cancel := s.TaskContext(taskCheckin)
	defer cancel()

	s.cancel() // 模拟调度器关停
	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("调度器关停后任务 ctx 未被取消（关停语义丢失）")
	}
}

// TestEndTaskClearsCancel 任务结束后取消函数要被清掉。
//
// 不清的后果：下一轮 beginTask 之前 StopTask 会拿到**上一轮**的 cancel，
// 报告「已取消」而实际什么也没停。
func TestEndTaskClearsCancel(t *testing.T) {
	s := New(Config{})
	if !s.beginTask(taskCheckin) {
		t.Fatal("checkin 应可占住")
	}
	_, cancel := s.TaskContext(taskCheckin)
	defer cancel()
	if !s.StopTask(string(TaskKeyCheckin)) {
		t.Fatal("运行中应可取消")
	}
	s.endTask(taskCheckin)
	if s.StopTask(string(TaskKeyCheckin)) {
		t.Error("任务已结束，StopTask 不该再报告取消（说明 cancel 没清）")
	}
}

// TestZeroValueSchedulerDoesNotPanic 零值 Scheduler 不 panic。
//
// 测试与部分调用方直接构造 &Scheduler{}（绕过 New），此时 lifecycle 与
// taskCancel 都是 nil。实测踩到过 panic: assignment to entry in nil map。
func TestZeroValueSchedulerDoesNotPanic(t *testing.T) {
	s := &Scheduler{}
	ctx, cancel := s.TaskContext(taskCheckin)
	defer cancel()
	if ctx == nil {
		t.Fatal("零值 Scheduler 应返回可用的 ctx")
	}
	if ctx.Err() != nil {
		t.Fatal("零值 Scheduler 的 ctx 不该已被取消")
	}
	// 取消它不该 panic。
	cancel()
	if ctx.Err() == nil {
		t.Fatal("cancel 未生效")
	}
}

var _ = context.Background
