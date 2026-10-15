//go:build windows

// ═══ 更新日志 ═══
// 2026-10-15：初版——识别「跨卷移动」（ERROR_NOT_SAME_DEVICE）。
//
// syscall.EXDEV 在 Windows 上不存在（引用会编译失败），对应的错误码是
// ERROR_NOT_SAME_DEVICE(17)。桌面版把 auths 与 auths-trash 放在不同盘符时，
// os.Rename 报的就是它——与容器里跨 bind mount 是同一个故障。
package panel

import (
	"errors"
	"syscall"
)

// errNotSameDevice 是 Windows 的 ERROR_NOT_SAME_DEVICE。
//
// 用字面量 17 而不是具名常量：标准库的 syscall 包**没有**导出这个常量
// （它只存在于 golang.org/x/sys/windows，那是额外的依赖），而它是个稳定的
// 公开 Win32 错误码（winerror.h 里自 Windows NT 起没变过）。
const errNotSameDevice syscall.Errno = 17

// isCrossDevice 报告错误是否为跨卷移动（Windows 的 ERROR_NOT_SAME_DEVICE）。
func isCrossDevice(err error) bool {
	return errors.Is(err, errNotSameDevice)
}
