//go:build !linux

// ═══ 更新日志 ═══
// 2026-09-25：非Linux开发环境提供面板子进程生命周期适配。
package panelruntime

import "os/exec"

func configureProcess(_ *exec.Cmd) {}
func stopProcess(cmd *exec.Cmd) {
	if cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
}
func killProcess(cmd *exec.Cmd) { stopProcess(cmd) }
