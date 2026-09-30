// ═══ 更新日志 ═══
// 2026-09-18：Windows 使用文件区域锁保护完整读改写，避免多实例开发与测试互相覆盖。
// 2026-09-24：短时重试共享冲突的原子替换，避免文件读取或扫描瞬间占用导致用量无法落盘。
//go:build windows

package usage

import (
	"errors"
	"os"
	"syscall"
	"time"
	"unsafe"
)

var ledgerKernel = syscall.NewLazyDLL("kernel32.dll")
var ledgerLockFile = ledgerKernel.NewProc("LockFileEx")
var ledgerUnlockFile = ledgerKernel.NewProc("UnlockFileEx")

func replaceLedger(source, target string) error {
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

// syncLedgerDir Windows 上没有可 fsync 的目录句柄（os.File.Sync 不作用于目录），
// 目录项持久化由文件系统负责，与 requestlog/apikeys 的 Windows 实现同口径。
func syncLedgerDir(string) error { return nil }

func lockLedger(path string) (func(), error) {
	// #nosec G304 -- 锁文件路径由账本路径推导，来自管理员配置，非请求输入
	file, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	operation := &syscall.Overlapped{}
	// #nosec G103 -- LockFileEx 需要把 OVERLAPPED 结构体地址交给 syscall，
	// operation 在本函数内分配且活到解锁闭包返回，不存在悬垂指针。
	result, _, failure := ledgerLockFile.Call(file.Fd(), 2, 0, 1, 0, uintptr(unsafe.Pointer(operation)))
	if result == 0 {
		_ = file.Close()
		return nil, failure
	}
	return func() {
		// #nosec G103 -- 同上：解锁用的仍是同一个 operation
		// 解锁失败没有可做的补救：进程即将释放这个句柄，锁随句柄关闭一并释放。
		_, _, _ = ledgerUnlockFile.Call(file.Fd(), 0, 1, 0, uintptr(unsafe.Pointer(operation)))
		_ = file.Close()
	}, nil
}
