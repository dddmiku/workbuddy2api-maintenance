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
	"strconv"
	"strings"
	"testing"
	"time"
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

// ═══ 2026-10-15：镜像链 + 空闲超时 ═══════════════════════════════════════════

// TestDefaultProxyMirrorsExcludeDeadHost 默认镜像链里不能出现已失效的域名。
//
// 实测背景：`gh-proxy.com` 的下载转发整体 403（连它自己文档的示例 URL 都 403），
// 而它当时正是 DefaultProxyBase。留在这里的后果是每次热更新都白等一次 403，
// 在慢链路上（板子直连 15KB/s）回落直连等于热更新直接不可用。
func TestDefaultProxyMirrorsExcludeDeadHost(t *testing.T) {
	for _, m := range defaultProxyMirrors {
		if strings.Contains(m, "gh-proxy.com") {
			t.Errorf("镜像链里不该有已失效的 gh-proxy.com：%s", m)
		}
		if !strings.HasPrefix(m, "https://") {
			t.Errorf("镜像必须走 https：%s", m)
		}
	}
	if strings.Contains(DefaultProxyBase, "gh-proxy.com") {
		t.Errorf("默认代理不该指向 gh-proxy.com（下载转发已 403）：%s", DefaultProxyBase)
	}
	if len(defaultProxyMirrors) < 2 {
		t.Errorf("镜像链至少要两个节点，否则单点故障等于没有备份：%v", defaultProxyMirrors)
	}
}

// TestDownloadURLsIncludesMirrors 默认场景下候选应覆盖全部镜像，且直连在最后。
func TestDownloadURLsIncludesMirrors(t *testing.T) {
	const asset = "https://github.com/o/r/releases/download/v1/a"
	c := &Client{ProxyMirrors: append([]string(nil), defaultProxyMirrors...)}
	got := c.downloadURLs(asset)
	if len(got) != len(defaultProxyMirrors)+1 {
		t.Fatalf("候选数 = %d，期望 %d（镜像 + 直连）: %v", len(got), len(defaultProxyMirrors)+1, got)
	}
	if got[len(got)-1] != asset {
		t.Errorf("直连必须是最后一条兜底，实际末条 = %s", got[len(got)-1])
	}
	for i, m := range defaultProxyMirrors {
		if got[i] != m+"/"+asset {
			t.Errorf("第 %d 条 = %s，期望 %s", i, got[i], m+"/"+asset)
		}
	}
}

// TestDownloadURLsDedupes 重复的镜像与「与直连相同」的条目都要去掉，
// 否则会把同一个地址试两遍、白等一次超时。
func TestDownloadURLsDedupes(t *testing.T) {
	const asset = "https://github.com/o/r/releases/download/v1/a"
	c := &Client{ProxyMirrors: []string{"https://a.example", "https://a.example/", "https://b.example"}}
	got := c.downloadURLs(asset)
	if len(got) != 3 {
		t.Fatalf("去重后候选数 = %d，期望 3: %v", len(got), got)
	}
	if got[0] != "https://a.example/"+asset || got[1] != "https://b.example/"+asset {
		t.Fatalf("去重结果不对: %v", got)
	}
}

// TestExplicitProxyHasNoMirrors 用户显式配了代理就只认那一个，不擅自加节点。
func TestExplicitProxyHasNoMirrors(t *testing.T) {
	const asset = "https://github.com/o/r/releases/download/v1/a"
	m := NewManager(Options{Proxy: "https://my-proxy.example"})
	if len(m.client.ProxyMirrors) != 0 {
		t.Fatalf("显式代理不该带镜像链: %v", m.client.ProxyMirrors)
	}
	if got := m.client.downloadURLs(asset); len(got) != 2 || got[0] != "https://my-proxy.example/"+asset {
		t.Fatalf("显式代理候选 = %v", got)
	}
	// 默认（空）时才装镜像链。
	if dm := NewManager(Options{Proxy: ""}); len(dm.client.ProxyMirrors) != len(defaultProxyMirrors) {
		t.Fatalf("默认应装镜像链，实际 %v", dm.client.ProxyMirrors)
	}
	// 显式直连也不该装。
	if off := NewManager(Options{Proxy: "-"}); len(off.client.ProxyMirrors) != 0 {
		t.Fatalf(`"-" 直连不该带镜像链: %v`, off.client.ProxyMirrors)
	}
}

// TestIdleWatchdogFiresOnStall 卡住的读（永不返回）应在空闲超时后被取消，
// 而不是一直挂着。这是板子「慢链路被 60s 总超时误杀」的反面：真正该判死的是
// **长时间收不到字节**，看门狗正是这个判据。
func TestIdleWatchdogFiresOnStall(t *testing.T) {
	// handler 必须随请求上下文退出，否则 httptest.Close() 会一直等在途请求
	// （Close 会等所有 handler 返回，而 handler 卡住就成了死锁——写这条用例时
	// 实测挂满 600s 才被发现）。
	stalled := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "1024")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("x")) // 先给一个字节，之后卡住
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		<-r.Context().Done() // 客户端取消（看门狗触发）时才返回
	}))
	defer stalled.Close()

	c := NewClient("", "")
	c.HTTP = &http.Client{} // 去掉 client 级 Timeout，只留看门狗在起作用
	// 把看门狗超时压到很短，避免测试真的等 30s。
	old := assetIdleTimeout
	assetIdleTimeout = 300 * time.Millisecond
	defer func() { assetIdleTimeout = old }()

	start := time.Now()
	_, err := c.fetchAsset(context.Background(), stalled.URL, filepath.Join(t.TempDir(), "a"), 1024, "", nil)
	if err == nil {
		t.Fatal("卡住的下载应当失败，实际成功了")
	}
	if !strings.Contains(err.Error(), "无数据") {
		t.Fatalf("错误应说明是无进展，实际 %v", err)
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("看门狗没生效，等了 %v", elapsed)
	}
}

// TestIdleWatchdogAllowsSlowButAlive 慢但**持续有字节**的下载不该被误杀。
//
// 这正是原实现的问题：60s 总超时对 19MB 资产等于要求 ≥320KB/s，板子这类慢链路
// 必然失败（实测只下了 652KB 就报 context deadline exceeded）。分块慢发时，
// 只要每块间隔小于空闲超时，就该完整下完。
func TestIdleWatchdogAllowsSlowButAlive(t *testing.T) {
	content := []byte(strings.Repeat("payload!", 4096)) // 32KB
	sum := sha256.Sum256(content)
	digest := hex.EncodeToString(sum[:])

	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", strconv.Itoa(len(content)))
		w.WriteHeader(http.StatusOK)
		chunk := len(content) / 8
		for off := 0; off < len(content); off += chunk {
			end := off + chunk
			if end > len(content) {
				end = len(content)
			}
			_, _ = w.Write(content[off:end])
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			time.Sleep(50 * time.Millisecond) // 远小于空闲超时
		}
	}))
	defer slow.Close()

	old := assetIdleTimeout
	assetIdleTimeout = 2 * time.Second
	defer func() { assetIdleTimeout = old }()

	c := NewClient("", "")
	target := filepath.Join(t.TempDir(), "slow")
	got, err := c.fetchAsset(context.Background(), slow.URL, target, int64(len(content)), digest, nil)
	if err != nil {
		t.Fatalf("慢但活着下载应当成功: %v", err)
	}
	if got != digest {
		t.Fatalf("摘要 = %s，期望 %s", got, digest)
	}
}
