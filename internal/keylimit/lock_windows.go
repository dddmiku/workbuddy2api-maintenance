//go:build windows

// ═══ 更新日志 ═══
// 2026-09-25：Windows 文件区域锁保持相同的非阻塞及进程退出释放语义。
package keylimit

import (
	"os"
	"syscall"
	"unsafe"
)

var kernel = syscall.NewLazyDLL("kernel32.dll")
var lockProc = kernel.NewProc("LockFileEx")
var unlockProc = kernel.NewProc("UnlockFileEx")

func tryFileLock(path string) (*os.File, bool, error) {
	// #nosec G304 -- fixed paths under the administrator-owned state directory.
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, false, err
	}
	operation := &syscall.Overlapped{}
	// #nosec G103 -- synchronous Windows syscall with a live OVERLAPPED value.
	result, _, failure := lockProc.Call(f.Fd(), 3, 0, 1, 0, uintptr(unsafe.Pointer(operation)))
	if result == 0 {
		_ = f.Close()
		if failure == syscall.Errno(33) {
			return nil, false, nil
		}
		return nil, false, failure
	}
	return f, true, nil
}

func unlockFile(f *os.File) {
	if f != nil {
		operation := &syscall.Overlapped{}
		// #nosec G103 -- synchronous Windows syscall with a live OVERLAPPED value.
		_, _, _ = unlockProc.Call(f.Fd(), 0, 1, 0, uintptr(unsafe.Pointer(operation)))
		_ = f.Close()
	}
}
