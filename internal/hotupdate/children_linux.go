// ═══ 更新日志 ═══
// 2026-09-25：候选交接失败时回收其私有面板及辅助程序，以进程启动标识避免PID重用误杀。
package hotupdate

import (
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"
)

type childIdentity struct {
	pid   int
	start string
}

func configureCandidate(cmd *exec.Cmd) { cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true} }

func candidateSession(pid int) (start, session string) {
	raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return "", ""
	}
	end := strings.LastIndex(string(raw), ") ")
	if end < 0 {
		return "", ""
	}
	fields := strings.Fields(string(raw)[end+2:])
	if len(fields) < 20 {
		return "", ""
	}
	return fields[19], fields[3]
}

func processStart(pid int) string {
	raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return ""
	}
	end := strings.LastIndex(string(raw), ") ")
	if end < 0 {
		return ""
	}
	fields := strings.Fields(string(raw)[end+2:])
	if len(fields) < 20 {
		return ""
	}
	return fields[19]
}

func candidateChildren(pid int) []childIdentity {
	var result []childIdentity
	seen := map[int]bool{}
	// A candidate has its own Linux session. Descendants keep that identity
	// after a leader crash and PID1 adoption, unlike parent/child relationships.
	processes, _ := os.ReadDir("/proc")
	for _, process := range processes {
		child, err := strconv.Atoi(process.Name())
		if err != nil || child == pid {
			continue
		}
		start, session := candidateSession(child)
		if start != "" && session == strconv.Itoa(pid) {
			result = append(result, childIdentity{child, start})
			seen[child] = true
		}
	}
	var walk func(int)
	walk = func(parent int) {
		if len(seen) >= 512 || seen[parent] {
			return
		}
		seen[parent] = true
		tasks, _ := os.ReadDir(fmt.Sprintf("/proc/%d/task", parent))
		var childFields []string
		for _, task := range tasks {
			if _, err := strconv.Atoi(task.Name()); err != nil {
				continue
			}
			raw, _ := os.ReadFile(fmt.Sprintf("/proc/%d/task/%s/children", parent, task.Name()))
			childFields = append(childFields, strings.Fields(string(raw))...)
		}
		for _, field := range childFields {
			child, err := strconv.Atoi(field)
			if err != nil || child <= 0 || seen[child] {
				continue
			}
			start := processStart(child)
			if start == "" {
				continue
			}
			result = append(result, childIdentity{child, start})
			walk(child)
		}
	}
	walk(pid)
	return result
}

func stopCandidateChildren(children []childIdentity) {
	signal := func(sig syscall.Signal) {
		for i := len(children) - 1; i >= 0; i-- {
			child := children[i]
			if processStart(child.pid) == child.start {
				_ = syscall.Kill(child.pid, sig)
			}
		}
	}
	signal(syscall.SIGTERM)
	if len(children) > 0 {
		time.Sleep(100 * time.Millisecond)
		signal(syscall.SIGKILL)
	}
}
