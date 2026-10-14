// ═══ 更新日志 ═══
// 2026-09-15: 新增。排程任务自省与手动触发的单测：快照顺序与开关语义、
//   互斥占用、未知 key。

package scheduler

import (
	"strings"
	"testing"
	"time"
)

func TestTaskSnapshotOrderAndSwitches(t *testing.T) {
	s := New(Config{}) // 零值 Config = 全启用、小时回落默认
	snap := s.TaskSnapshot()
	// 2026-10-14：redeem/lottery/makeup 合并为 growth，故 11 → 9 类。
	wantKeys := []string{"checkin", "trae_check", "travel", "activity", "keepalive", "school", "cat",
		"growth", "balance"}
	if len(snap) != len(wantKeys) {
		t.Fatalf("应返回 %d 类任务，实际 %d", len(wantKeys), len(snap))
	}
	for i, k := range wantKeys {
		if snap[i].Key != k {
			t.Fatalf("第 %d 项应为 %s，实际 %s", i, k, snap[i].Key)
		}
		if snapshotIsEmpty(snap[i]) {
			t.Fatalf("%s 的名称/说明为空", k)
		}
	}
	if len(snap[0].Hours) != 2 || snap[0].Hours[0] != 9 || snap[0].Hours[1] != 21 {
		t.Fatalf("签到小时应为 [9 21]，实际 %v", snap[0].Hours)
	}
	// 启用且未跑过：有下次时点、无上次时点。
	if snap[0].NextRun == nil {
		t.Fatal("启用任务应有下次运行时间")
	}
	if snap[0].LastRun != nil {
		t.Fatal("从未运行过的任务，上次运行应为 null")
	}

	// 显式禁用后：Enabled=false 且不再给下次时点（与 nextWake 跳过零时点同口径）。
	s2 := New(Config{CatDisabled: true})
	// 按 key 取而不是写死下标：任务表增删时下标会整体后移，
	// 写死下标会让这条断言去检查另一个任务（2026-10-11 新增 trae_check 时踩到）。
	var cat TaskInfo
	for _, info := range s2.TaskSnapshot() {
		if info.Key == "cat" {
			cat = info
		}
	}
	if cat.Key != "cat" {
		t.Fatal("快照里应有 cat 任务")
	}
	if cat.Enabled {
		t.Fatal("CatDisabled=true 时 cat 应为停用")
	}
	if cat.NextRun != nil {
		t.Fatal("停用任务不应给出下次运行时间")
	}
}

func snapshotIsEmpty(info TaskInfo) bool {
	return info.Name == "" || info.Detail == ""
}

func TestBeginTaskMutualExclusion(t *testing.T) {
	s := New(Config{})
	k, ok := kindOf("checkin")
	if !ok {
		t.Fatal("checkin 应能解析为内部任务种类")
	}
	if !s.beginTask(k) {
		t.Fatal("首次占用应成功")
	}
	if s.beginTask(k) {
		t.Fatal("重复占用应失败")
	}
	// 手动触发撞上正在执行的任务：返回 ErrTaskBusy 而不是排队。
	if err := s.TriggerTask("checkin"); err != ErrTaskBusy {
		t.Fatalf("应返回 ErrTaskBusy，实际 %v", err)
	}
	// 占用期间快照应报 running。
	if !s.TaskSnapshot()[0].Running {
		t.Fatal("占用期间快照应标记 running")
	}
	s.endTask(k)
	if s.TaskSnapshot()[0].Running {
		t.Fatal("释放后不应再标记 running")
	}
	if s.TaskSnapshot()[0].LastRun == nil {
		t.Fatal("释放后应记下完成时刻")
	}
}

func TestTriggerTaskUnknownKey(t *testing.T) {
	s := New(Config{})
	if err := s.TriggerTask("nosuch"); err != ErrUnknownTask {
		t.Fatalf("未知 key 应返回 ErrUnknownTask，实际 %v", err)
	}
	if err := s.TriggerTask(""); err != ErrUnknownTask {
		t.Fatalf("空 key 应返回 ErrUnknownTask，实际 %v", err)
	}
}

func TestTaskKeyRoundTrip(t *testing.T) {
	for _, key := range []string{"checkin", "trae_check", "travel", "activity", "keepalive", "school",
		"cat", "growth", "balance"} {
		k, ok := kindOf(key)
		if !ok {
			t.Fatalf("%s 应可解析", key)
		}
		if got := k.key(); string(got) != key {
			t.Fatalf("taskKind 与 TaskKey 往返不一致：%s → %s", key, got)
		}
	}
}

func TestTaskLogRing(t *testing.T) {
	s := New(Config{})
	// 日志环是包级全局：本包其它测试（如 school_test 的假 cat 脚本）也会往里写。
	// 断言前先清空，否则用例之间产生顺序依赖——单跑过、全量跑挂。
	taskSink.mu.Lock()
	taskSink.ring = map[TaskKey][]string{}
	taskSink.prev = map[TaskKey][]string{}
	taskSink.ended = map[TaskKey]time.Time{}
	taskSink.mu.Unlock()

	// 未知 key：报错而不是空日志。
	if _, err := s.TaskLog("nosuch"); err != ErrUnknownTask {
		t.Fatalf("未知 key 应返回 ErrUnknownTask，实际 %v", err)
	}
	// 已登记但没跑过：空切片、无错误。
	lines, err := s.TaskLog("checkin")
	if err != nil {
		t.Fatalf("已知 key 不应报错：%v", err)
	}
	if len(lines) != 0 {
		t.Fatalf("未跑过应为空，实际 %d 行", len(lines))
	}

	// 模拟一次执行：begin 后写日志，应归到该任务名下。
	taskSink.begin(TaskKeyCheckin)
	taskSink.emit("第一行")
	taskSink.emit("第二行")
	taskSink.end()

	lines, _ = s.TaskLog("checkin")
	if len(lines) != 2 {
		t.Fatalf("应归集 2 行，实际 %d：%v", len(lines), lines)
	}
	// 行首带 log 包的时间戳（LstdFlags），这里只断言任务前缀与正文。
	if !strings.Contains(lines[0], "[checkin] 第一行") || !strings.Contains(lines[1], "[checkin] 第二行") {
		t.Fatalf("行内容不对：%q / %q", lines[0], lines[1])
	}
	// 不串到别的任务。
	other, _ := s.TaskLog("cat")
	if len(other) != 0 {
		t.Fatalf("不该串到 cat：%v", other)
	}

	// 环有上限：超出后只留最近的。
	taskSink.begin(TaskKeyCat)
	for i := 0; i < taskLogRing+50; i++ {
		taskSink.emit("x")
	}
	taskSink.end()
	lines, _ = s.TaskLog("cat")
	if len(lines) != taskLogRing {
		t.Fatalf("环上限应为 %d，实际 %d", taskLogRing, len(lines))
	}

	// 无任务上下文时写入被丢弃，不污染任何任务。
	taskSink.emit("飘在空中的一行")
	lines, _ = s.TaskLog("checkin")
	if len(lines) != 2 {
		t.Fatalf("无上下文写入不该进任何任务：%v", lines)
	}
	// 重新 begin：当前环换成新一轮，但**上一轮的行要留得住**——
	// 面板「最近一次日志」正是靠它（2026-10-11 用户反馈看不到最近一次）。
	taskSink.begin(TaskKeyCheckin)
	taskSink.emit("新一轮")
	// 运行中读：给的是当前这一轮，且带 running 标记。
	res, _ := s.TaskLogDetail("checkin")
	if !res.Running {
		t.Fatal("begin 之后 running 应为 true")
	}
	if len(res.Lines) != 1 || !strings.Contains(res.Lines[0], "新一轮") {
		t.Fatalf("运行中应给当前这一轮：%v", res.Lines)
	}
	taskSink.end()
	// 结束后读：仍是这一轮（它现在成了「最近一次」）。
	res, _ = s.TaskLogDetail("checkin")
	if res.Running {
		t.Fatal("end 之后 running 应为 false")
	}
	if len(res.Lines) != 1 || !strings.Contains(res.Lines[0], "新一轮") {
		t.Fatalf("结束后应给最近一轮：%v", res.Lines)
	}
	// 再开一轮：上一轮（新一轮那次的 1 行）必须仍可读，直到这轮也结束。
	taskSink.begin(TaskKeyCheckin)
	taskSink.emit("第三轮")
	res, _ = s.TaskLogDetail("checkin")
	if !res.Running || len(res.Lines) != 1 || !strings.Contains(res.Lines[0], "第三轮") {
		t.Fatalf("运行中应给第三轮：running=%v lines=%v", res.Running, res.Lines)
	}
	taskSink.end()
	res, _ = s.TaskLogDetail("checkin")
	if len(res.Lines) != 1 || !strings.Contains(res.Lines[0], "第三轮") {
		t.Fatalf("应给第三轮：%v", res.Lines)
	}
	// markEnded 记下完成时刻。
	taskSink.markEnded(TaskKeyCheckin)
	res, _ = s.TaskLogDetail("checkin")
	if res.EndedAt.IsZero() {
		t.Fatal("markEnded 之后 ended_at 不该为零值")
	}
}

// TestNewTasksGrowth 成长中心任务注册与开关（2026-10-14：原 redeem/lottery/makeup
// 三条合并为 growth 一条）。
func TestNewTasksGrowth(t *testing.T) {
	s := New(Config{})
	byKey := map[string]TaskInfo{}
	for _, ti := range s.TaskSnapshot() {
		byKey[ti.Key] = ti
	}
	ti, ok := byKey["growth"]
	if !ok {
		t.Fatalf("快照缺少任务 growth")
	}
	if !ti.Enabled {
		t.Fatalf("growth 零值配置下应默认启用")
	}
	if ti.NextRun == nil {
		t.Fatalf("growth 启用时应有下次运行时间")
	}
	if ti.Name == "" || ti.Detail == "" {
		t.Fatalf("growth 名称/说明为空")
	}
	// 默认小时 [9,21]（早晚各一轮完整七段）。
	if len(ti.Hours) != 2 || ti.Hours[0] != 9 || ti.Hours[1] != 21 {
		t.Fatalf("growth 默认小时=%v want [9 21]", ti.Hours)
	}
	// 只服务 CN（global 已被排除）。
	if len(ti.Realms) != 1 || ti.Realms[0] != "cn" {
		t.Fatalf("growth realms=%v want [cn]", ti.Realms)
	}
	// 旧的三个 key 不该再出现（合并后只留 growth）。
	for _, gone := range []string{"redeem", "lottery", "makeup"} {
		if _, still := byKey[gone]; still {
			t.Fatalf("合并后不该再有 %s 任务", gone)
		}
	}

	// 显式禁用后：停用 + 不再给下次时点。
	s2 := New(Config{GrowthDisabled: true})
	for _, ti := range s2.TaskSnapshot() {
		if ti.Key != "growth" {
			continue
		}
		if ti.Enabled {
			t.Fatalf("growth 显式禁用后仍报启用")
		}
		if ti.NextRun != nil {
			t.Fatalf("growth 停用后不应给出下次运行时间")
		}
	}

	// 未知 key 仍报错（改任务种类不能把 kindOf 的兜底弄坏）。
	if _, err := s2.TaskLog("nosuchtask"); err != ErrUnknownTask {
		t.Fatalf("未知 key 应报 ErrUnknownTask，实际 %v", err)
	}
}

// TestTaskRealmsExcludeGlobalForNonBalance 国际版不参与任何任务（2026-10-14 用户要求）：
// 除 checkin / balance（只读余额）外，其余任务的 realms 不该含 global。
func TestTaskRealmsExcludeGlobalForNonBalance(t *testing.T) {
	s := New(Config{})
	for _, ti := range s.TaskSnapshot() {
		hasGlobal := false
		for _, r := range ti.Realms {
			if r == "global" {
				hasGlobal = true
			}
		}
		switch ti.Key {
		case "checkin", "balance":
			// 这两个含 global 是刻意的：global 号仍查余额（纯读，驱动快过期优先消耗）。
			if !hasGlobal {
				t.Errorf("%s 应含 global（只读余额）", ti.Key)
			}
		default:
			if hasGlobal {
				t.Errorf("%s 不该含 global（国际版不做任务），realms=%v", ti.Key, ti.Realms)
			}
		}
	}
}
