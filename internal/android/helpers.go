// ═══ 更新日志 ═══
// 2026-10-13：初版——安卓端把辅助程序从 APK 的 native lib 目录解出来。
//
// ═══ 为什么辅助程序要打进 APK 的 jniLibs ═══
//
// 安卓的 App 私有目录**不允许执行**任何文件（挂载参数是 noexec）。这是系统级限制，
// 不是权限问题，`chmod +x` 也没用。唯一可执行的位置是 APK 里 `lib/<abi>/` 下的
// `.so` 文件——安装时系统把它们解到 `/data/app/.../lib/<abi>/`，那个目录是可执行的。
//
// 于是打包时把 Go 二进制改名成 `lib<name>.so` 放进 jniLibs（构建脚本负责），
// 运行时就能直接从 nativeLibraryDir 起进程。这也是 Go/Flutter/Unity 等在安卓上
// 「带一个外部可执行文件」的通行做法。
//
// 本文件只做**校验与解析**：路径由 Kotlin 侧通过 `nativeLibraryDir` 传进来，
// 这里负责确认每个辅助程序都在，缺哪个就明确报出来（而不是等到运维点按钮才发现）。
package android

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// HelperNames 是安卓端需要随包分发的辅助程序。
//
// 为什么是这几个：面板会 shell out 的只有 login（扫码加号）、credit（查积分）、
// fission_bin（补发邀请）。signin_bin / trial_bin / activity_bin 只被 shell 脚本
// 和排程用——排程里的 school/growth 走的是 Python 脚本，安卓上不启用，所以不带。
var HelperNames = []string{"login", "credit", "fission_bin"}

// HelperLibPrefix 是辅助程序在 APK 里的文件名前缀。
//
// 安卓只把 `lib*.so` 当原生库解包并赋予可执行权限，所以打包时
// `login` → `libwb2login.so`。前缀用 `wb2` 是为了避免与真实的三方 .so 撞名。
const HelperLibPrefix = "libwb2"

// LibName 返回某个辅助程序在 APK 里的 .so 文件名。
func LibName(helper string) string {
	return HelperLibPrefix + helper + ".so"
}

// ResolveHelpers 在 nativeLibraryDir 里找出全部辅助程序的可执行路径。
//
// 找不到任何一个都返回错误，并把**缺哪个**说清楚：安卓上「按钮点了没反应」是最难
// 定位的一类故障，而「缺少 login」这种信息能直接指向打包问题。
func ResolveHelpers(nativeLibraryDir string) (map[string]string, error) {
	if strings.TrimSpace(nativeLibraryDir) == "" {
		return nil, fmt.Errorf("没有拿到 nativeLibraryDir（打包配置有问题）")
	}
	resolved := make(map[string]string, len(HelperNames))
	var missing []string
	for _, name := range HelperNames {
		path := filepath.Join(nativeLibraryDir, LibName(name))
		info, err := os.Stat(path)
		if err != nil || info.IsDir() {
			missing = append(missing, LibName(name))
			continue
		}
		// 只检查存在性，不检查可执行位：jniLibs 里的文件由系统安装时解包，
		// 权限由系统给（一定是 0755），而 os.Stat 的 Mode 在部分安卓版本上
		// 拿不到真实位——按它判会误报「不可执行」。
		resolved[name] = path
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("缺少辅助程序：%s（请检查 APK 的 jniLibs 是否完整）", strings.Join(missing, "、"))
	}
	return resolved, nil
}
