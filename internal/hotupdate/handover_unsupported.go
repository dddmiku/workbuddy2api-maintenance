//go:build windows

// ═══ 更新日志 ═══
// 2026-10-13：初版——Windows 上无法交接监听套接字，改用「下次启动生效」。
package hotupdate

// HandoverSupported 本平台能否在运行中把监听套接字交给新进程。
//
// Windows 上做不到：复制监听 FD 需要 Unix 的 dup + FD 继承，Windows 没有等价机制
// （见 fd_windows.go）。所以那里改用「下载并落盘 + 写 current 指针，下次启动生效」。
//
// 这不是降级取舍，而是平台事实：与其让「加号/更新」在 Windows 上永远报一个
// 用户看不懂的 dup listener 错误，不如明确告知「重启后生效」。
const HandoverSupported = false
