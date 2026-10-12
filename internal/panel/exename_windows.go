//go:build windows

// ═══ 更新日志 ═══
// 2026-10-13：初版——Windows 上辅助程序带 .exe 后缀。
package panel

import "strings"

// platformExecutableName 给辅助程序名补 .exe。
//
// 带路径分隔符的名字（scripts/xxx.py 这类脚本项）原样返回——它们没有平台后缀。
// 写死无后缀会让桌面版报「运行辅助程序不可用」，2026-10-13 桌面打包时踩过。
func platformExecutableName(name string) string {
	if strings.ContainsAny(name, `/\`) {
		return name
	}
	return name + ".exe"
}
