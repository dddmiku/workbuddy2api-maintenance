//go:build !windows

// ═══ 更新日志 ═══
// 2026-09-25：使用非阻塞旁路锁，共享热重载期间的密钥额度与在途租约。
package keylimit

import (
	"errors"
	"os"
	"syscall"
)

func tryFileLock(path string) (*os.File, bool, error) {
	// #nosec G304 -- fixed paths under the administrator-owned state directory.
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, false, err
	}
	err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if err != nil {
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return nil, false, nil
		}
		return nil, false, err
	}
	return f, true, nil
}

func unlockFile(f *os.File) {
	if f != nil {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}
}
