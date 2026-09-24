//go:build !linux

// ═══ 更新日志 ═══
// 2026-09-25：非Linux热更新不运行内嵌面板进程树，保留原有主候选回收。
package hotupdate

import "os/exec"

type childIdentity struct{}

func configureCandidate(_ *exec.Cmd) {}

func candidateChildren(_ int) []childIdentity { return nil }
func stopCandidateChildren(_ []childIdentity) {}
