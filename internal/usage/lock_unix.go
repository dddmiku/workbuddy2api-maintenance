// ═══ 更新日志 ═══
// 2026-09-17：用量账本的跨进程文件锁（flock）。热更新期间新旧进程同时落盘时，
//             读-改-写必须互斥，否则一方的新增量会被另一方覆盖。
// 2026-09-24：原子替换使用平台实现，Unix 保持单次 rename 的原有语义。
//go:build !windows

package usage

import (
	"errors"
	"os"
	"syscall"
)

func replaceLedger(source, target string) error { return os.Rename(source, target) }

// syncLedgerDir 在 rename 之后同步父目录，让「目录项替换」本身落盘。
// 只刷 tmp 文件内容不够：崩溃发生在 rename 返回后、目录元数据落盘前时，
// 重启看到的仍是旧账本（2026-09-30 深度体检发现，与 requestlog/apikeys 同口径）。
func syncLedgerDir(dir string) error {
	// #nosec G304 -- 账本目录由账本路径推导，来自管理员配置，非请求输入
	handle, err := os.Open(dir)
	if err != nil {
		return err
	}
	return errors.Join(handle.Sync(), handle.Close())
}

// lockLedger 对账本旁路文件加排他锁；返回的解锁函数必须调用。
//
// 锁加在 <path>.lock 上而不是账本本身：账本是 tmp + rename 原子替换的，
// rename 之后 inode 会变，锁在被替换掉的旧 inode 上就失去意义。
func lockLedger(path string) (func(), error) {
	// #nosec G304 -- 锁文件路径由账本路径推导，来自管理员配置，非请求输入
	file, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX); err != nil {
		_ = file.Close()
		return nil, err
	}
	return func() {
		_ = syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
		_ = file.Close()
	}, nil
}
