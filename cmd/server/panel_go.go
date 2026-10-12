// ═══ 更新日志 ═══
// 2026-10-13：初版——Go 版面板的挂载（与 Python 版共存，按环境变量选择引擎）。
//
// 为什么两套并存而不是直接替换：容器里已经跑着 Python 面板的部署是**在线**的，
// 它们的 credentials.json、登录 cookie、运维习惯都是既成事实。切引擎是一次
// 可回退的部署动作，不该和「加一个安卓端」这件事绑在一起强制发生。所以：
//
//   - 容器部署默认仍走 Python（WB2API_PANEL_GO 未设时行为零变更）；
//   - 桌面版与安卓版走 Go（它们没有 CPython 可用，Python 引擎根本起不来）；
//   - WB2API_PANEL_GO=0/1 可显式覆盖，用于灰度与回退。
//
// Go 引擎不需要「解临时目录 + 起子进程 + 就绪探测 + 反代」那一整条链：它就在网关
// 进程里，直接注册 handler。少一层进程也少一整类故障（子进程崩溃、socket 残留、
// 就绪超时）。
package main

import (
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"workbuddy2api/internal/android"
	"workbuddy2api/internal/panel"
	panelassets "workbuddy2api/panel"
)

// useGoPanel 决定面板引擎。
//
// 优先级：显式 WB2API_PANEL_GO > 运行形态。原生形态（桌面/安卓，WB2API_RUNTIME=native）
// 一定用 Go：那些环境里没有 CPython，Python 引擎会一直起不来并在后台反复重试。
func useGoPanel() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("WB2API_PANEL_GO"))) {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	}
	return strings.EqualFold(strings.TrimSpace(os.Getenv("WB2API_RUNTIME")), "native")
}

// goPanelOptions 由统一布局构造 Go 面板的依赖。
func goPanelOptions(layout unifiedLayout) panel.Options {
	opts := layout.panel
	return panel.Options{
		AdminDir:        opts.AdminDir,
		AuthDir:         opts.AuthDir,
		TrashDir:        filepath.Join(opts.BaseDir, "auths-trash"),
		ConfigPath:      opts.ConfigPath,
		BaseDir:         opts.BaseDir,
		RuntimeDir:      opts.RuntimeDir,
		GatewayURL:      gatewayURL(),
		AdminPrefix:     opts.AdminPrefix,
		Container:       panelContainer(),
		Runtime:         strings.TrimSpace(os.Getenv("WB2API_RUNTIME")),
		TrustedProxies:  opts.TrustedProxies,
		AdminAllowCIDRs: strings.TrimSpace(os.Getenv("WB2API_ADMIN_ALLOW_CIDRS")),
		LogPath:         opts.LogPath,
		Assets:          panelassets.FrontendFiles(),
		HelperPaths:     helperPaths(opts.RuntimeDir),
	}
}

// helperPaths 解析辅助程序的实际路径（名字 → 绝对路径）。
//
// 桌面版留空：辅助程序就在 RuntimeDir 下，按平台拼名即可。安卓版必须给显式路径，
// 因为**应用私有目录是 noexec**（挂载参数，不是权限问题，chmod +x 也没用），辅助
// 程序只能放在 APK 的 jniLibs 里、由系统解到 nativeLibraryDir——那里才可执行。
// 所以安卓侧把该目录通过 WB2API_HELPER_DIR 传进来，这里把「名字 → 路径」显式算出来。
func helperPaths(runtimeDir string) map[string]string {
	dir := strings.TrimSpace(os.Getenv("WB2API_HELPER_DIR"))
	if dir == "" {
		return nil
	}
	paths := map[string]string{}
	for _, name := range android.HelperNames {
		// jniLibs 里的文件名是 libwb2<name>.so（安卓只把 lib*.so 当原生库解包并
		// 赋予可执行权限）。名字映射写在 internal/android，打包脚本与运行时共用一份。
		paths[name] = filepath.Join(dir, android.LibName(name))
	}
	return paths
}

// gatewayURL 是网关自身的 HTTP 地址（管理 socket 不可用时的回落路径）。
//
// 取配置里的 listen 而不是写死 127.0.0.1:7863：桌面版会在首选端口被占时换端口，
// 写死会让回落路径连到一个没人监听的地址。
func gatewayURL() string {
	if explicit := strings.TrimSpace(os.Getenv("WB2API_GATEWAY_URL")); explicit != "" {
		return explicit
	}
	addr := strings.TrimSpace(os.Getenv("WB2A_LISTEN"))
	if addr == "" {
		addr = "127.0.0.1:7863"
	}
	if strings.HasPrefix(addr, ":") {
		addr = "127.0.0.1" + addr
	}
	return "http://" + addr
}

// panelContainer 是网关容器名（Go 面板在容器模式下用它拼 docker 命令）。
func panelContainer() string {
	if name := strings.TrimSpace(os.Getenv("WB2API_CONTAINER")); name != "" {
		return name
	}
	return "workbuddy2api"
}

// startGoPanel 构造 Go 面板；失败只记录不拦启动。
//
// 面板构造失败（配置读不到、资源缺失）不该让网关 API 也起不来——面板是运维界面，
// 网关是服务本身。这与 Python 引擎的 RetryInitialFailure 是同一条取舍。
func startGoPanel(layout unifiedLayout) (http.Handler, error) {
	server, err := panel.New(goPanelOptions(layout))
	if err != nil {
		return nil, err
	}
	log.Printf("[panel] 使用 Go 版面板引擎（无需 Python）")
	return server, nil
}
