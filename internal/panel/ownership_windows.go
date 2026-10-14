//go:build windows

// ═══ 更新日志 ═══
// 2026-10-13：初版——Windows 上没有 uid/gid 语义，只做权限收口。
package panel

import (
	"log"
	"os"
)

// chownApp 在 Windows 上只收紧权限。
//
// 为什么不做 chown：Windows 的 ACL 模型里没有「属主 = 10001」这个概念，
// 而且网关与面板在桌面版里是同一个用户启动的，文件天然可读。
// 这里的 chmod 也只会切换只读位（Go 在 Windows 上的既有语义），保留它是为了让
// 「文件被别的进程锁住/权限模型异常」这类问题仍然留痕。
func chownApp(path string) {
	if err := os.Chmod(path, 0600); err != nil {
		log.Printf("修改权限失败 %s：%v", path, err)
	}
}
