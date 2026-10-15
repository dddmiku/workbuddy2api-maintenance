//go:build !windows

// ═══ 更新日志 ═══
// 2026-10-15：初版——识别「跨文件系统重命名」（EXDEV）。
//
// 容器里 auths 与 auths-trash 是两个独立的 bind mount，rename(2) 跨挂载点必报
// EXDEV（"invalid cross-device link"）。调用方据此回落到「复制 + 删源」。
package panel

import (
	"errors"
	"syscall"
)

// isCrossDevice 报告错误是否为跨文件系统重命名（EXDEV）。
func isCrossDevice(err error) bool {
	return errors.Is(err, syscall.EXDEV)
}
