// ═══ 更新日志 ═══
// 2026-09-18：Windows 明确拒绝 Unix 监听 FD 交接，避免进入不可用的 ExtraFiles 路径。
package hotupdate

import (
	"errors"
	"os"
	"syscall"
)

// errListenerInheritanceUnsupported 是 Windows 侧的固定失败原因。
//
// 特意用包级变量而不是在函数里 errors.New：后者会让 staticcheck 推导出
// listenerFile 的错误恒非 nil，进而把调用点的 `if err != nil` 报成
// 「比较恒为真」（SA4023）。那是平台差异造成的误报——在 Unix 上同一段代码
// 既能成功也能失败——用一个不可静态折叠的值把它消掉，比在每个调用点加
// lint 指令干净（那些指令在 Linux 构建下又会变成「匹配不到」）。
var errListenerInheritanceUnsupported = errors.New("listener inheritance is unsupported on Windows")

func duplicateListenerFile(raw syscall.RawConn) (*os.File, error) {
	return nil, errListenerInheritanceUnsupported
}
