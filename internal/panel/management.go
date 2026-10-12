// ═══ 更新日志 ═══
// 2026-10-13：初版——经本机管理 socket 调用网关内部接口（与 panel/key_management.py 同口径）。
//
// 面板能用管理通道，是因为它与网关跑在**同一台机器**上、且 socket 文件权限 0600：
// 这是「本机可信」这一条边界的物化。网关的内部接口（/keys、/usage、/update、
// /accounts/revive、/runtime/reload …）都只挂在这个 socket 上，普通调用密钥看不到。
//
// 为什么面板不直接用 config.json 里的 api_key 走 HTTP：那把密钥是可分发的（要发给
// 调用方），把它当管理凭证用意味着「任何一个拿到调用密钥的人都能改密钥库」。管理
// 能力必须走一条调用方拿不到的通道。
package panel

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// managementResponseLimit 管理接口响应上限，与 key_management.py 一致（8 MiB）。
//
// 用量快照含每把密钥 120 天日数据，密钥多时会超过 1 MiB；历史上这里曾是 1 MiB，
// 结果密钥一多整个用量页就整体失效。放宽到 8 MiB 并保留上限，防止一个坏响应
// 把面板的内存吃光。
const managementResponseLimit = 8 << 20

// DefaultAPIKeysFile 密钥库默认位置，必须与 cmd/server 的默认值一致。
//
// 面板读的是**原始** config.json，网关读的是归一化后的配置；两边默认值一旦分叉，
// 就会出现「网关已启用、面板却报未启用」这种互相矛盾的状态。
const DefaultAPIKeysFile = "./data/api_keys.json"

// ManagementClient 访问网关内部接口。
type ManagementClient struct {
	// Handler 是**同进程内**的网关管理接口。
	//
	// 优先用它，理由有三：
	//   1. 少一层 socket，就少一类故障（路径解析、权限、残留文件、就绪时序）；
	//   2. Windows 上 Go 的 net.Dial("unix", …) 直接失败（实测 connect: invalid
	//      argument），桌面版经 socket 调管理接口**一个都调不通**——密钥页、用量页、
	//      热更新页会全挂。同进程分发没有这个问题。
	//   3. 安卓上同样没有 Unix socket 的可靠支持。
	//
	// Handler 为空时才退到 socket 路径（面板与网关分进程部署的形态）。
	Handler http.Handler
	// SocketPath 是本机管理 socket 的绝对路径；空串表示「密钥管理未启用」。
	SocketPath string
	// BaseDir 用于把配置里的 /app/… 路径映射回宿主/桌面目录。
	BaseDir string
	// Transport 可被测试替换（unix dial / 回环 TCP）。
	Transport http.RoundTripper
	// Timeout 单次调用超时；0 表示 15 秒。
	Timeout time.Duration
}

// SocketPath 解析生效的管理 socket 路径。
//
// 返回空串表示多密钥管理被显式关闭——与网关的 applyAPIKeysDefault 同一口径：
//   - 显式 api_keys_enabled=false → 关闭；
//   - 写了路径 → 用它；
//   - 其余（没写、写空串、null）→ 默认路径。
//
// 空串按「没配」处理是有意的：历史 config.example.json 里这一项就是空串，照抄它的
// 部署很多，把它当成关闭信号正是「密钥管理尚未启用」这条提示的来源。
func ResolveSocketPath(configPath, baseDir string) (string, error) {
	object, err := readConfigObject(configPath)
	if err != nil {
		return "", err
	}
	if value, ok := object.Get("api_keys_enabled"); ok {
		if flag, isBool := value.(bool); isBool && !flag {
			return "", nil
		}
	}
	registry := DefaultAPIKeysFile
	if value, ok := object.Get("api_keys_file"); ok {
		if text, isText := value.(string); isText && strings.TrimSpace(text) != "" {
			registry = text
		}
	}
	path := ""
	if value, ok := object.Get("api_keys_socket"); ok {
		if text, isText := value.(string); isText && strings.TrimSpace(text) != "" {
			path = text
		}
	}
	if path == "" {
		path = filepath.Join(filepath.Dir(registry), "api_keys.sock")
	}
	// 容器里的配置常写成 /app/data/...（镜像内路径），但面板可能把网关目录挂到
	// 别处。按 base 前缀映射回来，否则面板会去连一个容器里才存在的路径。
	if strings.HasPrefix(path, "/app/") {
		path = filepath.Join(baseDir, path[len("/app/"):])
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(baseDir, path)
	}
	return filepath.Clean(path), nil
}

// ManagementEnabled 判断是否有可用的管理通道（面板据此显示密钥页）。
func (c *ManagementClient) ManagementEnabled() bool {
	return c.Handler != nil || c.SocketPath != ""
}

// Request 调用管理接口，返回 (HTTP 状态码, 解析后的 JSON 对象)。
//
// 失败一律映射成 503 + 可读中文：面板的按钮只关心「有没有成功」和「为什么没成功」，
// 把 Go 的底层错误原文抛给用户没有意义。
func (c *ManagementClient) Request(ctx context.Context, method, endpoint string, body any) (int, map[string]any) {
	if c.Handler != nil {
		return c.requestInProcess(ctx, method, endpoint, body)
	}
	if c.SocketPath == "" {
		return 503, map[string]any{"ok": false, "message": "密钥管理尚未启用"}
	}
	var payload io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return 503, map[string]any{"ok": false, "message": "暂时无法连接密钥管理服务，请稍后刷新"}
		}
		payload = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://localhost"+endpoint, payload)
	if err != nil {
		return 503, map[string]any{"ok": false, "message": "暂时无法连接密钥管理服务，请稍后刷新"}
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	client := &http.Client{Transport: c.transport(), Timeout: c.timeout()}
	resp, err := client.Do(req)
	if err != nil {
		return 503, map[string]any{"ok": false, "message": "暂时无法连接密钥管理服务，请稍后刷新"}
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, managementResponseLimit+1))
	if err != nil || len(raw) > managementResponseLimit {
		return 503, map[string]any{"ok": false, "message": "暂时无法连接密钥管理服务，请稍后刷新"}
	}
	var result map[string]any
	if err := json.Unmarshal(raw, &result); err != nil {
		return 503, map[string]any{"ok": false, "message": "暂时无法连接密钥管理服务，请稍后刷新"}
	}
	return resp.StatusCode, result
}

// requestInProcess 直接把请求交给同进程的管理 handler。
//
// 用 httptest.NewRecorder 而不是真的起一个本地监听：面板与网关在同一个进程里，
// 中间隔一层 loopback TCP 只是白白多两次上下文切换与一次端口占用。
func (c *ManagementClient) requestInProcess(ctx context.Context, method, endpoint string, body any) (int, map[string]any) {
	var payload io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return 503, map[string]any{"ok": false, "message": "暂时无法连接密钥管理服务，请稍后刷新"}
		}
		payload = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://internal"+endpoint, payload)
	if err != nil {
		return 503, map[string]any{"ok": false, "message": "暂时无法连接密钥管理服务，请稍后刷新"}
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	recorder := httptest.NewRecorder()
	c.Handler.ServeHTTP(recorder, req)
	result := recorder.Result()
	defer result.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(result.Body, managementResponseLimit+1))
	if err != nil || len(raw) > managementResponseLimit {
		return 503, map[string]any{"ok": false, "message": "暂时无法连接密钥管理服务，请稍后刷新"}
	}
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return 503, map[string]any{"ok": false, "message": "暂时无法连接密钥管理服务，请稍后刷新"}
	}
	return result.StatusCode, decoded
}

func (c *ManagementClient) transport() http.RoundTripper {
	if c.Transport != nil {
		return c.Transport
	}
	socket := c.SocketPath
	return &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", socket)
		},
		MaxIdleConns:          8,
		MaxIdleConnsPerHost:   8,
		IdleConnTimeout:       30 * time.Second,
		ResponseHeaderTimeout: 4 * time.Minute,
	}
}

func (c *ManagementClient) timeout() time.Duration {
	if c.Timeout > 0 {
		return c.Timeout
	}
	return 15 * time.Second
}

// ResolveGatewayBinary 在运行目录里找一个辅助程序，按平台补后缀。
//
// Windows 上辅助程序是 login.exe / credit.exe，Linux/Android 上无后缀。
// 判据用 GOOS 而不是「文件是否存在」：后者会让 Linux 上恰好存在同名 .exe 的文件时误判。
func ResolveGatewayBinary(runtimeDir, name string) string {
	return filepath.Join(runtimeDir, platformExecutableName(name))
}

// fileExists 判断路径存在且不是目录。
func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

// errorMessage 从网关的错误信封里取出可读文案。
//
// 网关失败走 OpenAI 风格信封：{"error":{"code":…,"message":…}}；面板自己的接口则是
// 平铺的 {"message":…}。两处都要看，否则「连不上网关」会被显示成空消息。
func errorMessage(payload map[string]any) string {
	if payload == nil {
		return ""
	}
	if text, ok := payload["message"].(string); ok && strings.TrimSpace(text) != "" {
		return text
	}
	if nested, ok := payload["error"].(map[string]any); ok {
		if text, ok := nested["message"].(string); ok {
			return text
		}
	}
	return ""
}

// gatewayEnvelopeError 把「网关未响应」这一类失败统一成面板文案。
func gatewayEnvelopeError(payload map[string]any) string {
	if message := errorMessage(payload); message != "" {
		return message
	}
	return "网关未响应（旧版本网关请先升级）"
}

// jsonObject 把 any 断言成 JSON 对象；不是对象时返回 nil（调用方按缺省处理）。
func jsonObject(value any) map[string]any {
	object, _ := value.(map[string]any)
	return object
}

// jsonNumber 把 JSON 数字宽松转成 float64；转不了返回 (0,false)。
func jsonNumber(value any) (float64, bool) {
	switch v := value.(type) {
	case float64:
		return v, true
	case int:
		return float64(v), true
	case int64:
		return float64(v), true
	case json.Number:
		parsed, err := v.Float64()
		return parsed, err == nil
	}
	return 0, false
}

// jsonBool 取布尔值；缺省或类型不符时返回 fallback。
func jsonBool(value any, fallback bool) bool {
	flag, ok := value.(bool)
	if !ok {
		return fallback
	}
	return flag
}

// jsonString 取字符串值；缺省或类型不符时返回空串。
func jsonString(value any) string {
	text, _ := value.(string)
	return text
}

// formatSeconds 把耗时格式化成一位小数，供「重启耗时 Ns」这类文案使用。
func formatSeconds(value float64) string {
	return fmt.Sprintf("%.1f", value)
}
