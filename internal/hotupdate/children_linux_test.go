// ═══ 更新日志 ═══
// 2026-09-25：候选网关启动即崩溃后，仍按独立会话找到并清理已被收养的辅助进程。
package hotupdate

import (
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
)

func TestCandidateSessionFindsOrphanedHelper(t *testing.T) {
	cmd := exec.Command("sh", "-c", "sleep 30 >/dev/null 2>&1 & echo $!")
	configureCandidate(cmd)
	raw, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
	children := candidateChildren(cmd.Process.Pid)
	found := false
	for _, child := range children {
		if child.pid == pid {
			found = true
		}
	}
	if !found {
		t.Fatalf("lost orphan helper %d from candidate session %d", pid, cmd.Process.Pid)
	}
	stopCandidateChildren(children)
}
