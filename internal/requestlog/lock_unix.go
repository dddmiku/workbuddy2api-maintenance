// ═══ 更新日志 ═══
// 2026-09-25：旁路 flock 串行化全部读改写，rename 后同步目录并防止链接文件泄露。
// 2026-09-25：注明管理员存储路径和末级链接校验边界，精确说明两处动态路径扫描误报。
//go:build !windows

package requestlog

import (
	"errors"
	"os"
	"syscall"
)

func platformOpen(path string, flags int, mode os.FileMode) (*os.File, error) {
	// #nosec G304 -- Only openPrivate supplies Open's absolute admin-configured journal/lock path, never request data; O_NOFOLLOW rejects leaf symlinks and openPrivate checks file type, identity and link count. Parent directories are trusted admin storage.
	return os.OpenFile(path, flags|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, mode)
}

func singleLink(_ *os.File, info os.FileInfo) error {
	if stat, ok := info.Sys().(*syscall.Stat_t); !ok || stat.Nlink != 1 {
		return ErrUnsafePath
	}
	return nil
}

func lockStore(path string) (func(), error) {
	file, err := openPrivate(path+".lock", os.O_CREATE|os.O_RDWR)
	if err != nil {
		return nil, err
	}
	for {
		err = syscall.Flock(int(file.Fd()), syscall.LOCK_EX)
		if !errors.Is(err, syscall.EINTR) {
			break
		}
	}
	if err != nil {
		_ = file.Close()
		return nil, err
	}
	return func() { _ = syscall.Flock(int(file.Fd()), syscall.LOCK_UN); _ = file.Close() }, nil
}

func replaceJournal(source, target string) error { return os.Rename(source, target) }

func syncDirectory(path string) error {
	// #nosec G304 -- Only replaceLocked supplies filepath.Dir(Store.path), derived from Open's absolute admin-configured path; this opens the trusted parent directory only for fsync after rename, never a request-selected path.
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	err = dir.Sync()
	return errors.Join(err, dir.Close())
}
