// ═══ 更新日志 ═══
// 2026-09-25：CI资产契约改为双架构完整运行包及校验文件，保留旧裸二进制选取的独立兼容测试。
// 2026-09-17：锁定自更新取件契约：按架构挑资产、下载逐字节校验 SHA-256、
//
//	摘要不符时不留残留文件、超大资产直接拒绝。
package hotupdate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	versionpkg "workbuddy2api/internal/version"
)

func TestLatestPicksAssetForThisArchitecture(t *testing.T) {
	want, err := assetName()
	if err != nil {
		t.Skipf("unsupported arch %s", runtime.GOARCH)
	}
	payload := map[string]any{
		"tag_name":     "v9.9.9",
		"name":         "v9.9.9",
		"published_at": "2026-09-17T10:00:00Z",
		"body":         "notes",
		// 资产清单覆盖三端：测试跑在哪个平台上，就得有对应的那份资产。
		// 只列 linux 会让 Windows/安卓上跑测试时找不到本机资产而失败。
		"assets": []map[string]any{
			{"name": "wb2api-linux-amd64", "size": 11, "browser_download_url": "https://example.invalid/amd64",
				"digest": "sha256:aaaa"},
			{"name": "wb2api-linux-arm64", "size": 22, "browser_download_url": "https://example.invalid/arm64",
				"digest": "sha256:bbbb"},
			{"name": "wb2api-windows-amd64", "size": 44, "browser_download_url": "https://example.invalid/win-amd64",
				"digest": "sha256:cccc"},
			{"name": "workbuddy2api-android-arm64.apk", "size": 55, "browser_download_url": "https://example.invalid/android-arm64.apk",
				"digest": "sha256:dddd"},
			{"name": "SHA256SUMS.txt", "size": 33, "browser_download_url": "https://example.invalid/sums"},
		},
	}
	body, _ := json.Marshal(payload)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/"+DefaultRepo+"/releases/latest" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		if got := r.Header.Get("Accept"); !strings.Contains(got, "github") {
			t.Errorf("accept header = %q", got)
		}
		_, _ = w.Write(body)
	}))
	defer server.Close()

	client := NewClient("", "")
	client.APIBase = server.URL
	release, err := client.Latest(context.Background())
	if err != nil {
		t.Fatalf("latest: %v", err)
	}
	if release.Tag != "v9.9.9" || release.AssetName != want {
		t.Fatalf("release = %+v want tag v9.9.9 asset %s", release, want)
	}
	if release.Digest == "" || release.AssetSize == 0 {
		t.Fatalf("digest/size must be carried over: %+v", release)
	}
	if release.PublishedAt.IsZero() || release.Notes != "notes" {
		t.Fatalf("metadata lost: %+v", release)
	}
}

func TestLatestRejectsHTTPError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte("rate limited"))
	}))
	defer server.Close()
	client := NewClient("", "")
	client.APIBase = server.URL
	if _, err := client.Latest(context.Background()); err == nil || !strings.Contains(err.Error(), "403") {
		t.Fatalf("err = %v want an HTTP 403 failure", err)
	}
}

func TestDownloadVerifiesDigest(t *testing.T) {
	content := []byte("#!/bin/sh\necho wb2api\n")
	sum := sha256.Sum256(content)
	digest := hex.EncodeToString(sum[:])
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(content)
	}))
	defer server.Close()
	dir := t.TempDir()
	client := NewClient("", "")
	release := Release{Tag: "v9.9.9", AssetName: "wb2api-linux-amd64", AssetURL: server.URL, AssetSize: int64(len(content))}

	// 摘要不符：必须失败并清掉临时文件，绝不留下一个未校验的二进制。
	bad := release
	bad.Digest = "sha256:" + strings.Repeat("0", 64)
	if _, _, err := client.Download(context.Background(), bad, dir); err == nil {
		t.Fatal("digest mismatch must fail the download")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("failed download left files behind: %v", entries)
	}

	// 摘要正确：落盘且带上可执行位。
	good := release
	good.Digest = "sha256:" + digest
	path, got, err := client.Download(context.Background(), good, dir)
	if err != nil {
		t.Fatalf("download: %v", err)
	}
	if got != digest {
		t.Fatalf("sum = %s want %s", got, digest)
	}
	if filepath.Dir(path) != dir || !strings.Contains(filepath.Base(path), "v9.9.9") {
		t.Fatalf("unexpected target path %s", path)
	}
	saved, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read saved binary: %v", err)
	}
	if string(saved) != string(content) {
		t.Fatalf("saved content mismatch")
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	// Windows 没有可执行位，只在 Unix 上核对。
	if runtime.GOOS != "windows" && info.Mode().Perm()&0o100 == 0 {
		t.Fatalf("downloaded binary must be executable, mode = %v", info.Mode())
	}
}

func TestDownloadRejectsOversizeAsset(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("oversize asset must be rejected before any request")
	}))
	defer server.Close()
	client := NewClient("", "")
	release := Release{Tag: "v9.9.9", AssetName: "wb2api-linux-amd64", AssetURL: server.URL,
		AssetSize: maxReleaseBytes + 1}
	if _, _, err := client.Download(context.Background(), release, t.TempDir()); err == nil {
		t.Fatal("oversize asset must fail")
	}
}

func TestDownloadRequiresAssetURL(t *testing.T) {
	client := NewClient("", "")
	if _, _, err := client.Download(context.Background(), Release{Tag: "v9.9.9"}, t.TempDir()); err == nil {
		t.Fatal("release without an asset must fail")
	}
}

func TestUpdateAvailableComparesVersions(t *testing.T) {
	original := versionpkg.Version
	defer func() { versionpkg.Version = original }()

	versionpkg.Version = "1.2.0"
	cases := []struct {
		tag  string
		want bool
	}{
		{"v1.2.0", false},
		{"1.2.0", false},
		{"v1.3.0", true},
		{"", false},
	}
	for _, item := range cases {
		if got := (Release{Tag: item.tag}).UpdateAvailable(); got != item.want {
			t.Fatalf("UpdateAvailable(%q) = %v want %v", item.tag, got, item.want)
		}
	}
	// 开发构建：远端只要有 tag 就提示可更新。
	versionpkg.Version = "dev"
	if !(Release{Tag: "v1.2.0"}).UpdateAvailable() {
		t.Fatal("dev builds must treat any release as an update")
	}
}

// TestReleaseAssetNamesMatchWorkflow 固定私有 CI 的完整运行包及校验文件输出。
// CI 保存构建 artifact；裸二进制兼容性由 Latest 测试覆盖，不要求 CI 重复产出。
func TestReleaseAssetNamesMatchWorkflow(t *testing.T) {
	workflow, err := os.ReadFile(filepath.Join("..", "..", ".github", "workflows", "build.yml"))
	if err != nil {
		t.Fatalf("cannot verify release workflow: %v", err)
	}
	text := string(workflow)
	for _, name := range []string{"dist/wb2api-runtime-linux-amd64.tar.gz", "dist/wb2api-runtime-linux-arm64.tar.gz", "dist/SHA256SUMS.txt"} {
		if !strings.Contains(text, name) {
			t.Fatalf("build.yml must save the private build artifact %s", name)
		}
	}
}

func TestNewClientDefaultsRepoAndTimeout(t *testing.T) {
	client := NewClient("  ", "token")
	if client.Repo != DefaultRepo {
		t.Fatalf("repo = %q want default", client.Repo)
	}
	if client.Token != "token" || client.HTTP == nil || client.HTTP.Timeout != httpTimeout {
		t.Fatalf("client = %+v", client)
	}
	if client.apiBase() != defaultAPIBase {
		t.Fatalf("api base = %q want %q", client.apiBase(), defaultAPIBase)
	}
	client.APIBase = "http://127.0.0.1:8080/"
	if client.apiBase() != "http://127.0.0.1:8080" {
		t.Fatalf("api base override = %q", client.apiBase())
	}
}

// TestAssetNameFollowsRuntimePlatform 锁住「三端各拿各的资产」。
//
// 安卓的 GOOS 是 "android" 而不是 "linux"（内核是 Linux，但 Go 把它单独标出来）。
// 不按 GOOS 分的话，手机与桌面都会去下 Linux 的二进制——那个在手机上根本跑不起来。
func TestAssetNameFollowsRuntimePlatform(t *testing.T) {
	name, err := assetName()
	if err != nil {
		// 不支持的架构必须报错，且错误里说清是架构问题。
		if !strings.Contains(err.Error(), "unsupported") {
			t.Fatalf("assetName() error = %v; 不支持时错误里应含 unsupported", err)
		}
		return
	}
	want, wantErr := assetNameFor(runtime.GOOS, runtime.GOARCH)
	if wantErr != nil {
		t.Fatalf("assetNameFor(%s,%s) error = %v", runtime.GOOS, runtime.GOARCH, wantErr)
	}
	if name != want {
		t.Fatalf("assetName() = %q，期望 %q（GOOS=%s GOARCH=%s）", name, want, runtime.GOOS, runtime.GOARCH)
	}
}

// TestAssetNamePerPlatform 三端的资产名必须在**任意主机**上都能被断言。
//
// 真事故（2026-10-13）：这条断言原本写成按 runtime.GOOS 分支，安卓那一支期望
// "wb2api-android-<arch>"，而实现早已改成 APK 资产 "workbuddy2api-android-<arch>.apk"。
// 因为该分支只在 GOOS==android 时执行、而本项目从不在安卓上跑 `go test`，这个
// 错误永远暴露不出来——等于安卓这条最容易出问题的路径**完全没有测试**。
//
// 现在断言的是 assetNameFor 这个纯函数，三端一起覆盖，与宿主平台无关。
// 这些名字必须与发布产物一致：linux 见 tools/private_gateway_release_20260925.py，
// windows 见 desktop/build.py，android 见 android/build.py 的 APK 输出名。
func TestAssetNamePerPlatform(t *testing.T) {
	cases := []struct {
		goos, goarch string
		want         string
	}{
		{"linux", "amd64", "wb2api-linux-amd64"},
		{"linux", "arm64", "wb2api-linux-arm64"},
		{"windows", "amd64", "wb2api-windows-amd64"},
		{"android", "arm64", "workbuddy2api-android-arm64.apk"},
	}
	for _, c := range cases {
		got, err := assetNameFor(c.goos, c.goarch)
		if err != nil {
			t.Errorf("assetNameFor(%s,%s) error = %v", c.goos, c.goarch, err)
			continue
		}
		if got != c.want {
			t.Errorf("assetNameFor(%s,%s) = %q，期望 %q", c.goos, c.goarch, got, c.want)
		}
	}
	// 安卓的资产必须以 .apk 结尾：裸二进制在手机上装不起来（SELinux 禁止在
	// 应用私有目录执行），发错了会让更新下载到一个用不了的文件。
	if _, err := assetNameFor("android", "386"); err != nil {
		t.Errorf("assetNameFor(android,386) 不应报错（APK 资产与架构无关）: %v", err)
	}
}

// TestBundleEnabledFollowsPlatform 宿主传给 Manager 的 Bundle 必须与平台一致。
//
// 真机事故（2026-10-13）：安卓上 layout.enabled 为真，Bundle 就被打开了，于是
// 网关把下载到的**裸二进制**当 tar.gz 解包，直接报 `gzip: invalid header`。
// 这个判据由宿主（cmd/server）给出，测试在这里锁住它的取值来源。
func TestBundleEnabledFollowsPlatform(t *testing.T) {
	if runtime.GOOS == "linux" {
		if !BundleSupported() {
			t.Fatal("Linux 必须支持完整运行包")
		}
		return
	}
	if BundleSupported() {
		t.Fatalf("%s 上不得启用完整运行包（会把裸二进制当 tar.gz 解包）", runtime.GOOS)
	}
}

// TestBundleOnlyOnLinux 完整运行包只有 Linux 部署有。
//
// 桌面与安卓用的是单文件资产：桌面版的数据与程序在同一个文件夹里、由启动器管；
// 安卓的程序在 APK 的 nativeLibraryDir 里，「解压到目录再整体替换」的前提不成立。
// 若在这两个平台上仍去要运行包，会去找一个不存在的资产名。
func TestBundleOnlyOnLinux(t *testing.T) {
	if runtime.GOOS == "linux" && !bundleSupported() {
		t.Fatal("Linux 必须支持完整运行包")
	}
	if runtime.GOOS != "linux" && bundleSupported() {
		t.Fatalf("%s 不该走完整运行包路径", runtime.GOOS)
	}
}
