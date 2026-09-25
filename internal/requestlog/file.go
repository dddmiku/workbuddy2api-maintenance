// ═══ 更新日志 ═══
// 2026-09-25：拒绝符号链接、硬链接与特殊文件，日志和稳定锁文件均收紧为仅所有者读写。
package requestlog

import (
	"errors"
	"os"
)

func openPrivate(path string, flags int) (*os.File, error) {
	before, err := os.Lstat(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if err == nil && !before.Mode().IsRegular() {
		return nil, ErrUnsafePath
	}
	file, err := platformOpen(path, flags, 0o600)
	if err != nil {
		return nil, err
	}
	info, err := file.Stat()
	if err == nil && !info.Mode().IsRegular() {
		err = ErrUnsafePath
	}
	if err == nil {
		err = singleLink(file, info)
	}
	current, statErr := os.Lstat(path)
	if err == nil {
		err = statErr
	}
	if err == nil && (!current.Mode().IsRegular() || !os.SameFile(info, current)) {
		err = ErrUnsafePath
	}
	if err == nil && before != nil && !os.SameFile(before, info) {
		err = ErrUnsafePath
	}
	if err == nil {
		err = file.Chmod(0o600)
	}
	if err != nil {
		_ = file.Close()
		return nil, err
	}
	return file, nil
}
