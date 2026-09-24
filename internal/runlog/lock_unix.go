//go:build !windows

// ═══ 更新日志 ═══
// 2026-09-25：日志采用独立flock文件，保证交接中两个运行实例轮换时不覆盖对方日志。
package runlog

import (
	"os"
	"syscall"
)

func lock(path string) (func(), error) {
	// #nosec G304 -- sidecar lock uses the same administrator-configured log path, not a client path.
	file, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX); err != nil {
		_ = file.Close()
		return nil, err
	}
	return func() { _ = syscall.Flock(int(file.Fd()), syscall.LOCK_UN); _ = file.Close() }, nil
}
