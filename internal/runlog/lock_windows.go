//go:build windows

// ═══ 更新日志 ═══
// 2026-09-25：Windows开发验证使用文件区域锁，与Linux保持跨进程轮换互斥。
package runlog

import (
	"os"
	"syscall"
	"unsafe"
)

var kernel = syscall.NewLazyDLL("kernel32.dll")
var lockFile = kernel.NewProc("LockFileEx")
var unlockFile = kernel.NewProc("UnlockFileEx")

func lock(path string) (func(), error) {
	// #nosec G304 -- sidecar lock uses the same administrator-configured log path, not a client path.
	file, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	operation := &syscall.Overlapped{}
	// #nosec G103 -- OVERLAPPED remains alive until the unlock closure finishes.
	result, _, failure := lockFile.Call(file.Fd(), 2, 0, 1, 0, uintptr(unsafe.Pointer(operation)))
	if result == 0 {
		_ = file.Close()
		return nil, failure
	}
	return func() {
		// #nosec G103 -- Same live OVERLAPPED used for the matching unlock.
		_, _, _ = unlockFile.Call(file.Fd(), 0, 1, 0, uintptr(unsafe.Pointer(operation)))
		_ = file.Close()
	}, nil
}
