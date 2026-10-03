// ═══ 更新日志 ═══
// 2026-09-25：Windows 区域锁保护跨进程读改写，重试短暂共享冲突并拒绝多硬链接文件。
//go:build windows

package requestlog

import (
	"errors"
	"os"
	"syscall"
	"time"
	"unsafe"
)

var journalKernel = syscall.NewLazyDLL("kernel32.dll")
var journalLockFile = journalKernel.NewProc("LockFileEx")
var journalUnlockFile = journalKernel.NewProc("UnlockFileEx")

func platformOpen(path string, flags int, mode os.FileMode) (*os.File, error) {
	// #nosec G304 -- path 由本包内部拼接（数据目录 + 固定文件名），
	// 不来自请求输入；与 lock_unix.go 同一调用契约，只是平台实现不同。
	return os.OpenFile(path, flags, mode)
}

func singleLink(file *os.File, _ os.FileInfo) error {
	var info syscall.ByHandleFileInformation
	if err := syscall.GetFileInformationByHandle(syscall.Handle(file.Fd()), &info); err != nil {
		return err
	}
	if info.NumberOfLinks != 1 {
		return ErrUnsafePath
	}
	return nil
}

func lockStore(path string) (func(), error) {
	file, err := openPrivate(path+".lock", os.O_CREATE|os.O_RDWR)
	if err != nil {
		return nil, err
	}
	operation := &syscall.Overlapped{}
	// #nosec G103 -- OVERLAPPED belongs to this handle and remains live in the unlock closure.
	result, _, failure := journalLockFile.Call(file.Fd(), 2, 0, 1, 0, uintptr(unsafe.Pointer(operation)))
	if result == 0 {
		_ = file.Close()
		return nil, failure
	}
	return func() {
		// #nosec G103 -- Same live OVERLAPPED and handle used for the lock.
		_, _, _ = journalUnlockFile.Call(file.Fd(), 0, 1, 0, uintptr(unsafe.Pointer(operation)))
		_ = file.Close()
	}, nil
}

func replaceJournal(source, target string) error {
	for attempt := 0; ; attempt++ {
		err := os.Rename(source, target)
		if err == nil || attempt == 6 {
			return err
		}
		if !errors.Is(err, syscall.ERROR_ACCESS_DENIED) && !errors.Is(err, syscall.Errno(32)) && !errors.Is(err, syscall.Errno(33)) {
			return err
		}
		time.Sleep((5 * time.Millisecond) << attempt)
	}
}

// Windows file Sync flushes contents; a directory handle cannot be synced with
// os.File.Sync. Access control follows the containing directory's Windows ACL.
func syncDirectory(string) error { return nil }
