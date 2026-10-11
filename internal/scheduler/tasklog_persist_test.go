// ═══ 更新日志 ═══
// 2026-10-11：任务日志落盘。日志环原本是纯内存的，容器一重启「最近一次日志」
//
//	就没了——用户看到「明明运行过」却是空页（部署重启 8 分钟后实测）。
package scheduler

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// resetSink 把包级日志环恢复到干净状态（含落盘目录）。
func resetSink(t *testing.T, dir string) {
	t.Helper()
	taskSink.mu.Lock()
	taskSink.ring = map[TaskKey][]string{}
	taskSink.prev = map[TaskKey][]string{}
	taskSink.ended = map[TaskKey]time.Time{}
	taskSink.dir = dir
	taskSink.mu.Unlock()
}

// TestTaskLogSurvivesRestart 任务日志必须能跨进程重启读到。
//
// 这是用户实测的缺口：「明明运行过」但面板显示空页。根因是日志环纯内存，
// 而每次部署都会重启容器——2026-10-11 那次重启后 8 分钟，22:44 的 trae_check
// 运行记录已经查不到。
func TestTaskLogSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	resetSink(t, dir)

	// 跑一轮并结束（模拟真实执行）。
	taskSink.begin(TaskKeyTraeCheck)
	taskSink.emit("第一行输出")
	taskSink.emit("checkin done: ok=4")
	taskSink.end()
	taskSink.markEnded(TaskKeyTraeCheck)

	// 落盘文件应存在。
	path := taskLogFile(dir, TaskKeyTraeCheck)
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("应写出落盘文件 %s：%v", path, err)
	}

	// 模拟重启：清空全部内存态，再从磁盘 load。
	taskSink.mu.Lock()
	taskSink.ring = map[TaskKey][]string{}
	taskSink.prev = map[TaskKey][]string{}
	taskSink.ended = map[TaskKey]time.Time{}
	taskSink.mu.Unlock()
	taskSink.load()

	s := New(Config{})
	res, err := s.TaskLogDetail("trae_check")
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Lines) != 2 {
		t.Fatalf("重启后应读回 2 行，实际 %d：%v", len(res.Lines), res.Lines)
	}
	if !strings.Contains(res.Lines[1], "checkin done") {
		t.Fatalf("内容不符：%v", res.Lines)
	}
	if res.Running {
		t.Fatal("从磁盘读回的不是「运行中」")
	}
	if res.EndedAt.IsZero() {
		t.Fatal("重启后应读回完成时刻")
	}
}

// TestTaskLogDiskNotWrittenForOtherTasks 落盘按任务分文件，互不干扰。
func TestTaskLogDiskNotWrittenForOtherTasks(t *testing.T) {
	dir := t.TempDir()
	resetSink(t, dir)
	taskSink.begin(TaskKeyCheckin)
	taskSink.emit("checkin 的内容")
	taskSink.end()

	if _, err := os.Stat(taskLogFile(dir, TaskKeyTraeCheck)); err == nil {
		t.Fatal("没跑过的任务不该有落盘文件")
	}
	if _, err := os.Stat(taskLogFile(dir, TaskKeyCheckin)); err != nil {
		t.Fatalf("跑过的任务应有落盘文件：%v", err)
	}
}

// TestTaskLogRunsWithoutDiskDir 没配落盘目录时纯内存工作（不 panic、不报错）。
//
// SetTaskLogDir 在目录不可用时会把 dir 清空并退回内存——日志是观测手段，
// 不能因为落不了盘而影响任务执行。
func TestTaskLogRunsWithoutDiskDir(t *testing.T) {
	resetSink(t, "")
	taskSink.begin(TaskKeyCheckin)
	taskSink.emit("内存态的一行")
	taskSink.end()
	s := New(Config{})
	res, err := s.TaskLogDetail("checkin")
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Lines) != 1 {
		t.Fatalf("纯内存也应能读回：%v", res.Lines)
	}
}

// TestSetTaskLogDirRejectsUnwritableDir 目录建不出来时退回内存，不拦启动。
func TestSetTaskLogDirRejectsUnwritableDir(t *testing.T) {
	// 用一个「父路径是文件」的目录，MkdirAll 必然失败。
	base := t.TempDir()
	blocker := filepath.Join(base, "not-a-dir")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	SetTaskLogDir(filepath.Join(blocker, "task-logs"))
	taskSink.mu.Lock()
	got := taskSink.dir
	taskSink.mu.Unlock()
	if got != "" {
		t.Fatalf("目录不可用时应退回纯内存（dir=\"\"），实际 %q", got)
	}
	// 复位，免得影响其它用例。
	resetSink(t, "")
}

// TestLastRunRestoredFromDisk 重启后任务页的「上次运行」也要能恢复。
//
// 与日志同源：两者都取自「最后一次执行的完成时刻」。只恢复日志、不恢复时刻，
// 会出现「日志有内容、上次运行却空白」的自相矛盾。
func TestLastRunRestoredFromDisk(t *testing.T) {
	dir := t.TempDir()
	resetSink(t, dir)

	taskSink.begin(TaskKeyTraeCheck)
	taskSink.emit("一轮输出")
	taskSink.end()
	taskSink.markEnded(TaskKeyTraeCheck)

	// 模拟重启：清内存后 load。
	taskSink.mu.Lock()
	taskSink.prev = map[TaskKey][]string{}
	taskSink.ended = map[TaskKey]time.Time{}
	taskSink.mu.Unlock()
	taskSink.load()

	s := New(Config{})
	for _, info := range s.TaskSnapshot() {
		if info.Key != "trae_check" {
			continue
		}
		if info.LastRun == nil {
			t.Fatal("重启后「上次运行」应从落盘恢复，实际为 nil")
		}
		if time.Since(*info.LastRun) > time.Hour {
			t.Fatalf("恢复的时刻不合理：%v", *info.LastRun)
		}
		return
	}
	t.Fatal("快照里应有 trae_check")
}

// TestPersistDoesNotDeadlockOnFailure 落盘失败时不得死锁。
//
// 关键约束：标准 logger 被 ensureLogTee 分流回 taskSink.Write，那条路径要拿
// taskSink.mu。所以**持锁时不能打日志**——磁盘满/目录只读会让 persistLocked
// 走进错误分支，一 log 就自死锁，整个调度器卡死。
//
// 2026-10-11：这个 bug 由 TestBeginTaskMutualExclusion 挂起（600s 超时）暴露。
// 这里直接构造「落盘必失败」的场景复现。
func TestPersistDoesNotDeadlockOnFailure(t *testing.T) {
	// 目录指向一个「父路径是文件」的位置：WriteFile 必然失败。
	base := t.TempDir()
	blocker := filepath.Join(base, "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	resetSink(t, filepath.Join(blocker, "task-logs"))

	done := make(chan struct{})
	go func() {
		defer close(done)
		// begin/emit/end/markEnded 全走一遍：任何一处持锁打日志都会卡住。
		taskSink.begin(TaskKeyCheckin)
		taskSink.emit("内容")
		taskSink.end()
		taskSink.markEnded(TaskKeyCheckin)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("落盘失败路径死锁：持 taskSink.mu 时调用了 log（日志被 tee 回同一把锁）")
	}
	resetSink(t, "")
}
