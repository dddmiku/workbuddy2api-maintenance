//go:build !windows

// ═══ 更新日志 ═══
// 2026-10-13：初版——Unix 上支持运行中交接监听套接字。
package hotupdate

// HandoverSupported 本平台能否在运行中把监听套接字交给新进程。
//
// Unix 用 dup + FD 继承（ExtraFiles）实现，进程不中断、在途请求跑完再退出。
// Windows 上不可用，见 handover_unsupported.go。
const HandoverSupported = true
