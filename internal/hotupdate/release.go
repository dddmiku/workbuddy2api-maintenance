// ═══ 更新日志 ═══
// 2026-09-26：最新发布查询同时定位签名清单资产，供验签使用。
// 2026-09-25：私有资产通过GitHub API下载，令牌仅发送到同仓库API；统一运行包要求可信摘要。
// 2026-09-24：下载前严格校验摘要格式，拒绝 sha256: 空摘要绕过校验并覆盖更新文件。
// 2026-09-24：稳定版本按数字顺序判断更新，避免旧 Release 被重新标为 latest 后误触自动降级。
// 2026-09-18：发布名称编码为单个文件名，下载使用独占临时文件并在校验后设可执行位，防止越界和符号链接覆盖。
// 2026-09-17：新增自更新取件：查 GitHub Release 最新版本、按架构挑二进制并校验
//
//	SHA-256，供管理台一键热更新使用。
package hotupdate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"workbuddy2api/internal/version"
)

const (
	// DefaultRepo 默认发布仓库（可用 config update.repo 覆盖）。
	DefaultRepo = "dddmiku/workbuddy2api-maintenance"
	// defaultAPIBase GitHub API 根地址；测试用它指向本地假服务。
	defaultAPIBase = "https://api.github.com"
	// maxReleaseBytes 单个下载对象上限，防超大文件把磁盘写满。
	maxReleaseBytes = 128 << 20
	// httpTimeout 查版本/下载的超时。
	httpTimeout = 60 * time.Second
)

// Release 一个候选版本。
type Release struct {
	Tag         string    `json:"tag"`
	Name        string    `json:"name"`
	PublishedAt time.Time `json:"published_at"`
	Notes       string    `json:"notes"`
	AssetURL    string    `json:"asset_url"`
	AssetAPIURL string    `json:"asset_api_url,omitempty"`
	AssetName   string    `json:"asset_name"`
	AssetSize   int64     `json:"asset_size"`
	Digest      string    `json:"digest"` // 形如 "sha256:xxxx"，发布端未提供时为空
	// 签名清单（SHA256SUMS.txt 及其 ed25519 签名）的下载地址；缺失即拒绝更新。
	SumsAPIURL    string `json:"sums_api_url,omitempty"`
	SumsURL       string `json:"sums_url,omitempty"`
	SumsSigAPIURL string `json:"sums_sig_api_url,omitempty"`
	SumsSigURL    string `json:"sums_sig_url,omitempty"`
}

// UpdateAvailable 按稳定版本号判断是否可自动升级；其他自定义标签沿用名称比较。
func (r Release) UpdateAvailable() bool {
	current := strings.TrimSpace(version.Version)
	if r.Tag == "" {
		return false
	}
	if current == "" || current == "dev" {
		// 开发构建：只要远端有 tag 就认为可更新，交由使用者判断。
		return true
	}
	if candidate, ok := stableVersion(r.Tag); ok {
		if running, ok := stableVersion(current); ok {
			for i := range candidate {
				if candidate[i] != running[i] {
					return candidate[i] > running[i]
				}
			}
			return false
		}
	}
	return strings.TrimPrefix(r.Tag, "v") != strings.TrimPrefix(current, "v")
}

func stableVersion(tag string) ([3]uint64, bool) {
	var out [3]uint64
	parts := strings.Split(strings.TrimPrefix(strings.TrimSpace(tag), "v"), ".")
	if len(parts) != len(out) {
		return out, false
	}
	for i, part := range parts {
		value, err := strconv.ParseUint(part, 10, 64)
		if err != nil {
			return out, false
		}
		out[i] = value
	}
	return out, true
}

// Client 查版本 / 下载用。
type Client struct {
	Bundle bool // Select a complete runtime instead of a standalone server.
	Repo   string
	HTTP   *http.Client
	Token  string // 私有仓库时用；公开仓库留空即可
	// APIBase 覆盖 GitHub API 根地址（测试用）；空 = 官方地址。
	APIBase string
	// ProxyBase 是下载加速代理的前缀（如 https://gh-proxy.com）。
	//
	// 空 = 直连 GitHub。设置后，**资产下载**改走 <ProxyBase>/<原始 GitHub 地址>。
	//
	// 为什么只代理下载、不代理 API 查询：查询要带 Authorization 头（私有仓库场景），
	// 而把令牌发给第三方代理等于把它交出去。下载的是公开的发行资产、且**必须通过
	// 签名校验**才会被安装（见 manager.go 的验签步骤），所以经过代理不降低信任级别
	// ——代理能改字节，但改不过签名。
	ProxyBase string
}

// DefaultProxyBase 默认的下载加速代理。
//
// 国内直连 GitHub 的 releases 经常超时或极慢，热更新因此形同虚设。用户
// 2026-10-13 明确要求「更新都走 gh-proxy 加速」。
//
// 仍可用 config update.proxy 覆盖（设为空串即恢复直连）。
const DefaultProxyBase = "https://gh-proxy.com"

// NewClient 构造取件客户端；repo 为空回落默认仓库。
func NewClient(repo, token string) *Client {
	if strings.TrimSpace(repo) == "" {
		repo = DefaultRepo
	}
	return &Client{Repo: repo, Token: token, HTTP: &http.Client{Timeout: httpTimeout}}
}

// proxied 把 GitHub 地址改写成经加速代理的地址；未配置代理时原样返回。
//
// 只对 github.com / *.githubusercontent.com 生效：这条改写会把目标地址拼进 URL，
// 若不加限制，一个被篡改的 browser_download_url 就能让网关去请求任意主机
// （SSRF 面）。release.go 里的资产 URL 来自 GitHub API，但纵深防御不依赖上游可信。
func (c *Client) proxied(raw string) string {
	base := strings.TrimRight(strings.TrimSpace(c.ProxyBase), "/")
	if base == "" || raw == "" {
		return raw
	}
	target, err := url.Parse(raw)
	if err != nil || target.Scheme != "https" {
		return raw
	}
	host := strings.ToLower(target.Hostname())
	if host != "github.com" && host != "api.github.com" && host != "objects.githubusercontent.com" &&
		!strings.HasSuffix(host, ".githubusercontent.com") {
		return raw
	}
	return base + "/" + raw
}

// apiBase 返回生效的 API 根地址。
func (c *Client) apiBase() string {
	if strings.TrimSpace(c.APIBase) == "" {
		return defaultAPIBase
	}
	return strings.TrimRight(c.APIBase, "/")
}

// assetName 本机平台与架构对应的发布资产名。
func assetName() (string, error) {
	return assetNameFor(runtime.GOOS, runtime.GOARCH)
}

// assetNameFor 给定 GOOS/GOARCH 的发布资产名。
//
// 三端共用同一套发布流程，资产名按 GOOS 区分：
//   - linux/amd64、linux/arm64：容器与服务器部署（既有形态，名字不变）；
//   - android/arm64：安卓 App —— 资产是 **APK**，不是裸二进制；
//   - windows/amd64：桌面版。
//
// 安卓的 GOOS 是 "android" 而不是 "linux"，虽然内核是 Linux——Go 的 runtime 会
// 把它单独标出来，资产名也必须跟着分，否则手机去下 Linux 的二进制（用不了）。
//
// 之所以把它写成接受 goos/goarch 的**纯函数**（而不是直接读 runtime）：资产名
// 分叉过一次而没人发现——测试里按 runtime.GOOS 分支断言，于是安卓那一支只在
// 安卓主机上才会执行，而本项目从不在安卓上跑 `go test`，那个断言写错了也永远
// 不会失败。参数化之后，三端的命名都能在任意主机上被断言。
func assetNameFor(goos, goarch string) (string, error) {
	if goos == "windows" {
		return "wb2api-windows-" + goarch, nil
	}
	if goos == "android" {
		// 安卓上更新的是 **APK**，不是裸二进制。
		//
		// 为什么：应用私有目录（/data/data/<pkg>/files）里的文件**不能执行**——
		// 不是权限位的问题，是 SELinux 的 untrusted_app 域禁止在 app_data_file 上
		// execute（2026-10-13 真机实测：fork/exec ... permission denied，且
		// setenforce 0 也需要 root）。唯一可执行的位置是 APK 的 nativeLibraryDir，
		// 而它是只读的、改不了。
		//
		// 所以安卓的自更新走系统机制：下载新 APK → 交给系统安装器 → 用户确认后
		// Android 原地替换（应用数据保留）→ 新的 nativeLibraryDir 里就是新二进制。
		// 这条路不需要 root，也是安卓上唯一被支持的自更新方式。
		//
		// 前缀是 "workbuddy2api-"（与 APK 文件名一致），不是 linux/windows 那种
		// "wb2api-"：安卓的资产就是 android/build.py 打出来的那个 APK，两边必须
		// 同名，否则手机永远找不到可更新的资产。
		return "workbuddy2api-android-" + goarch + ".apk", nil
	}
	switch goarch {
	case "amd64":
		return "wb2api-linux-amd64", nil
	case "arm64":
		return "wb2api-linux-arm64", nil
	default:
		return "", fmt.Errorf("unsupported architecture %q for self-update", goarch)
	}
}

// Latest 查询最新发布版本（含本机架构的二进制资产）。
func (c *Client) Latest(ctx context.Context) (Release, error) {
	name, err := assetName()
	if err != nil {
		return Release{}, err
	}
	if c.Bundle {
		// 完整运行包只有 Linux 部署有；其他平台即使误开了 Bundle 也要回落单文件，
		// 否则会去找一个不存在的资产名、报「没有本机架构的二进制资产」。
		if bundleSupported() {
			name = runtimeAssetName()
		}
	}
	if !validRepo(c.Repo) {
		return Release{}, errors.New("invalid update repository")
	}
	url := fmt.Sprintf("%s/repos/%s/releases/latest", c.apiBase(), c.Repo)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return Release{}, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "workbuddy2api-self-update")
	resp, err := c.do(req)
	if err != nil {
		return Release{}, fmt.Errorf("query latest release: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return Release{}, fmt.Errorf("query latest release: HTTP %d %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var payload struct {
		TagName     string    `json:"tag_name"`
		Name        string    `json:"name"`
		PublishedAt time.Time `json:"published_at"`
		Body        string    `json:"body"`
		Assets      []struct {
			Name               string `json:"name"`
			Size               int64  `json:"size"`
			BrowserDownloadURL string `json:"browser_download_url"`
			URL                string `json:"url"`
			Digest             string `json:"digest"`
		} `json:"assets"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&payload); err != nil {
		return Release{}, fmt.Errorf("decode release: %w", err)
	}
	release := Release{
		Tag:         payload.TagName,
		Name:        payload.Name,
		PublishedAt: payload.PublishedAt,
		Notes:       payload.Body,
	}
	for _, asset := range payload.Assets {
		switch asset.Name {
		case name:
			release.AssetName = asset.Name
			release.AssetURL = asset.BrowserDownloadURL
			release.AssetAPIURL = asset.URL
			release.AssetSize = asset.Size
			release.Digest = asset.Digest
		case sumsAssetName:
			release.SumsURL = asset.BrowserDownloadURL
			release.SumsAPIURL = asset.URL
		case sumsSigAssetName:
			release.SumsSigURL = asset.BrowserDownloadURL
			release.SumsSigAPIURL = asset.URL
		}
	}
	return release, nil
}

// Download 把资产下载到 dir，返回文件路径与 SHA-256。
// 发布端提供 digest 时逐字节校验；未提供则只回报实测值，由调用方决定是否信任。
func (c *Client) Download(ctx context.Context, release Release, dir string) (string, string, error) {
	if release.AssetAPIURL != "" {
		release.AssetURL = release.AssetAPIURL
	}
	if release.AssetURL == "" {
		return "", "", errors.New("release has no asset for this architecture")
	}
	if release.AssetSize > maxReleaseBytes {
		return "", "", fmt.Errorf("asset too large: %d bytes", release.AssetSize)
	}
	if release.AssetSize < 0 {
		return "", "", errors.New("invalid asset size")
	}
	if c.Bundle && release.Digest == "" {
		return "", "", errors.New("runtime bundle requires a SHA-256 digest from the release API")
	}
	want := ""
	if release.Digest != "" {
		if !strings.HasPrefix(release.Digest, "sha256:") {
			return "", "", errors.New("asset digest must use sha256")
		}
		want = strings.TrimPrefix(release.Digest, "sha256:")
		decoded, err := hex.DecodeString(want)
		if err != nil || len(decoded) != sha256.Size {
			return "", "", errors.New("asset sha256 digest must contain 64 hexadecimal characters")
		}
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", "", err
	}
	// 资产下载走加速代理（若已配置）。注意这里**不带头部令牌**：代理是第三方，
	// 而发行资产本来就是公开的，且下载后必须通过 sha256 + 发布签名校验。
	downloadURL := c.proxied(release.AssetURL)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, downloadURL, nil)
	if err != nil {
		return "", "", err
	}
	req.Header.Set("User-Agent", "workbuddy2api-self-update")
	req.Header.Set("Accept", "application/octet-stream")
	resp, err := c.do(req)
	if err != nil {
		return "", "", fmt.Errorf("download asset: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", "", fmt.Errorf("download asset: HTTP %d", resp.StatusCode)
	}

	target := filepath.Join(dir, url.PathEscape(release.Tag)+"-"+url.PathEscape(release.AssetName))
	file, err := os.CreateTemp(dir, ".download-*.part")
	if err != nil {
		return "", "", err
	}
	tmp := file.Name()
	defer os.Remove(tmp)
	defer file.Close()
	hasher := sha256.New()
	written, copyErr := io.Copy(io.MultiWriter(file, hasher), io.LimitReader(resp.Body, maxReleaseBytes+1))
	if copyErr != nil {
		return "", "", fmt.Errorf("download asset: %w", copyErr)
	}
	if written > maxReleaseBytes {
		return "", "", fmt.Errorf("asset exceeds %d bytes", maxReleaseBytes)
	}
	if release.AssetSize > 0 && written != release.AssetSize {
		return "", "", fmt.Errorf("asset size mismatch: want %d got %d", release.AssetSize, written)
	}
	sum := hex.EncodeToString(hasher.Sum(nil))
	if want != "" && !strings.EqualFold(want, sum) {
		return "", "", fmt.Errorf("sha256 mismatch: want %s got %s", want, sum)
	}
	if err := file.Chmod(0o755); err != nil {
		return "", "", err
	}
	if err := file.Sync(); err != nil {
		return "", "", err
	}
	if err := file.Close(); err != nil {
		return "", "", err
	}
	if err := os.Rename(tmp, target); err != nil {
		return "", "", err
	}
	return target, sum, nil
}

// runtimeAssetName 完整运行包（网关 + 辅助程序 + 脚本）的资产名。
//
// 只有 Linux 部署用完整运行包（它的运行目录就是解压出来的那个目录）。
// 桌面与安卓用的是单文件资产：桌面版的数据与程序在同一个文件夹里、由启动器管；
// 安卓的程序在 APK 的 nativeLibraryDir 里，完整包那套「解压到目录再整体替换」
// 的前提不成立。
func runtimeAssetName() string {
	return "wb2api-runtime-linux-" + runtime.GOARCH + ".tar.gz"
}

// bundleSupported 本平台是否支持「完整运行包」形式的更新。
func bundleSupported() bool {
	return runtime.GOOS == "linux"
}

// BundleSupported 是 bundleSupported 的导出形式，供宿主（cmd/server）决定
// 是否启用完整运行包模式。
func BundleSupported() bool { return bundleSupported() }

func validRepo(repo string) bool {
	parts := strings.Split(repo, "/")
	if len(parts) != 2 {
		return false
	}
	for _, part := range parts {
		if part == "" || part == "." || part == ".." {
			return false
		}
		for _, c := range part {
			if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_' || c == '.') {
				return false
			}
		}
	}
	return true
}

// A release redirect can lead to a signed CDN URL. Never forward repository
// credentials or cookies there, even when the redirect stays on the same host.
func (c *Client) authorizedURL(target *url.URL) bool {
	base, err := url.Parse(c.apiBase())
	if err != nil || !validRepo(c.Repo) || target.User != nil || target.Scheme != base.Scheme || target.Host != base.Host {
		return false
	}
	// 仓库前缀要么精确等于 /repos/<repo>（元数据接口），要么以它加斜杠开头（其下的资源）。
	// 只认带尾斜杠的形式会让「查仓库可见性」这类不带尾斜杠的接口拿不到令牌，
	// 于是私有仓库连检查更新都失败——2026-09-28 实测到。
	prefix := strings.TrimRight(base.Path, "/") + "/repos/" + c.Repo
	return target.Path == prefix || strings.HasPrefix(target.Path, prefix+"/")
}

func (c *Client) do(req *http.Request) (*http.Response, error) {
	if c.Token != "" && c.authorizedURL(req.URL) {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	client := c.HTTP
	if client == nil {
		client = &http.Client{Timeout: httpTimeout}
	}
	copy := *client
	previous := copy.CheckRedirect
	copy.CheckRedirect = func(next *http.Request, via []*http.Request) error {
		if previous != nil {
			if err := previous(next, via); err != nil {
				return err
			}
		}
		next.Header.Del("Authorization")
		next.Header.Del("Cookie")
		if len(via) >= 5 {
			return errors.New("too many release redirects")
		}
		if len(via) > 0 && via[0].URL.Scheme == "https" && next.URL.Scheme != "https" {
			return errors.New("release redirect must not downgrade HTTPS")
		}
		return nil
	}
	return copy.Do(req)
}

// Visibility 仓库可见性探测结果。
type Visibility string

const (
	// VisibilityPublic 公开仓库：匿名即可读发布，热更新无需令牌。
	VisibilityPublic Visibility = "public"
	// VisibilityPrivate 私有仓库：匿名读不到发布，热更新需要令牌；
	// 未配令牌时网关应明确拒绝而不是给出 404。
	VisibilityPrivate Visibility = "private"
	// VisibilityUnknown 探测失败（网络不可达、令牌无效、仓库不存在等）。
	VisibilityUnknown Visibility = "unknown"
)

// RepoVisibility 探测发布仓库的可见性。
//
// 热更新的可用性由「仓库是否开放」决定，而不是一个需要人工同步的开关：仓库公开时
// 任何人都能读到 Release（含 SHA256SUMS 与签名），热更新天然可用；仓库转私有后匿名
// 读取会 404，热更新必须带令牌才有意义，否则只会给使用者一个无法解释的失败。
//
// 判定依据是 GitHub 的仓库元数据接口：匿名请求 200 = 公开；匿名 404 = 私有或不存在。
// 带令牌时用令牌再问一次，用于区分「私有」与「根本不存在/无权限」。
func (c *Client) RepoVisibility(ctx context.Context) Visibility {
	if !validRepo(c.Repo) {
		return VisibilityUnknown
	}
	// 匿名探测必须真的不带凭据：do() 会按 c.Token 自动加 Authorization，
	// 所以这里显式用一个 Token 为空的副本，而不是依赖调用方恰好没配令牌。
	anonymous := &Client{Repo: c.Repo, HTTP: c.HTTP, APIBase: c.APIBase}
	status, err := anonymous.repoMetaStatus(ctx)
	if err != nil {
		return VisibilityUnknown
	}
	switch status {
	case http.StatusOK:
		return VisibilityPublic
	case http.StatusNotFound:
		// 匿名读不到：要么私有，要么不存在。带令牌确认一次。
		if c.Token == "" {
			return VisibilityPrivate
		}
		authed, err := c.repoMetaStatus(ctx)
		if err != nil {
			return VisibilityUnknown
		}
		if authed == http.StatusOK {
			return VisibilityPrivate
		}
		return VisibilityUnknown
	default:
		return VisibilityUnknown
	}
}

// repoMetaStatus 请求仓库元数据，只回状态码（不读正文，避免把错误页当数据）。
func (c *Client) repoMetaStatus(ctx context.Context) (int, error) {
	if !validRepo(c.Repo) {
		return 0, errors.New("invalid update repository")
	}
	_ = c.HTTP // do() 在 HTTP 为 nil 时自行构造，这里只是显式说明可为 nil
	url := fmt.Sprintf("%s/repos/%s", c.apiBase(), c.Repo)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "workbuddy2api-self-update")
	resp, err := c.do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	return resp.StatusCode, nil
}
