// ═══ 更新日志 ═══
// 2026-10-13：初版——受限运行通道：容器模式走 docker CLI，原生模式直接起同版辅助程序。
//
// 面板要跑的东西只有五类：./login（扫码）、./credit（积分）、./fission_bin（补发邀请）、
// 容器 inspect/restart/logs。**全部走这一个文件**，是为了让「面板能执行什么」有单一答案：
// 评审只需要读一个白名单函数，而不是在 HTTP 处理里到处找 subprocess。
//
// 原生模式（桌面/安卓）没有容器边界，所以白名单必须**更窄**而不是更宽：容器里
// docker exec 至少要能拼出容器名才可能越界，原生模式下参数直接就是 argv。因此原生
// 模式只接受**逐个字段精确匹配**的调用形态（见 nativeAllowed），任何多余的旗标一律拒绝。
package panel

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// ExecResult 是一次受限运行的输出。
type ExecResult struct {
	Code   int
	Stdout string
	Stderr string
}

// Runner 是受限运行通道：docker 或原生。
type Runner interface {
	Run(ctx context.Context, args []string, timeout time.Duration, env map[string]string) ExecResult
	// RunCredit 实现 CreditRunner，供积分缓存使用。
	RunCredit(ctx context.Context, env map[string]string) ([]byte, error)
}

// 运行失败时的固定返回码，与 Python 版一致（调用方据此区分超时/缺程序）。
const (
	execTimeoutCode = 124
	execMissingCode = 127
)

// execShortTimeout 是「只是问一句状态」这类调用的超时（inspect / healthz）。
// 它不该跟重启（210s）用同一个预算：状态查询卡住时面板要能快速回退到「未知」，
// 而不是把首屏一起拖住。
const execShortTimeout = 20 * time.Second

// DockerRunner 经 docker CLI 操作同机网关容器。
type DockerRunner struct {
	Container string
	// Runtime 为 "native" 时改走 NativeRunner 的语义（由 NewRunner 选择，这里不判）。
	Runtime string
}

// NativeRunner 直接起同版辅助程序（桌面 / 安卓）。
type NativeRunner struct {
	RuntimeDir string
	AuthDir    string
	AdminDir   string
	GatewayDir string
	// HelperPaths 显式指定辅助程序的可执行路径（名字 → 绝对路径）。
	//
	// 为什么需要它：桌面版里辅助程序就在 RuntimeDir 下、按名字拼路径即可；安卓上
	// 它们必须放在 APK 的 jniLibs 里改名成 libwb2<name>.so（应用私有目录是 noexec，
	// 只有 nativeLibraryDir 能执行）。有了这张表，两种形态共用同一段执行逻辑，
	// 安卓侧不需要为「路径不同」再写一套。
	HelperPaths map[string]string
	// Admin 是管理 socket 客户端，用于把 inspect/restart 映射成网关内部接口。
	Admin *ManagementClient
	// LogPath 是网关日志文件（runlog 写的那份），原生模式下日志页读它。
	LogPath string
}

// NewRunner 按运行形态选择执行器。
//
// 判据是环境变量 WB2API_RUNTIME=native（由网关在拉起面板时注入），与 Python 版同源。
func NewRunner(runtime, container, runtimeDir, authDir, adminDir, gatewayDir, logPath string, admin *ManagementClient, helperPaths map[string]string) Runner {
	if runtime == "native" {
		return &NativeRunner{RuntimeDir: runtimeDir, AuthDir: authDir, AdminDir: adminDir, LogPath: logPath,
			GatewayDir: gatewayDir, Admin: admin, HelperPaths: helperPaths}
	}
	if container == "" {
		container = "workbuddy2api"
	}
	return &DockerRunner{Container: container}
}

// ── docker 模式 ────────────────────────────────────────────────────────

func (d *DockerRunner) Run(ctx context.Context, args []string, timeout time.Duration, env map[string]string) ExecResult {
	// 显式 -e KEY=VALUE 前缀：面板给 ./credit 透传 WB2A_EXPIRING_SOON 时用。
	// 键名先校验，避免把任意字符串拼进 docker 的参数位。
	full := []string{}
	if len(env) > 0 {
		keys := make([]string, 0, len(env))
		for key := range env {
			if envKeyRe.MatchString(key) {
				keys = append(keys, key)
			}
		}
		sortStrings(keys)
		for _, key := range keys {
			full = append(full, "-e", key+"="+env[key])
		}
	}
	full = append(full, args...)
	return runCommand(ctx, "docker", full, timeout, nil, "")
}

func (d *DockerRunner) RunCredit(ctx context.Context, env map[string]string) ([]byte, error) {
	result := d.Run(ctx, []string{"exec", d.Container, "./credit"}, 90*time.Second, env)
	if result.Code != 0 {
		message := firstNonEmpty(result.Stderr, result.Stdout, "credit 执行失败")
		return nil, errors.New(message)
	}
	return []byte(result.Stdout), nil
}

// ── 原生模式 ──────────────────────────────────────────────────────────

// nativeAllowed 判定一条 docker 形态的 argv 在原生模式下是否被允许。
//
// 这是安全边界：原生部署没有容器隔离，面板拼出来的 argv 会直接变成进程参数。
// 因此只认**完整形态**的调用，逐字段比较，不做前缀匹配、不接受额外旗标。
// 返回 (辅助程序名, 传给它的参数, 是否允许)。
func (n *NativeRunner) nativeAllowed(args []string) (string, []string, bool) {
	if len(args) < 3 || args[0] != "exec" {
		return "", nil, false
	}
	command := args[1:]
	// 剥掉面板自己拼的 -e KEY=VALUE 前缀（给 ./credit 透传 WB2A_EXPIRING_SOON）。
	for len(command) >= 2 && command[0] == "-e" {
		pair := command[1]
		key, _, found := strings.Cut(pair, "=")
		if !found || !envKeyRe.MatchString(key) {
			return "", nil, false
		}
		command = command[2:]
	}
	// 剥完旗标后第一个位置参数必须是容器名。
	if len(command) == 0 || command[0] != n.containerName() {
		return "", nil, false
	}
	command = command[1:]
	if len(command) == 0 {
		return "", nil, false
	}
	switch command[0] {
	case "./credit":
		if len(command) == 1 {
			return "credit", nil, true
		}
	case "./login":
		// 只允许 --realm=cn|global、可选 --session=<32 hex>、末尾 url|poll。
		if len(command) != 3 && len(command) != 4 {
			return "", nil, false
		}
		if command[1] != "--realm=cn" && command[1] != "--realm=global" {
			return "", nil, false
		}
		if len(command) == 4 && !sessionFlagRe.MatchString(command[2]) {
			return "", nil, false
		}
		last := command[len(command)-1]
		if last != "url" && last != "poll" {
			return "", nil, false
		}
		return "login", command[1:], true
	case "./fission_bin":
		// 补发邀请：只允许 -uid <数字> 与 -gen <非负整数>。面板只按 uid 补发，
		// 不暴露设备号代数（默认 gen=0 已是账号默认设备）。
		if fissionArgsAllowed(command[1:]) {
			return "fission_bin", command[1:], true
		}
	}
	return "", nil, false
}

// resolveHelper 定位一个辅助程序：显式路径优先，其次运行目录按平台拼名。
func (n *NativeRunner) resolveHelper(name string) string {
	if path := n.HelperPaths[name]; path != "" {
		return path
	}
	// 网关旁边优先（桌面/安卓形态），其次工作目录。
	//
	// 容器里部署脚本把网关本体放在 data/updates/runtime-*/ 下，辅助程序留在 /app
	// ——而容器的 cwd 就是 /app。只看网关旁边会让「Go 引擎 + 容器形态」下所有
	// 辅助程序调用都报「运行辅助程序不可用」（扫码加号、积分、补发邀请全废）。
	candidate := ResolveGatewayBinary(n.RuntimeDir, name)
	if fileExists(candidate) {
		return candidate
	}
	if n.GatewayDir != "" {
		if fallback := ResolveGatewayBinary(n.GatewayDir, name); fileExists(fallback) {
			return fallback
		}
	}
	return candidate
}

func (n *NativeRunner) containerName() string {
	if name := strings.TrimSpace(os.Getenv("WB2API_CONTAINER")); name != "" {
		return name
	}
	return "workbuddy2api"
}

func (n *NativeRunner) Run(ctx context.Context, args []string, timeout time.Duration, env map[string]string) ExecResult {
	if len(args) == 3 && args[0] == "inspect" && args[2] == n.containerName() {
		// 「容器在跑吗」在原生模式下 = 网关健康接口是否正常。
		if n.Admin == nil || !n.Admin.ManagementEnabled() {
			return ExecResult{Code: 1, Stderr: "管理通道不可用"}
		}
		// **不能要求 code == 200**：/healthz 在一个账号都没有时返回 503，而那正是
		// 「网关活着、只是池里还没有可服务的号」——刚装好、还没加号时的正常状态。
		// 只认 200 会让面板把「运行中」判成「已停止」（2026-10-13 真机实测）。
		// 判据是响应体里的 service 字段：它是网关的身份标识。
		_, payload := n.Admin.Request(ctx, "GET", "/healthz", nil)
		running := jsonString(payload["service"]) == "workbuddy2api"
		if running {
			return ExecResult{Code: 0, Stdout: "true"}
		}
		return ExecResult{Code: 0, Stdout: "false"}
	}
	if len(args) == 2 && args[0] == "restart" && args[1] == n.containerName() {
		if n.Admin == nil || !n.Admin.ManagementEnabled() {
			return ExecResult{Code: 1, Stderr: "管理通道不可用"}
		}
		code, payload := n.Admin.Request(ctx, "POST", "/runtime/reload", map[string]any{})
		if code == 200 && jsonBool(payload["ok"], false) {
			return ExecResult{Code: 0, Stdout: "runtime reloaded"}
		}
		return ExecResult{Code: 1, Stderr: gatewayEnvelopeError(payload)}
	}
	if len(args) == 4 && args[0] == "logs" && args[1] == "--tail" && args[3] == n.containerName() {
		// 原生模式没有 docker logs 可用，日志就在本机文件里——直接读它。
		//
		// 这里原本返回「请直接查看运行日志文件」，等于**面板的日志页在桌面/安卓上是
		// 空白的**（2026-10-13 实测：/api/logs 返回 rc=1、rows=[]）。Python 版的
		// native_runtime 实现了同一件事（按 runlog 的轮转顺序读 .2 → .1 → 当前，
		// 且封顶 8MB），Go 版漏了，于是同一个面板在两个引擎下表现不一致。
		return tailLogFile(n.LogPath, args[2])
	}
	name, argv, allowed := n.nativeAllowed(args)
	if !allowed {
		return ExecResult{Code: 1, Stderr: "不支持此运行操作"}
	}
	executable := n.resolveHelper(name)
	if !fileExists(executable) {
		return ExecResult{Code: execMissingCode, Stderr: "运行辅助程序不可用，请检查完整版本包"}
	}
	childEnv := map[string]string{
		"WB2A_AUTH_DIR":          n.AuthDir,
		"WB2API_LOGIN_STATE_DIR": filepath.Join(n.AdminDir, "login-states"),
	}
	for key, value := range env {
		childEnv[key] = value
	}
	return runCommand(ctx, executable, argv, timeout, childEnv, n.GatewayDir)
}

func (n *NativeRunner) RunCredit(ctx context.Context, env map[string]string) ([]byte, error) {
	result := n.Run(ctx, []string{"exec", n.containerName(), "./credit"}, 90*time.Second, env)
	if result.Code != 0 {
		return nil, errors.New(firstNonEmpty(result.Stderr, result.Stdout, "credit 执行失败"))
	}
	return []byte(result.Stdout), nil
}

// ── 共用 ──────────────────────────────────────────────────────────────

var (
	envKeyRe      = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	sessionFlagRe = regexp.MustCompile(`^--session=[a-f0-9]{32}$`)
	uidFlagRe     = regexp.MustCompile(`^[0-9]{1,32}$`)
	genFlagRe     = regexp.MustCompile(`^[0-9]{1,4}$`)
)

// fissionArgsAllowed 校验 ./fission_bin 的参数：只允许 -uid <数字> 与 -gen <非负整数>。
//
// 任何能拼出任意路径或注入旗标的参数都要拒掉；-auths 不接受（由运行通道自己注入
// WB2A_AUTH_DIR），否则面板就成了一条「读任意目录」的通道。
func fissionArgsAllowed(args []string) bool {
	index := 0
	for index < len(args) {
		flag := args[index]
		if flag != "-uid" && flag != "-gen" {
			return false
		}
		if index+1 >= len(args) {
			return false
		}
		value := args[index+1]
		if flag == "-uid" {
			if !uidFlagRe.MatchString(value) {
				return false
			}
		} else if !genFlagRe.MatchString(value) {
			return false
		}
		index += 2
	}
	return true
}

// runCommand 起一个子进程并收集输出；超时/找不到程序映射成固定返回码。
func runCommand(ctx context.Context, name string, args []string, timeout time.Duration, extraEnv map[string]string, dir string) ExecResult {
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	// #nosec G204 -- name 与 args 全部由本文件的受限运行通道构造，不是外部输入
	cmd := exec.CommandContext(runCtx, name, args...)
	if dir != "" {
		cmd.Dir = dir
	}
	if len(extraEnv) > 0 {
		cmd.Env = append(os.Environ(), flattenEnv(extraEnv)...)
	}
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	cmd.Stdin = nil
	err := cmd.Run()
	out, errText := strings.TrimSpace(stdout.String()), strings.TrimSpace(stderr.String())
	if err == nil {
		return ExecResult{Code: 0, Stdout: out, Stderr: errText}
	}
	if errors.Is(runCtx.Err(), context.DeadlineExceeded) {
		return ExecResult{Code: execTimeoutCode, Stdout: out, Stderr: fmt.Sprintf("运行操作超时(%ds)", int(timeout.Seconds()))}
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return ExecResult{Code: exitErr.ExitCode(), Stdout: out, Stderr: errText}
	}
	if errors.Is(err, exec.ErrNotFound) || errors.Is(err, os.ErrNotExist) {
		return ExecResult{Code: execMissingCode, Stdout: out, Stderr: "找不到可执行程序"}
	}
	return ExecResult{Code: 1, Stdout: out, Stderr: firstNonEmpty(errText, err.Error())}
}

func flattenEnv(env map[string]string) []string {
	keys := make([]string, 0, len(env))
	for key := range env {
		keys = append(keys, key)
	}
	sortStrings(keys)
	out := make([]string, 0, len(keys))
	for _, key := range keys {
		out = append(out, key+"="+env[key])
	}
	return out
}

// sortStrings 是 sort.Strings 的薄包装，避免在本文件再引一次 sort。
func sortStrings(items []string) {
	for i := 1; i < len(items); i++ {
		for j := i; j > 0 && items[j] < items[j-1]; j-- {
			items[j], items[j-1] = items[j-1], items[j]
		}
	}
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

// DecodeExecJSON 把辅助程序的 stdout 解析成对象；解析失败时把原文前 400 字节带上，
// 便于运维在面板上直接看到「输出无法解析」的原因。
func DecodeExecJSON(stdout string) (map[string]any, string) {
	var result map[string]any
	if err := json.Unmarshal([]byte(stdout), &result); err != nil {
		return nil, truncateText(stdout, 400)
	}
	return result, ""
}

// logTailLimit 单次读取的日志字节上限。
//
// 与 Python 版同值（8MB）。运维可能用一个大得多的诊断文件替换掉日志文件，
// 没有上限就会把它整个读进内存。
const logTailLimit = 8 << 20

// tailLogFile 从网关日志文件里取最后 count 行（按 runlog 的轮转顺序）。
//
// 顺序是 .2 → .1 → 当前：runlog 轮转时把当前改名成 .1、旧 .1 变成 .2，所以时间上
// 最早的是 .2。倒过来读会让「最近的日志」里混进更早的内容。
//
// 每个文件从**尾部**最多读 8MB：日志可能有几百 MB，而这里只要最后几百行。
// 截断处可能落在行中间，所以跳掉第一个不完整的行（Python 版同此处理）。
func tailLogFile(logPath, rawCount string) ExecResult {
	count, err := strconv.Atoi(strings.TrimSpace(rawCount))
	if err != nil || count < 1 {
		return ExecResult{Code: 1, Stderr: "无效日志范围"}
	}
	if count > 5000 {
		count = 5000
	}
	if logPath == "" {
		return ExecResult{Code: 1, Stderr: "未配置日志文件路径"}
	}
	lines := make([]string, 0, count)
	for _, path := range []string{logPath + ".2", logPath + ".1", logPath} {
		chunk, err := readTailLines(path, logTailLimit)
		if err != nil {
			continue // 文件不存在是正常情况（轮转档还没生成）
		}
		lines = append(lines, chunk...)
	}
	if len(lines) > count {
		lines = lines[len(lines)-count:]
	}
	return ExecResult{Code: 0, Stdout: strings.Join(lines, "")}
}

// readTailLines 读一个文件尾部至多 limit 字节，按行切分返回（每行含换行符）。
func readTailLines(path string, limit int64) ([]string, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	size := info.Size()
	offset := int64(0)
	if size > limit {
		offset = size - limit
	}
	if _, err := file.Seek(offset, io.SeekStart); err != nil {
		return nil, err
	}
	raw, err := io.ReadAll(file)
	if err != nil {
		return nil, err
	}
	text := string(raw)
	if offset > 0 {
		// 截断处多半落在某行中间：丢掉第一个不完整的片段。
		if idx := strings.IndexByte(text, '\n'); idx >= 0 {
			text = text[idx+1:]
		} else {
			text = ""
		}
	}
	if text == "" {
		return nil, nil
	}
	parts := strings.SplitAfter(text, "\n")
	if last := parts[len(parts)-1]; last == "" {
		parts = parts[:len(parts)-1]
	}
	return parts, nil
}
