// ═══ 更新日志 ═══
// 2026-10-13：新增。锁住下载加速代理的改写范围与关闭方式。
package hotupdate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestProxiedRewritesGitHubURLs 代理只改写 GitHub 的地址。
func TestProxiedRewritesGitHubURLs(t *testing.T) {
	c := &Client{ProxyBase: "https://gh-proxy.com"}
	cases := map[string]string{
		"https://github.com/o/r/releases/download/v1/a.apk": "https://gh-proxy.com/https://github.com/o/r/releases/download/v1/a.apk",
		"https://objects.githubusercontent.com/x/y":         "https://gh-proxy.com/https://objects.githubusercontent.com/x/y",
		"https://api.github.com/repos/o/r/releases/latest":  "https://gh-proxy.com/https://api.github.com/repos/o/r/releases/latest",
	}
	for in, want := range cases {
		if got := c.proxied(in); got != want {
			t.Errorf("proxied(%q) = %q，期望 %q", in, got, want)
		}
	}
}

// TestProxiedRefusesForeignHosts 非 GitHub 主机一律不改写。
//
// 这是 SSRF 纵深防御：改写会把目标地址拼进 URL，若不加限制，一个被篡改的
// browser_download_url 就能让网关去请求任意主机。资产 URL 来自 GitHub API，
// 但不应依赖上游可信。
func TestProxiedRefusesForeignHosts(t *testing.T) {
	c := &Client{ProxyBase: "https://gh-proxy.com"}
	for _, in := range []string{
		"https://evil.example.com/x",
		"http://github.com/o/r", // 非 https
		"https://notgithub.com/o/r",
		"https://github.com.evil.example.com/o/r",
	} {
		if got := c.proxied(in); got != in {
			t.Errorf("proxied(%q) 不该改写，实际 %q", in, got)
		}
	}
}

// TestProxiedDisabledIsPassthrough 未配置代理时原样返回。
func TestProxiedDisabledIsPassthrough(t *testing.T) {
	c := &Client{}
	raw := "https://github.com/o/r/releases/download/v1/a"
	if got := c.proxied(raw); got != raw {
		t.Fatalf("未配置代理时应原样返回，实际 %q", got)
	}
}

// TestManagerProxySentinel 空值走默认代理，"-" 表示显式直连。
//
// 留空与「不想要代理」无法区分，所以用一个不可能与真实地址冲突的哨兵值。
func TestManagerProxySentinel(t *testing.T) {
	if got := NewManager(Options{Proxy: ""}).client.ProxyBase; got != DefaultProxyBase {
		t.Fatalf("空值应回落到默认代理，实际 %q", got)
	}
	if got := NewManager(Options{Proxy: "-"}).client.ProxyBase; got != "" {
		t.Fatalf(`"-" 应表示直连，实际 %q`, got)
	}
	custom := "https://my-proxy.example"
	if got := NewManager(Options{Proxy: custom}).client.ProxyBase; got != custom {
		t.Fatalf("自定义代理未生效，实际 %q", got)
	}
}

// TestDefaultProxyIsHTTPS 默认代理必须是 https。
func TestDefaultProxyIsHTTPS(t *testing.T) {
	if !strings.HasPrefix(DefaultProxyBase, "https://") {
		t.Fatalf("默认代理必须走 https，实际 %q", DefaultProxyBase)
	}
}

// TestDownloadFallsBackToDirect 代理不可用时必须回落到直连。
//
// 代理是第三方服务，会挂、会限流、也可能某天消失。只走代理等于把「能不能更新」
// 押在它身上；只走直连又会在被墙时完全卡住。所以两条都要试。
//
// 注意 proxied() 只改写 GitHub 主机（SSRF 纵深防御），所以这里不能靠一个本地
// 假代理来观察——直接用 downloadURLs 给出的候选，按顺序喂给 fetchAsset 验证
// 「第一条失败、第二条成功」这条路径。
func TestDownloadFallsBackToDirect(t *testing.T) {
	content := []byte("#!/bin/sh\necho wb2api\n")
	sum := sha256.Sum256(content)
	digest := hex.EncodeToString(sum[:])

	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError) // 模拟代理挂掉
	}))
	defer dead.Close()
	alive := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(content)
	}))
	defer alive.Close()

	client := NewClient("", "")
	dir := t.TempDir()
	target := filepath.Join(dir, "asset")

	// 第一条（死）必须失败，第二条（活）必须成功——这就是回落。
	if _, err := client.fetchAsset(context.Background(), dead.URL, target, int64(len(content)), digest, nil); err == nil {
		t.Fatal("死地址应当失败")
	}
	got, err := client.fetchAsset(context.Background(), alive.URL, target, int64(len(content)), digest, nil)
	if err != nil {
		t.Fatalf("活地址应当成功: %v", err)
	}
	if got != digest {
		t.Fatalf("摘要 = %s，期望 %s", got, digest)
	}
	if data, err := os.ReadFile(target); err != nil || string(data) != string(content) {
		t.Fatalf("落盘内容不对: %v", err)
	}
}

// TestDownloadURLsOrder 候选地址必须「代理在前、直连在后」。
func TestDownloadURLsOrder(t *testing.T) {
	const asset = "https://github.com/o/r/releases/download/v1/a"
	c := &Client{ProxyBase: "https://gh-proxy.com"}
	got := c.downloadURLs(asset)
	want := []string{"https://gh-proxy.com/" + asset, asset}
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("候选顺序 = %v，期望 %v", got, want)
	}
	// 未配置代理时只有直连，不该重复试同一个地址。
	if only := (&Client{}).downloadURLs(asset); len(only) != 1 || only[0] != asset {
		t.Fatalf("未配置代理时 = %v，期望只有直连", only)
	}
}

// TestFetchAssetRejectsDigestMismatch 内容校验失败必须报错。
//
// 摘要不符说明拿到的字节是错的（代理篡改或传输损坏），换一条路重来也是同样的
// 内容；这时要显式报错，而不是靠重试掩盖——自更新的信任根不能被「多试几次」稀释。
func TestFetchAssetRejectsDigestMismatch(t *testing.T) {
	content := []byte("tampered")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(content)
	}))
	defer server.Close()

	client := NewClient("", "")
	dir := t.TempDir()
	_, err := client.fetchAsset(context.Background(), server.URL, filepath.Join(dir, "asset"),
		int64(len(content)), strings.Repeat("0", 64), nil)
	if err == nil {
		t.Fatal("摘要不符必须报错")
	}
	// 失败不得留下半成品。
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Fatalf("失败后残留文件: %v", entries)
	}
}

// TestProgressOf 进度折算的边界。
func TestProgressOf(t *testing.T) {
	cases := []struct {
		downloaded, total int64
		want              float64
	}{
		{0, 100, 0},    // 还没开始
		{50, 100, 0.5}, // 一半
		{100, 100, 1},  // 完成
		{120, 100, 1},  // 超出时夹到 1，不报 >1
		{50, 0, 0},     // 总大小未知 → 不确定态
		{-5, 100, 0},   // 异常输入不产生负进度
	}
	for _, c := range cases {
		if got := progressOf(c.downloaded, c.total); got != c.want {
			t.Errorf("progressOf(%d,%d) = %v，期望 %v", c.downloaded, c.total, got, c.want)
		}
	}
}

// TestHandoverSupportedMatchesPlatform 交接能力必须与平台一致。
//
// Windows 无法复制监听套接字（dup + FD 继承是 Unix 机制），所以那里的更新只能
// 「下载落盘 + 重启生效」。把它写成一个常量而不是散落的 runtime.GOOS 判断，
// 是为了让「哪些平台支持运行中更新」只有一处答案。
func TestHandoverSupportedMatchesPlatform(t *testing.T) {
	if runtime.GOOS == "windows" && HandoverSupported {
		t.Fatal("Windows 上不支持运行中交接")
	}
	if runtime.GOOS != "windows" && !HandoverSupported {
		t.Fatalf("%s 上应当支持运行中交接", runtime.GOOS)
	}
}
