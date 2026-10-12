//go:build !windows

// ═══ 更新日志 ═══
// 2026-10-13：初版——Linux/Android/macOS 上辅助程序不带后缀。
package panel

// platformExecutableName 按平台给辅助程序名补后缀。
//
// 非 Windows 平台（含 Android）的二进制没有扩展名。这里不看文件是否存在：
// 那种判据会让「恰好存在一个同名 .exe」的目录误判，而辅助程序名是固定集合。
func platformExecutableName(name string) string { return name }
