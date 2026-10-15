// ═══ 更新日志 ═══
// 2026-09-25：统一运行布局解析，版本代码与持久化数据分离，管理台随主程序运行并使用同一配置。
package main

import (
	"context"
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

	panelpkg "workbuddy2api/internal/panel"
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

// BootstrapAndroidLayout 在**读配置之前**准备安卓端的目录与首次运行的配置。
//
// 必须早于 loadConfigOrEnv：那份配置里有两处安卓特有的默认值（只绑回环、
// 关掉跑不了的脚本类任务），而 loadConfigOrEnv 在没有配置文件时会用内置默认值
// ——那就把安卓的取舍绕过去了（表现为「安卓上默认监听所有网卡」）。
//
// 为什么不让 Kotlin 侧写这份配置：默认值与网关的调度语义强耦合，放在 Go 里是
// 单一事实来源；写在 Java 里会在下次改默认值时被漏掉（那份 JSON 没人会想起去改）。
//
// 只在配置**不存在**时写：用户改过的配置绝不能被覆盖。
func BootstrapAndroidLayout(configPath string) error {
	if strings.TrimSpace(os.Getenv("WB2API_ANDROID")) != "1" {
		return nil
	}
	// configPath 形如 <root>/config/config.json，数据根是它上两级。
	layout := android.NewLayout(filepath.Dir(filepath.Dir(configPath)), os.Getenv("WB2API_HELPER_DIR"))
	if err := layout.Ensure(); err != nil {
		return err
	}
	_, err := layout.EnsureConfig()
	return err
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
	// 辅助程序可能**不在网关旁边**：容器里部署脚本把网关本体放在
	// data/updates/runtime-*/ 下（热更新要换的就是它），而辅助程序留在 /app。
	// 只查 exe 同目录会让「Go 引擎 + 容器形态」直接起不来——实测报
	// "incomplete runtime: login is unavailable"，而 /app/login 明明存在
	// （2026-10-13 在 VPS 上隔离验证时发现，幸亏没直接切生产）。
	//
	// 按顺序找：显式指定 → 网关旁边 → 工作目录（容器里 cwd 就是 /app）。
	helperDir := strings.TrimSpace(os.Getenv("WB2API_HELPER_DIR"))
	workDir, _ := os.Getwd()
	for _, name := range required {
		if helperDir != "" {
			path := filepath.Join(helperDir, android.LibName(strings.TrimSuffix(name, ".py")))
			if _, err := os.Stat(path); err != nil {
				return unifiedLayout{}, fmt.Errorf("incomplete runtime: %s is unavailable", name)
			}
			continue
		}
		found := false
		for _, dir := range []string{runtimeDir, workDir} {
			if dir == "" {
				continue
			}
			if _, err := os.Stat(filepath.Join(dir, executableName(name))); err == nil {
				found = true
				break
			}
		}
		if !found {
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
//
// allowCIDRs 非空时整套服务（面板 + API）外面再包一层来源网段闸门：不在白名单
// 且非回环的来源直接 403。面板侧入口有同一份判定（panel.Server 的
// AdminClientAllowed），这里补的是 **API 侧**——此前 /v1/* 不经过面板闸门，
// 配了 admin_allow_cidrs 也只护住面板，API 仍然全可达。放组装层（而不是
// internal/server）是因为闸门口径与面板共享（panel.NetworksAllow），
// internal/server 不该依赖面板包。
func withPanel(gateway http.Handler, panel http.Handler, allowCIDRs string) http.Handler {
	mux := http.NewServeMux()
	prefix := adminPrefix()
	mux.Handle(prefix+"/", panel)
	mux.HandleFunc(prefix, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, prefix+"/", http.StatusTemporaryRedirect)
	})
	mux.Handle("/", gateway)
	if strings.TrimSpace(allowCIDRs) == "" {
		return mux
	}
	proxies := os.Getenv("WB2API_TRUSTED_PROXIES")
	if proxies == "" {
		proxies = "127.0.0.1/32,::1/128"
	}
	// panelServer 是 *panel.Server（Go 引擎）或 *panelruntime.Runtime（Python 引擎）
	// 的 http.Handler；这里只需要它的处理能力，闸门口径走 panel 包的纯函数。
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !panelpkg.NetworksAllow(allowCIDRs, panelpkg.ClientIPOf(r, proxies)) {
			writeForbiddenOrigin(w)
			return
		}
		mux.ServeHTTP(w, r)
	})
}

// writeForbiddenOrigin 是来源闸门的拒绝响应。
func writeForbiddenOrigin(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusForbidden)
	_, _ = w.Write([]byte(`{"error":{"message":"source network not allowed by admin_allow_cidrs","type":"forbidden"}}`))
}

// reloadHandler 处理 /runtime/reload：让网关重新加载配置与账号。
//
// ═══ 为什么需要它，以及为什么不能只靠进程交接 ═══
//
// 面板加号 / 删号 / 改任务开关之后，网关必须重新读一遍 auths 与 config 才生效。
// Linux 上的做法是「起新实例、交接监听套接字、旧进程退出」——但那在 **Windows
// 上根本做不到**（见 internal/hotupdate/fd_windows.go：无法复制监听套接字）。
//
// 后果很具体（2026-10-13 用户实测）：Windows 桌面版扫码登录，上游显示「登录成功」，
// 面板却一直停在「等待扫码…」——因为凭证已经写进 auths 了，但随后的 reload 失败，
// 面板把整件事报成失败，前端只认 ok=true，于是无限轮询。
//
// 所以这里改成**进程内重载**：重新读 auths 与 config，原地更新号池。不需要新进程，
// 也就不需要套接字交接，Windows 上同样成立；顺带比换进程更快（毫秒级，无中断）。
//
// 热更新（换二进制）仍然走 handover——那个确实需要新进程，且只在 Linux/桌面换版本
// 时发生，不是「加一个账号」的必经路径。
func reloadHandler(reload func(context.Context) error, manager *hotupdate.Manager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// This handler is installed only on the private management socket.
		w.Header().Set("Content-Type", "application/json")
		if reload != nil {
			if err := reload(r.Context()); err != nil {
				w.WriteHeader(http.StatusConflict)
				_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "message": err.Error()})
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
			return
		}
		// 没有进程内重载能力时（不该发生）才回落到交接。
		if err := manager.Reload(r.Context()); err != nil {
			w.WriteHeader(http.StatusConflict)
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "message": err.Error()})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
	}
}

// lanAllowCIDRs 返回当前生效的来源网段白名单（读网关实际加载的那份 config.json）。
//
// configPath 来自 -config flag（网关真实加载的路径），**不是** WB2API_CONFIG_PATH
// 环境变量——那个变量 native 形态下经常没设（板子 systemd unit 只设 GATEWAY_DIR
// 一族），回落到相对路径 config.json 会读错文件，闸门形同虚设（2026-10-15 板子
// 实测：白名单写了 10.99/16，本机 192.168.5.x 仍 200——就是读错了配置路径）。
//
// 空串 = 不启用闸门。注意**不按 listen 是否回环过滤**：运维在只绑回环的部署里
// 手写了 admin_allow_cidrs 也照常生效（闸门挡的是来源不是监听面，两层独立）；
// 面板的局域网开关（SetLANAccess）只在开启时写这个键，但手写配置同样受尊重。
func lanAllowCIDRs(configPath string) string {
	if strings.TrimSpace(configPath) == "" {
		configPath = "config.json"
	}
	raw, err := os.ReadFile(configPath)
	if err != nil {
		return ""
	}
	var cfg struct {
		AdminAllowCIDRs string `json:"admin_allow_cidrs"`
	}
	if json.Unmarshal(raw, &cfg) != nil {
		return ""
	}
	return strings.TrimSpace(cfg.AdminAllowCIDRs)
}
