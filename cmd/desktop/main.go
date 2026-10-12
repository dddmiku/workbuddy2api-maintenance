// ═══ 更新日志 ═══
// 2026-10-13：初版——桌面启动器：准备运行目录、找 Python、拉起网关、开浏览器、管生命周期。
//
// 桌面分发（.app / .exe）与容器/服务器部署的差别，全在这个文件里：
//
//	容器里                  桌面上
//	─────────────────────   ─────────────────────────────────────────────
//	/app 由镜像构建         用户双击，没有构建步骤，目录要自己建
//	config.json 由挂载提供   首次运行要生成一份可用的默认配置
//	python3 一定在          用户机器上可能只有 python.exe，或干脆没有
//	docker 管进程           自己要当 PID 1：管信号、管退出、别留僵尸
//	用户自己去开面板         启动后自动打开浏览器
//
// 关键设计：**启动器不复制网关代码**。它只做「准备环境 + exec 网关二进制」，
// 网关本身仍然是从 cmd/server 编出来的那一个，行为与服务器版逐字一致。
// 这样桌面版不会变成第二套实现，也不会出现「桌面能跑、服务器不能跑」的分叉。
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"
)

// 版本由构建脚本注入（与网关同一套 ldflags 约定）。
var (
	version = "dev"
	builtAt = "unknown"
)

// desktopLayout 是桌面版的数据目录布局。
//
// 全部数据放在**可执行文件旁边**（便携式），而不是 %APPDATA% / ~/Library：
// 用户把整个文件夹拷到 U 盘或另一台机器就能接着用，账号与密钥跟着走；
// 也方便「备份 = 拷贝文件夹」。macOS 的 .app 是只读包体，所以实际根目录由
// 启动器传入（见 rootDir）。
type desktopLayout struct {
	Root       string // 数据根目录
	Config     string // config/config.json
	ConfigDir  string
	Auths      string
	AuthTrash  string
	DataDir    string
	PanelData  string
	RuntimeDir string // 网关二进制所在目录（辅助程序也在那）
}

func layoutFor(root, runtimeDir string) desktopLayout {
	return desktopLayout{
		Root:       root,
		ConfigDir:  filepath.Join(root, "config"),
		Config:     filepath.Join(root, "config", "config.json"),
		Auths:      filepath.Join(root, "auths"),
		AuthTrash:  filepath.Join(root, "auths-trash"),
		DataDir:    filepath.Join(root, "data"),
		PanelData:  filepath.Join(root, "panel-data"),
		RuntimeDir: runtimeDir,
	}
}

// ensureLayout 建出运行所需的全部目录，并在首次运行时生成 config.json。
//
// 幂等：已存在的一律不动。用户改过的配置不会被启动器覆盖——这是硬要求，
// 否则「重启一次配置就回默认」会让面板里的设置全部失效。
func ensureLayout(l desktopLayout) error {
	for _, dir := range []string{l.ConfigDir, l.Auths, l.AuthTrash, l.DataDir, l.PanelData} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			return fmt.Errorf("创建目录 %s: %w", dir, err)
		}
	}
	if _, err := os.Stat(l.Config); err == nil {
		return nil // 已有配置，绝不覆盖
	}
	if err := os.WriteFile(l.Config, []byte(defaultConfig()), 0600); err != nil {
		return fmt.Errorf("写入默认配置: %w", err)
	}
	return nil
}

// defaultConfig 生成一份开箱可用的桌面配置。
//
// 与 config.example.json 的差别：路径全部相对数据根目录（启动器已把工作目录
// 切到那里，所以 "./auths" 就落在用户的数据目录里），且**不开自动更新**——
// 桌面版是便携包，更新靠用户换新版文件夹，而不是让二进制在用户机器上自我替换
// （那在 macOS 上会破坏签名，在 Windows 上会撞正在运行的文件锁）。
func defaultConfig() string {
	cfg := map[string]any{
		"listen":          "127.0.0.1:7863",
		"api_key":         "",
		"api_keys_file":   "./data/api_keys.json",
		"api_keys_socket": "./data/api_keys.sock",
		"auth_dir":        "./auths",
		"state_file":      "./data/state.json",
		"server": map[string]any{
			"max_body_mb": 8,
		},
		"update": map[string]any{"enabled": false},
		"schedule": map[string]any{
			"checkin_hours": []int{9, 21}, "travel_hours": []int{9, 21},
			"activity_hours": []int{10}, "keepalive_hours": []int{22},
			"school_hours": []int{12}, "cat_hours": []int{1},
			"redeem_hours": []int{9}, "lottery_hours": []int{21},
			"makeup_hours": []int{9}, "trae_check_hours": []int{10},
			"checkin_enabled": true, "travel_enabled": true,
			"activity_enabled": true, "keepalive_enabled": true,
			"school_enabled": true, "cat_enabled": true,
			"redeem_enabled": true, "lottery_enabled": true,
			"makeup_enabled": true, "trae_check_enabled": true,
		},
	}
	raw, _ := json.MarshalIndent(cfg, "", "  ")
	return string(raw) + "\n"
}

// findPython 找一个**真的能执行**的 Python 3 解释器。
//
// 为什么要逐个试而不是直接 exec.Command("python3")：
//   - Windows 官方安装器只给 python.exe，没有 python3；
//   - PATH 上常有 Microsoft Store 的 python3.exe App Execution Alias **存根**，
//     exec.Command 能找到它、但执行报 exit status 9009（仓库里已有同类教训，
//     见 internal/scheduler/school.go 的 pythonCmd 注释）；
//   - macOS 上 python3 通常有，但可能是 Xcode 命令行工具的一个 shim。
//
// 判据是「跑起来并打印出版本号」，不是「文件存在」。
func findPython() (string, error) {
	if v := strings.TrimSpace(os.Getenv("WB2A_PYTHON")); v != "" {
		if ok, _ := pythonWorks(v); ok {
			return v, nil
		}
		return "", fmt.Errorf("WB2A_PYTHON=%s 无法执行", v)
	}
	candidates := []string{"python3", "python"}
	if runtime.GOOS == "windows" {
		// py -3 是 Windows 官方启动器，能绕开 Store 存根。
		candidates = []string{"python3", "python", "py"}
	}
	var tried []string
	for _, name := range candidates {
		args := []string{"--version"}
		if name == "py" {
			args = []string{"-3", "--version"}
		}
		if ok, out := pythonRuns(name, args); ok {
			return name, nil
		} else {
			tried = append(tried, name+": "+out)
		}
	}
	return "", fmt.Errorf("没有找到可用的 Python 3。尝试过：\n  %s", strings.Join(tried, "\n  "))
}

func pythonWorks(name string) (bool, string) { return pythonRuns(name, []string{"--version"}) }

// pythonRuns 执行解释器并确认它报的是 Python 3。
//
// 必须校验主版本：python 在部分系统上仍指向 Python 2，而面板用了 f-string 与
// pathlib，Python 2 会以语法错误崩溃——那种失败发生在子进程里，报错很难懂。
func pythonRuns(name string, args []string) (bool, string) {
	cmd := exec.Command(name, args...)
	out, err := cmd.CombinedOutput()
	text := strings.TrimSpace(string(out))
	if err != nil {
		if text == "" {
			text = err.Error()
		}
		return false, text
	}
	if !strings.HasPrefix(text, "Python 3") {
		return false, "不是 Python 3: " + text
	}
	return true, text
}

// waitForHealth 轮询网关直到它开始服务，或超时。
func waitForHealth(addr string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	url := "http://" + addr + "/healthz"
	client := &http.Client{Timeout: 2 * time.Second}
	for time.Now().Before(deadline) {
		resp, err := client.Get(url)
		if err == nil {
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if resp.StatusCode == 200 {
				return true
			}
		}
		time.Sleep(300 * time.Millisecond)
	}
	return false
}

// freePort 在首选端口被占用时换一个。
//
// 桌面版最常见的启动失败是「上次没退干净，端口还占着」。与其让用户看到
// bind: address already in use，不如换一个端口继续跑，并把实际端口告诉用户。
func freePort(preferred string) string {
	host, port, err := net.SplitHostPort(preferred)
	if err != nil {
		host, port = "127.0.0.1", "7863"
	}
	if canBind(host, port) {
		return preferred
	}
	for candidate := 7864; candidate < 7900; candidate++ {
		if canBind(host, fmt.Sprint(candidate)) {
			return net.JoinHostPort(host, fmt.Sprint(candidate))
		}
	}
	return preferred
}

func canBind(host, port string) bool {
	ln, err := net.Listen("tcp", net.JoinHostPort(host, port))
	if err != nil {
		return false
	}
	_ = ln.Close()
	return true
}

// openBrowser 尽力打开默认浏览器；失败不影响网关运行（用户可手动访问）。
func openBrowser(url string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	case "darwin":
		cmd = exec.Command("open", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	_ = cmd.Start()
}

func main() {
	exe, err := os.Executable()
	if err != nil {
		fatal("无法定位程序自身: %v", err)
	}
	runtimeDir := filepath.Dir(exe)

	// 数据根目录：优先环境变量（便于把数据放到别处），否则用可执行文件旁边。
	// macOS 的 .app 里可执行文件在 Contents/MacOS/，数据要放到 .app **外面**，
	// 由启动脚本通过 WB2A_DESKTOP_ROOT 指定，否则写进只读包体会失败。
	root := strings.TrimSpace(os.Getenv("WB2A_DESKTOP_ROOT"))
	if root == "" {
		root = runtimeDir
	}
	layout := layoutFor(root, runtimeDir)
	if err := ensureLayout(layout); err != nil {
		fatal("%v", err)
	}

	python, err := findPython()
	if err != nil {
		printPythonHelp(err)
		os.Exit(1)
	}
	fmt.Printf("使用解释器: %s\n", python)

	// 网关二进制：与启动器同目录（打包时一起放进去）。
	gateway := filepath.Join(runtimeDir, gatewayBinaryName())
	if _, err := os.Stat(gateway); err != nil {
		fatal("找不到网关程序 %s：打包不完整，请重新下载。", gateway)
	}

	// 读端口：优先配置文件里的 listen，便于用户改端口。
	addr := listenAddr(layout.Config)
	addr = freePort(addr)
	host, port, _ := net.SplitHostPort(addr)
	if host == "" {
		host = "127.0.0.1"
	}

	cmd := exec.Command(gateway, "-config", layout.Config)
	cmd.Dir = layout.Root
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Stdin = os.Stdin
	cmd.Env = append(os.Environ(),
		"WB2API_PANEL_ENABLED=1",
		"WB2API_GATEWAY_DIR="+layout.Root,
		"WB2API_AUTHS_DIR="+layout.Auths,
		"WB2API_ADMIN_DIR="+layout.PanelData,
		"WB2API_RUNTIME_DIR="+layout.RuntimeDir,
		"WB2A_PYTHON="+python,
		"WB2A_LISTEN="+net.JoinHostPort(host, port),
	)
	if err := cmd.Start(); err != nil {
		fatal("启动网关失败: %v", err)
	}

	// 转发退出信号：用户 Ctrl+C 或系统要求退出时，把子进程一起带走，
	// 否则会在任务管理器里留下一个占着端口的孤儿进程。
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-signals
		stopChild(cmd)
		os.Exit(0)
	}()

	panelURL := fmt.Sprintf("http://%s/", addr)
	if waitForHealth(addr, 30*time.Second) {
		fmt.Printf("\n网关已启动：%s\n管理面板：%s\n（关闭本窗口即停止服务）\n\n", addr, panelURL)
		openBrowser(panelURL)
	} else {
		fmt.Printf("\n网关启动较慢或失败，请查看上方日志。地址：%s\n\n", addr)
	}

	// 等网关退出；它退了启动器也就该退了（窗口关掉 = 服务停止）。
	if err := cmd.Wait(); err != nil {
		fmt.Printf("网关退出: %v\n", err)
		os.Exit(1)
	}
	fmt.Println("网关已停止。")
}

func gatewayBinaryName() string {
	if runtime.GOOS == "windows" {
		return "wb2api.exe"
	}
	return "wb2api"
}

// listenAddr 从配置里取监听地址；读不到就用默认。
func listenAddr(configPath string) string {
	raw, err := os.ReadFile(configPath)
	if err != nil {
		return "127.0.0.1:7863"
	}
	var cfg struct {
		Listen string `json:"listen"`
	}
	if err := json.Unmarshal(raw, &cfg); err != nil || strings.TrimSpace(cfg.Listen) == "" {
		return "127.0.0.1:7863"
	}
	addr := strings.TrimSpace(cfg.Listen)
	// 配置里可能是 ":7863"（监听所有网卡）。桌面版只服务本机，强制收回 loopback，
	// 避免用户无意间把带账号的面板暴露到局域网。
	if strings.HasPrefix(addr, ":") {
		return "127.0.0.1" + addr
	}
	return addr
}

func stopChild(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	_ = cmd.Process.Signal(os.Interrupt)
	done := make(chan struct{})
	go func() { _, _ = cmd.Process.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		_ = cmd.Process.Kill()
	}
}

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "错误: "+format+"\n", args...)
	fmt.Fprintln(os.Stderr, "\n按回车键退出…")
	_, _ = bufio.NewReader(os.Stdin).ReadString('\n')
	os.Exit(1)
}

// printPythonHelp 给出可操作的安装指引，而不是一句「缺少 Python」。
func printPythonHelp(err error) {
	fmt.Fprintln(os.Stderr, "错误: "+err.Error())
	fmt.Fprintln(os.Stderr, "\n管理面板需要 Python 3（网关本体不需要）。安装方式：")
	switch runtime.GOOS {
	case "windows":
		fmt.Fprintln(os.Stderr, "  1. 打开 https://www.python.org/downloads/windows/")
		fmt.Fprintln(os.Stderr, "  2. 下载 Python 3 安装包，安装时**勾选 Add python.exe to PATH**")
		fmt.Fprintln(os.Stderr, "  3. 重开本程序")
	case "darwin":
		fmt.Fprintln(os.Stderr, "  在终端执行：xcode-select --install")
		fmt.Fprintln(os.Stderr, "  或从 https://www.python.org/downloads/macos/ 安装")
	default:
		fmt.Fprintln(os.Stderr, "  Debian/Ubuntu: sudo apt install python3")
		fmt.Fprintln(os.Stderr, "  Fedora: sudo dnf install python3")
	}
	fmt.Fprintln(os.Stderr, "\n已装但仍报错时，可用环境变量指定解释器：WB2A_PYTHON=/path/to/python3")
	fmt.Fprintln(os.Stderr, "\n按回车键退出…")
	_, _ = bufio.NewReader(os.Stdin).ReadString('\n')
}
