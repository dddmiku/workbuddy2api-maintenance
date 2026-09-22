//go:build windows

// ═══ 更新日志 ═══
// 2026-09-18：Windows 用文件区域锁保护状态读改写，与 Unix 端保持相同的跨实例合并边界。
package pool

import (
	"os"
	"syscall"
	"unsafe"
)

var poolKernel = syscall.NewLazyDLL("kernel32.dll")
var poolLockFile = poolKernel.NewProc("LockFileEx")
var poolUnlockFile = poolKernel.NewProc("UnlockFileEx")

func lockPoolState(path string) (func(), error) {
	// #nosec G304 -- 锁文件路径由 state.json 路径推导，来自管理员配置，非请求输入
	file, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	operation := &syscall.Overlapped{}
	// #nosec G103 -- LockFileEx 需要把 OVERLAPPED 结构体地址交给 syscall，
	// operation 在本函数内分配且活到解锁闭包返回，不存在悬垂指针。
	result, _, failure := poolLockFile.Call(file.Fd(), 2, 0, 1, 0, uintptr(unsafe.Pointer(operation)))
	if result == 0 {
		_ = file.Close()
		return nil, failure
	}
	return func() {
		// #nosec G103 -- 同上：解锁用的仍是同一个 operation
		// 解锁失败没有可做的补救：进程即将释放这个句柄，锁随句柄关闭一并释放。
		_, _, _ = poolUnlockFile.Call(file.Fd(), 0, 1, 0, uintptr(unsafe.Pointer(operation)))
		_ = file.Close()
	}, nil
}
