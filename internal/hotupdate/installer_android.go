//go:build android

// ═══ 更新日志 ═══
// 2026-10-13：初版——安卓上把更新件交给系统安装器（应用私有目录不能执行）。
package hotupdate

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
)

// PendingInstallName 是「待安装」标记文件名。
//
// ═══ 为什么用标记文件，而不是直接起安装器 ═══
//
// 网关是 App 的**子进程**，它没有 Android 的 Context，拉不起系统安装器
// （那需要 startActivity）。所以它只负责把校验过的 APK 放到约定位置并写下这个
// 标记；App 进程看到标记后去起安装器（见 GatewayService.stageApkInstaller）。
//
// 这条分工是刻意的：下载与验签留在网关（它已经有完整的签名校验与摘要比对），
// 而「拉起系统 UI」这件事只有 App 做得到。
const PendingInstallName = "pending-install.apk"

// StageInstallerForAndroid 返回一个 StageInstaller 钩子。
//
// 安卓的应用私有目录**不能执行**（SELinux 的 untrusted_app 域禁止在
// app_data_file 上 execute，实测 `fork/exec ... permission denied`，且
// setenforce 0 也要 root），所以那套「起新实例、交接监听套接字」的做法在手机上
// 走不通。改成把 APK 交给系统安装器——不需要 root，也是安卓上唯一被支持的方式。
func StageInstallerForAndroid(updateDir string) func(context.Context, string) (string, error) {
	return func(_ context.Context, assetPath string) (string, error) {
		// 把下载件改名成固定的「待安装」文件名，App 只需盯一个固定路径，
		// 不用去解析哪个版本号对应哪个文件。
		target := filepath.Join(updateDir, PendingInstallName)
		if err := os.Rename(assetPath, target); err != nil {
			// 跨设备重命名会失败（下载目录与更新目录不同挂载点时），退回复制。
			raw, readErr := os.ReadFile(assetPath)
			if readErr != nil {
				return "", fmt.Errorf("准备安装包失败：%w", err)
			}
			if writeErr := os.WriteFile(target, raw, 0o600); writeErr != nil {
				return "", fmt.Errorf("写入安装包失败：%w", writeErr)
			}
			_ = os.Remove(assetPath)
		}
		log.Printf("[update] 安装包已就绪，等待 App 拉起系统安装器：%s", target)
		return "更新包已下载并校验，请在手机上确认安装（应用数据会保留）", nil
	}
}
