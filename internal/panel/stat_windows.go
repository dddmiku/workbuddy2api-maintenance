//go:build windows

// ═══ 更新日志 ═══
// 2026-10-13：初版——Windows 没有 uid/gid 语义，恒返回「取不到」。
package panel

import "io/fs"

// fileOwner 在 Windows 上恒返回 ok=false：ACL 模型里没有「属主 uid」这个概念，
// 重写配置文件时无需（也无法）恢复属主。
func fileOwner(fs.FileInfo) (int, int, bool) { return 0, 0, false }
