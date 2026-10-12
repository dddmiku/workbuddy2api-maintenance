// ═══ 更新日志 ═══
// 2026-10-13：初版——端到端冒烟：真起一个网关进程（Go 面板引擎），用真 HTTP 打它。
//
// 为什么要有这个测试：单元测试覆盖的是「面板的每个处理函数」，但**引擎选择与挂载**
// 这一段（useGoPanel → startGoPanel → withPanel）只有把真进程跑起来才能验到。
// 它同时验了三件事：
//
//  1. WB2API_PANEL_GO=1 时面板真的挂在 /admin 下、由 Go 引擎服务（不需要 Python）；
//  2. 未登录访问 /admin/ 会重定向到登录页，而 /admin/login 能拿到页面；
//  3. 网关自己的 /healthz 不受影响（面板挂载不能挡住 API）。
//
// 用 `go test` 而不是脚本：这样它能跟着 CI 一起跑，也不会因为「忘了跑脚本」而腐烂。
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// TestGoPanelEndToEnd 起一个真实网关进程并验证 Go 面板已挂载。
func TestGoPanelEndToEnd(t *testing.T) {
	if testing.Short() {
		t.Skip("端到端测试：-short 时跳过")
	}
	root := t.TempDir()
	for _, dir := range []string{"config", "auths", "data", "panel-data"} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0700); err != nil {
			t.Fatal(err)
		}
	}
	config := `{
  "listen": "127.0.0.1:0",
  "api_keys_file": "./data/api_keys.json",
  "auth_dir": "./auths",
  "state_file": "./data/state.json",
  "update": {"enabled": false}
}
`
	configPath := filepath.Join(root, "config", "config.json")
	if err := os.WriteFile(configPath, []byte(config), 0600); err != nil {
		t.Fatal(err)
	}

	port := freePort(t)
	listen := fmt.Sprintf("127.0.0.1:%d", port)
	binary := buildServer(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// #nosec G204 -- 被测二进制由本测试自己构建，参数是固定值
	cmd := exec.CommandContext(ctx, binary, "-config", configPath)
	cmd.Dir = root
	cmd.Env = append(os.Environ(),
		"WB2A_LISTEN="+listen,
		"WB2API_PANEL_ENABLED=1",
		"WB2API_PANEL_GO=1",
		"WB2API_RUNTIME=native",
		"WB2API_GATEWAY_DIR="+root,
		"WB2API_AUTHS_DIR="+filepath.Join(root, "auths"),
		"WB2API_ADMIN_DIR="+filepath.Join(root, "panel-data"),
		"WB2API_CONFIG_PATH="+configPath,
		"WB2API_RUNTIME_DIR="+filepath.Dir(binary),
	)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stderr = cmd.Stdout
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	}()
	logs := &strings.Builder{}
	go func() { _, _ = io.Copy(logs, stdout) }()

	if !waitReady(t, listen, logs) {
		t.Fatalf("网关未就绪。日志：\n%s", logs.String())
	}

	client := &http.Client{Timeout: 5 * time.Second}

	// 1) 网关自身的健康接口必须正常（面板挂载不能挡住 API）。
	health := getJSON(t, client, "http://"+listen+"/healthz")
	if health["service"] != "workbuddy2api" {
		t.Fatalf("/healthz 不对：%v", health)
	}

	// 2) 未登录访问面板根路径 → 重定向到登录页。
	//
	// 这里同时锁住「前缀必须被剥掉」这条：网关挂载在 /admin 下并把**完整路径**交给
	// 面板，而面板内部分支按内部路径写（/login）。不剥的话 /admin/login 匹配不上
	// /login，落到「未登录 → 重定向到 login」，而重定向目标又被浏览器加上前缀，
	// 自己指回自己 —— 症状是登录页反复重定向、永远进不去。
	resp := get(t, client, "http://"+listen+"/admin/", false)
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("/admin/ 应 302，实际 %d", resp.StatusCode)
	}
	if location := resp.Header.Get("Location"); location != "login" {
		t.Fatalf("应重定向到 login，实际 %q", location)
	}
	resp.Body.Close()

	// 3) 登录页必须由 Go 引擎直接服务（不需要 Python 子进程），且不能重定向。
	resp = get(t, client, "http://"+listen+"/admin/login", false)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("/admin/login 应 200，实际 %d（重定向到 %q）",
			resp.StatusCode, resp.Header.Get("Location"))
	}
	if !strings.Contains(strings.ToLower(string(body)), "<html") {
		t.Fatalf("登录页不是 HTML：%s", truncate(string(body), 200))
	}
	if !strings.Contains(string(body), "wb2a_admin") && !strings.Contains(string(body), "password") {
		t.Fatalf("登录页内容不对（应含登录表单）：%s", truncate(string(body), 400))
	}

	// 3b) 面板挂载前缀下的任意内部路径都必须被正确路由，不能互相串。
	//
	// 这一组锁的是「前缀剥离」：/admin/api/... 必须落到 /api/... 的处理函数，
	// 而不是落到登录重定向。写错的表现是登录页反复重定向（自己指回自己）。
	for _, probe := range []struct {
		path string
		want int
	}{
		{"/admin/api/state", http.StatusUnauthorized},
		{"/admin/api/keys", http.StatusUnauthorized},
		{"/admin/vendor/qrcode.js", http.StatusFound}, // 未登录：与 Python 版同序，先重定向
		{"/admin/nope", http.StatusFound},
	} {
		resp = get(t, client, "http://"+listen+probe.path, false)
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode != probe.want {
			t.Fatalf("%s 应 %d，实际 %d（重定向到 %q）",
				probe.path, probe.want, resp.StatusCode, resp.Header.Get("Location"))
		}
	}
	// 重定向目标必须是相对路径 "login"：带前缀的绝对路径会让浏览器拼出
	// /admin/login 之外的地址，而面板自己又会再加一层前缀。
	resp = get(t, client, "http://"+listen+"/admin/", false)
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if location := resp.Header.Get("Location"); location != "login" {
		t.Fatalf("重定向目标应为 login，实际 %q", location)
	}

	// 4) 未登录访问面板 API → 401 JSON（前端靠它跳登录页）。
	resp = get(t, client, "http://"+listen+"/admin/api/state", false)
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("/admin/api/state 应 401，实际 %d：%s", resp.StatusCode, body)
	}
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("401 必须是 JSON：%s", body)
	}
	if payload["message"] == nil {
		t.Fatalf("401 必须带 message：%v", payload)
	}

	// 5) 确认走的是 Go 引擎：日志里应有它自己的启动行，且**没有** Python 相关行。
	logText := logs.String()
	if !strings.Contains(logText, "Go 版面板引擎") {
		t.Fatalf("应走 Go 引擎。日志：\n%s", logText)
	}
	if strings.Contains(logText, "python") || strings.Contains(logText, "wb2panel-") {
		t.Fatalf("不该启动 Python 面板。日志：\n%s", logText)
	}
}

// buildServer 构建网关本体与它需要的辅助程序，放进同一个临时目录。
//
// 必须把辅助程序一起建出来：网关启动时会做完整性校验（缺一个就拒绝启动），
// 而这正是桌面/安卓分发包的真实形态——辅助程序与网关同目录（安卓上是
// nativeLibraryDir，由 WB2API_HELPER_DIR 指定）。只建网关会让测试卡在校验上，
// 而不是卡在真正要验的东西上。
func buildServer(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	suffix := ""
	if runtime.GOOS == "windows" {
		suffix = ".exe"
	}
	targets := []struct{ name, pkg string }{
		{"wb2api-test", "."},
		{"login", "../../cmd/login"},
		{"credit", "../../cmd/credit"},
		{"fission_bin", "../../cmd/fission"},
	}
	output := ""
	for _, target := range targets {
		path := filepath.Join(dir, target.name+suffix)
		cmd := exec.Command("go", "build", "-o", path, target.pkg)
		cmd.Dir = repoDir(t)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("构建 %s 失败：%v\n%s", target.name, err, out)
		}
		if target.name == "wb2api-test" {
			output = path
		}
	}
	return output
}

// repoDir 定位 cmd/server 目录（测试从它自己的位置往上找）。
func repoDir(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

func freePort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	return listener.Addr().(*net.TCPAddr).Port
}

// waitReady 等网关开始监听。
//
// 判据是「能收到 HTTP 响应」，不是「/healthz 返回 200」：/healthz 在一个账号都没有
// 时返回 503（池里没有可服务的号），而本测试用的就是空账号目录。要求 200 会让测试
// 永远等不到就绪——那是测试自己的判据错了，不是网关没起来。
func waitReady(t *testing.T, listen string, logs *strings.Builder) bool {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	client := &http.Client{Timeout: time.Second}
	for time.Now().Before(deadline) {
		resp, err := client.Get("http://" + listen + "/healthz")
		if err == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			return true
		}
		time.Sleep(200 * time.Millisecond)
	}
	return false
}

func get(t *testing.T, client *http.Client, url string, follow bool) *http.Response {
	t.Helper()
	clone := *client
	if !follow {
		// 不自动跟随重定向：要断言的就是重定向本身。
		clone.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	}
	resp, err := clone.Get(url)
	if err != nil {
		t.Fatalf("GET %s 失败：%v", url, err)
	}
	return resp
}

func getJSON(t *testing.T, client *http.Client, url string) map[string]any {
	t.Helper()
	resp, err := client.Get(url)
	if err != nil {
		t.Fatalf("GET %s 失败：%v", url, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("响应不是 JSON：%s", body)
	}
	return payload
}

func truncate(text string, n int) string {
	if len(text) <= n {
		return text
	}
	return text[:n]
}
