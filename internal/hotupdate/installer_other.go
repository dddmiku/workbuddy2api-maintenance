//go:build !android

// ═══ 更新日志 ═══
// 2026-10-13：初版——非安卓平台不需要安装器钩子（走进程交接）。
package hotupdate

import "context"

// StageInstallerForAndroid 在非安卓平台上返回 nil，表示走既有的进程交接路径。
//
// 桌面与 Linux 的更新件是可直接执行的文件，交接后由新实例接管监听套接字；
// 只有安卓需要「交给系统安装器」这一条绕路。
func StageInstallerForAndroid(string) func(context.Context, string) (string, error) {
	return nil
}
