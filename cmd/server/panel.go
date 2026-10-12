// ═══ 更新日志 ═══
// 2026-09-25：统一运行布局解析，版本代码与持久化数据分离，管理台随主程序运行并使用同一配置。
package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"workbuddy2api/internal/android"
	"workbuddy2api/internal/hotupdate"
	"workbuddy2api/internal/panelruntime"
	"workbuddy2api/internal/runlog"
)

// executableName 按平台给辅助程序名补后缀。
//
// Windows 上二进制带 .exe，Linux/macOS 不带。判据用 runtime.GOOS 而不是
// 「文件是否存在」：后者会让 Linux 上恰好存在同名带 .exe 的文件时误判。
// 非可执行项（scripts/ 下的 .py）原样返回——它们没有平台后缀。
func executableName(name string) string {
	if runtime.GOOS != "windows" || strings.Contains(name, "/") {
		return name
	}
	return name + ".exe"
}

// adminPrefixDefault 面板默认挂载前缀。可用 WB2API_ADMIN_PREFIX 覆盖：
// 面板被嵌进网关的一个子路径下，这个前缀同时用于路由注册、代理剥前缀、以及
// 面板拼**公网回调地址**（Trae 授权回调）。写死 "/admin" 会让别的部署形态
// （挂在 /panel、或直接挂在根）下 Trae 回调指向一个不存在的地址。
const adminPrefixDefault = "/admin"

// adminPrefix 返回归一化后的面板前缀：以 "/" 开头、不以 "/" 结尾。
func adminPrefix() string {
	prefix := strings.TrimSpace(os.Getenv("WB2API_ADMIN_PREFIX"))
	if prefix == "" {
		return adminPrefixDefault
	}
	if !strings.HasPrefix(prefix, "/") {
		prefix = "/" + prefix
	}
	prefix = strings.TrimRight(prefix, "/")
	if prefix == "" {
		return adminPrefixDefault
	}
	return prefix
}

type unifiedLayout struct {
	enabled bool
	panel   panelruntime.Options
}

func prepareUnified(cfg *Config, configPath string) (unifiedLayout, error) {
	if os.Getenv("WB2API_PANEL_ENABLED") != "1" {
		return unifiedLayout{}, nil
	}
	base, err := os.Getwd()
	if err != nil {
		return unifiedLayout{}, err
	}
	binary, err := os.Executable()
	if err != nil {
		return unifiedLayout{}, err
	}
	runtimeDir := filepath.Dir(binary)
	// 完整性校验按**面板引擎**要什么来查，不是一刀切。
	//
	// 为什么必须分档：Go 引擎只用 login / credit / fission_bin 三个辅助程序，而
	// signin_bin / trial_bin / activity_bin 与 scripts/*.py 只服务 Python 引擎和
	// 排程脚本。安卓包里压根没有后四个——一刀切地要求全部存在，会让安卓版在启动
	// 时直接报 "incomplete runtime: signin_bin is unavailable" 起不来
	// （2026-10-13 安卓打包实测）。
	required := []string{"login", "credit", "fission_bin"}
	if !useGoPanel() {
		required = []string{"login", "credit", "signin_bin", "trial_bin", "activity_bin",
			"fission_bin", "scripts/task_common.py"}
	}
	// 安卓上辅助程序不在网关旁边，而在 APK 的 nativeLibraryDir（应用私有目录是
	// noexec，只有那里能执行）。目录由 WB2API_HELPER_DIR 传入，名字是 libwb2*.so。
	helperDir := strings.TrimSpace(os.Getenv("WB2API_HELPER_DIR"))
	for _, name := range required {
		path := filepath.Join(runtimeDir, executableName(name))
		if helperDir != "" {
			path = filepath.Join(helperDir, android.LibName(strings.TrimSuffix(name, ".py")))
		}
		if _, err := os.Stat(path); err != nil {
			return unifiedLayout{}, fmt.Errorf("incomplete runtime: %s is unavailable", name)
		}
	}
	configPath, err = filepath.Abs(configPath)
	if err != nil {
		return unifiedLayout{}, err
	}
	authDir, err := filepath.Abs(cfg.AuthDir)
	if err != nil {
		return unifiedLayout{}, err
	}
	if cfg.APIKeysSocket == "" {
		registry := cfg.APIKeysFile
		if registry == "" {
			registry = cfg.StateFile
		}
		cfg.APIKeysSocket = filepath.Join(filepath.Dir(registry), "api_keys.sock")
	}
	adminSocket, err := filepath.Abs(cfg.APIKeysSocket)
	if err != nil {
		return unifiedLayout{}, err
	}
	adminDir := os.Getenv("WB2API_ADMIN_DIR")
	if adminDir == "" {
		adminDir = filepath.Join(base, "panel-data")
	}
	adminDir, err = filepath.Abs(adminDir)
	if err != nil {
		return unifiedLayout{}, err
	}
	// #nosec G703 -- adminDir is an absolute administrator deployment setting, not an HTTP or archive value.
	if err := os.MkdirAll(adminDir, 0700); err != nil {
		return unifiedLayout{}, err
	}
	logPath, err := filepath.Abs(filepath.Join(filepath.Dir(cfg.StateFile), "logs", "gateway.log"))
	if err != nil {
		return unifiedLayout{}, err
	}
	if err := runlog.Configure(logPath); err != nil {
		return unifiedLayout{}, err
	}
	log.SetOutput(runlog.Output(os.Stderr))
	// Only child task programs consume these variables. Their paths follow the
	// active bundle, while credentials remain in the existing mounted directory.
	if err := os.Setenv("WB2API_RUNTIME_DIR", runtimeDir); err != nil {
		return unifiedLayout{}, err
	}
	if err := os.Setenv("WB2A_AUTHS", authDir); err != nil {
		return unifiedLayout{}, err
	}
	proxies := os.Getenv("WB2API_TRUSTED_PROXIES")
	if proxies == "" {
		proxies = "127.0.0.1/32,::1/128"
	}
	return unifiedLayout{enabled: true, panel: panelruntime.Options{ConfigPath: configPath, BaseDir: base, AuthDir: authDir, AdminDir: adminDir, AdminSocket: adminSocket, RuntimeDir: runtimeDir, LogPath: logPath, AdminPrefix: adminPrefix(), Python: os.Getenv("WB2A_PYTHON"), TrustedProxies: proxies}}, nil
}

// withPanel 把面板挂到 adminPrefix 下，其余路径交回网关。
//
// panel 是 http.Handler 而不是 *panelruntime.Runtime：Go 引擎直接是 handler，
// Python 引擎的 Runtime 也实现了 ServeHTTP（它反代到子进程）。两种引擎在挂载点上
// 无差别，切换引擎不需要改路由。
func withPanel(gateway http.Handler, panel http.Handler) http.Handler {
	mux := http.NewServeMux()
	prefix := adminPrefix()
	mux.Handle(prefix+"/", panel)
	mux.HandleFunc(prefix, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, prefix+"/", http.StatusTemporaryRedirect)
	})
	mux.Handle("/", gateway)
	return mux
}

func reloadHandler(manager *hotupdate.Manager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// This handler is installed only on the private management socket.
		w.Header().Set("Content-Type", "application/json")
		if err := manager.Reload(r.Context()); err != nil {
			w.WriteHeader(http.StatusConflict)
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "message": err.Error()})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
	}
}
