//go:build !windows

// ═══ 更新日志 ═══
// 2026-10-13：初版——从 FileInfo 取属主 uid/gid（重写配置文件时用来恢复属主）。
package panel

import (
	"io/fs"
	"syscall"
)

// fileOwner 从 FileInfo 取出 uid/gid；取不到时 ok=false（调用方跳过 chown）。
func fileOwner(info fs.FileInfo) (int, int, bool) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, 0, false
	}
	return int(stat.Uid), int(stat.Gid), true
}
