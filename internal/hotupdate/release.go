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
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
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
	// apiTimeout 元数据请求（查版本 / 查仓库可见性）的超时。这类响应只有几 KB，
	// 60s 足够，超时即说明链路不通。
	apiTimeout = 60 * time.Second
	// assetTotalTimeout 单个资产下载的**总**超时上限。
	//
	// 为什么不能只用一个 60s：那是整段下载的墙钟上限，与体积无关。运行包 ~19MB，
	// 60s 内要下完需要 ≥320KB/s——板子这类慢链路上 346KB/s 刚好卡在边缘，直连
	// （实测 15KB/s）必然失败。2026-10-15 实测：板子热更新报
	// `download asset: context deadline exceeded`，只下了 652KB/18.9MB。
	//
	// 现在改为「总上限放宽到 15 分钟 + 空闲（无字节）30s 即判死」：慢但活着的链路
	// 能跑完，真正卡死的链路 30s 就退出，不必等满 15 分钟。
	assetTotalTimeout = 15 * time.Minute
)

// assetIdleTimeout 资产下载的**空闲**超时：连续这么久没有新字节才判失败。
// 这才是「链路断了」的判据——按总时长判会把慢链路误杀。
//
// 是 var 而不是 const：测试要把它压到毫秒级，否则每个用例都得真等 30 秒。
var assetIdleTimeout = 30 * time.Second

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
	// ProxyBase 是下载加速代理的前缀（如 https://v4.gh-proxy.org）。
	//
	// 空 = 直连 GitHub。设置后，**资产下载**改走 <ProxyBase>/<原始 GitHub 地址>。
	//
	// 为什么只代理下载、不代理 API 查询：查询要带 Authorization 头（私有仓库场景），
	// 而把令牌发给第三方代理等于把它交出去。下载的是公开的发行资产、且**必须通过
	// 签名校验**才会被安装（见 manager.go 的验签步骤），所以经过代理不降低信任级别
	// ——代理能改字节，但改不过签名。
	ProxyBase string

	// ProxyMirrors 是代理候选列表（依次尝试）。仅当 ProxyBase 取自**默认值**时
	// 非空——用户显式配置的代理不会被这一串覆盖（见 manager.go NewManager）。
	//
	// 非空时资产下载按 ProxyMirrors 顺序逐个试，最后再回落直连。任一节点返回
	// 403/超时都不致命，换下一个即可。
	ProxyMirrors []string
}

// DefaultProxyBase 默认的下载加速代理。
//
// 国内直连 GitHub 的 releases 经常超时或极慢，热更新因此形同虚设。用户
// 2026-10-13 明确要求「更新都走 gh-proxy 加速」。
//
// ⚠️ 2026-10-15 修正域名：原值是 `https://gh-proxy.com`，该域名的**下载转发已
// 整体失效**——实测它自己的下载路径与文档里的示例 URL（连 `torvalds/linux` 的
// 归档）**全部返回 403**（只有站点首页 HTML 是 200），于是每次热更新都白等一次
// 403 再回落直连。站点本身还在，但当前可用的加速节点在 **`gh-proxy.org`** 系列：
// 站点首页「转换链接」给出的线路是 `v4.gh-proxy.org`（cloudflare，主站加速），
// 另有 `gh-proxy.org` / `v6.gh-proxy.org` / `cdn.gh-proxy.org` / `axisnow.gh-proxy.org`。
//
// 选 `v4.gh-proxy.org` 作默认：板子（行空板 M10）实测 1.02 MB/s，19MB 运行包
// 18.6s 下完；`gh-proxy.org` 346KB/s、`v6` 203KB/s 都更慢，`cdn` 直接超时。
//
// 仍可用 config update.proxy 覆盖（传 "-" 即恢复直连）。
const DefaultProxyBase = "https://v4.gh-proxy.org"

// defaultProxyMirrors 是代理不可用时依次尝试的**同源备选节点**（都属 gh-proxy）。
//
// 为什么要有这一串：2026-10-15 实测 `gh-proxy.com` 整站下载转发 403 时，热更新
// 只能回落直连，而直连在板子上只有 15KB/s——一个镜像失效就等于热更新在慢链路上
// 全废。多个节点互为备份，单点故障不再致命。
//
// 顺序按板子实测速度排（v4 1.02MB/s > gh-proxy.org 346KB/s > v6 203KB/s）。
// 只在**用户没有显式配置**代理时启用；显式配了 `update.proxy` 就只认那一个，
// 不擅自替用户改主意。
var defaultProxyMirrors = []string{
	"https://v4.gh-proxy.org",
	"https://gh-proxy.org",
	"https://v6.gh-proxy.org",
	"https://ghfast.top",
}

// NewClient 构造取件客户端；repo 为空回落默认仓库。
func NewClient(repo, token string) *Client {
	if strings.TrimSpace(repo) == "" {
		repo = DefaultRepo
	}
	return &Client{Repo: repo, Token: token, HTTP: &http.Client{Timeout: apiTimeout}}
}

// proxied 把 GitHub 地址改写成经**生效代理**的地址；未配置代理时原样返回。
func (c *Client) proxied(raw string) string {
	bases := c.proxyBases()
	if len(bases) == 0 {
		return raw
	}
	if u := c.proxiedVia(bases[0], raw); u != "" {
		return u
	}
	return raw
}

// proxiedVia 用指定代理前缀改写 GitHub 地址；该地址不属于可代理范围时返回空串。
//
// 只对 github.com / *.githubusercontent.com 生效：这条改写会把目标地址拼进 URL，
// 若不加限制，一个被篡改的 browser_download_url 就能让网关去请求任意主机
// （SSRF 面）。release.go 里的资产 URL 来自 GitHub API，但纵深防御不依赖上游可信。
func (c *Client) proxiedVia(base, raw string) string {
	base = strings.TrimRight(strings.TrimSpace(base), "/")
	if base == "" || raw == "" {
		return ""
	}
	target, err := url.Parse(raw)
	if err != nil || target.Scheme != "https" {
		return ""
	}
	host := strings.ToLower(target.Hostname())
	if host != "github.com" && host != "api.github.com" && host != "objects.githubusercontent.com" &&
		!strings.HasSuffix(host, ".githubusercontent.com") {
		return ""
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
	case "arm":
		// 32 位 ARM（armv7）。行空板 M10（DFRobot UNIHIKER）是四核 Cortex-A7、
		// 423 MB 内存，用户 2026-10-13 要求支持它。
		//
		// 资产名**必须带 GOARM 版本**，不能只叫 "wb2api-linux-arm"：Go 的 arm
		// 目标有三个 ABI 变体（v5/v6/v7），软浮点与硬浮点的指令集不同，拿 v5 的
		// 二进制到 v7 板上跑会 SIGILL。带上版本号，发布端与自更新端才可能对得上。
		return armAssetName(goarmVersion()), nil
	default:
		return "", fmt.Errorf("unsupported architecture %q for self-update", goarch)
	}
}

// goarmVersion 本次构建的 GOARM 版本（"5"/"6"/"7"），非 arm 目标返回空串。
//
// 读环境变量而不是 runtime 常量：Go 在**编译期**就把 GOARM 烧进二进制了，
// 运行期没有导出这个值（`runtime.GOARCH` 只给 "arm"）。发布与自更新两端都靠
// 构建脚本传 GOARM=7，所以这里读的是「构建时约定的那个值」——它必须与
// build 脚本传给编译器的完全一致，否则会去找一个不存在的资产名。
func goarmVersion() string {
	if v := strings.TrimSpace(os.Getenv("GOARM")); v != "" {
		return v
	}
	// 没设时回落 v7：这是本项目的部署形态（行空板 M10 / 各类 Cortex-A7 板子
	// 都是 armv7 硬浮点）。宁可指向 v7 也不要指向一个不存在的通用名。
	return "7"
}

// armAssetName 32 位 ARM 的资产名。
func armAssetName(goarm string) string {
	return "wb2api-linux-armv" + goarm
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
	return c.DownloadWithProgress(ctx, release, dir, nil)
}

// DownloadWithProgress 与 Download 相同，但把已下载字节数回调出去（供进度条）。
//
// onProgress 可为 nil。回调在下载线程上同步调用，实现方要自己保证轻量（不阻塞、
// 不长时间持锁）——它每次读满一块就会被调用一次。
func (c *Client) DownloadWithProgress(ctx context.Context, release Release, dir string, onProgress func(written, total int64)) (string, string, error) {
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
	// 下载地址候选：**加速代理优先，直连兜底**。
	//
	// 代理能让国内访问快很多，但它是个第三方服务，会挂、会限流、也可能某天消失。
	// 只走代理等于把「能不能更新」押在它身上；只走直连又会在被墙时完全卡住。
	// 所以两条都试：代理失败就回落到 GitHub 直连。
	//
	// 回落不降低信任级别：下载完仍要过 sha256 + 发布签名两道校验（见下与
	// VerifySignedRelease），代理和直连都只是搬运工，改不了内容。
	candidates := c.downloadURLs(release.AssetURL)

	target := filepath.Join(dir, url.PathEscape(release.Tag)+"-"+url.PathEscape(release.AssetName))
	var lastErr error
	for index, candidate := range candidates {
		sum, err := c.fetchAsset(ctx, candidate, target, release.AssetSize, want, onProgress)
		if err == nil {
			if index > 0 {
				log.Printf("[update] 前序地址不可用，已改用：%s", candidate)
			}
			return target, sum, nil
		}
		lastErr = err
		if index < len(candidates)-1 {
			// 只在还有下一条可试时才提示，最后一条失败由调用方统一报错。
			log.Printf("[update] 下载失败，尝试下一个地址：%s（%v）", candidate, err)
		}
	}
	return "", "", lastErr
}

// downloadURLs 返回本次下载要依次尝试的地址：代理候选（按序）→ 直连兜底。
//
// 未配置代理时只有一个（直连）。配置了则把 ProxyMirrors 里每个节点都展开成
// 一条候选，最后接直连。去重：与直连相同的、以及重复出现的节点只保留一次，
// 避免把同一个地址试两遍、白等一次超时。
func (c *Client) downloadURLs(assetURL string) []string {
	direct := assetURL
	bases := c.proxyBases()
	if len(bases) == 0 {
		return []string{direct}
	}
	seen := map[string]bool{direct: true}
	out := make([]string, 0, len(bases)+1)
	for _, base := range bases {
		u := c.proxiedVia(base, assetURL)
		if u == "" || seen[u] {
			continue
		}
		seen[u] = true
		out = append(out, u)
	}
	if len(out) == 0 {
		return []string{direct}
	}
	return append(out, direct)
}

// proxyBases 返回本次要尝试的代理前缀列表。
//
// ProxyMirrors 非空时以它为准（默认值场景）；否则只认单个 ProxyBase
// （用户显式配置的场景，不替他加节点）。
func (c *Client) proxyBases() []string {
	if len(c.ProxyMirrors) > 0 {
		out := make([]string, 0, len(c.ProxyMirrors))
		for _, m := range c.ProxyMirrors {
			if s := strings.TrimRight(strings.TrimSpace(m), "/"); s != "" {
				out = append(out, s)
			}
		}
		return out
	}
	if base := strings.TrimRight(strings.TrimSpace(c.ProxyBase), "/"); base != "" {
		return []string{base}
	}
	return nil
}

// fetchAsset 从 url 下载到 target，校验大小与 sha256 后原子落盘；返回实测摘要。
//
// 校验失败**不**回落：那说明拿到的字节是错的（代理篡改、传输损坏），换一条路重来
// 也应当是同样的内容；真出问题要让它显式报错，而不是靠重试掩盖。
//
// 超时口径（2026-10-15 修）：下载用**空闲超时**而不是总时长——连续
// assetIdleTimeout 没有新字节才判死，整段另设 assetTotalTimeout 上限。此前用的是
// 60s 总超时（HTTP client 的 Timeout 是墙钟），19MB 的包要 ≥320KB/s 才下得完，
// 慢链路必然失败（实测板子只下了 652KB/18.9MB 就报 context deadline exceeded）。
func (c *Client) fetchAsset(ctx context.Context, url, target string, assetSize int64, want string, onProgress func(written, total int64)) (string, error) {
	// 总时长上限：ctx 由调用方传，这里再叠一层，避免上游给一个无上限的 ctx。
	ctx, cancelTotal := context.WithTimeout(ctx, assetTotalTimeout)
	defer cancelTotal()

	// 空闲看门狗必须在**建请求之前**创建，并用它派生的 ctx 建请求。
	// 否则取消的是子 ctx、而请求挂在父 ctx 上，取消对在途读取毫无影响——
	// 写这条时实测：看门狗按时触发了，io.Copy 却仍然卡在 Read 上不返回
	// （测试挂满 120s 超时才发现）。
	watchdog := newIdleWatchdog(ctx, assetIdleTimeout)
	defer watchdog.stop()

	req, err := http.NewRequestWithContext(watchdog.ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "workbuddy2api-self-update")
	req.Header.Set("Accept", "application/octet-stream")
	resp, err := c.do(req)
	if err != nil {
		if watchdog.fired() {
			return "", fmt.Errorf("download asset: 连接后 %v 无数据，链路疑似中断", assetIdleTimeout)
		}
		return "", fmt.Errorf("download asset: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("download asset: HTTP %d", resp.StatusCode)
	}

	file, err := os.CreateTemp(filepath.Dir(target), ".download-*.part")
	if err != nil {
		return "", err
	}
	tmp := file.Name()
	defer os.Remove(tmp)
	defer file.Close()
	hasher := sha256.New()
	body := io.LimitReader(resp.Body, maxReleaseBytes+1)
	if onProgress != nil {
		body = &progressReader{reader: body, total: assetSize, report: onProgress}
	}
	// 每次读到字节就把看门狗推后；连续 assetIdleTimeout 无进展则取消请求。
	body = watchdog.wrap(body)

	written, copyErr := io.Copy(io.MultiWriter(file, hasher), body)
	if copyErr != nil {
		if watchdog.fired() {
			return "", fmt.Errorf("download asset: 连续 %v 无数据（已下载 %d/%d 字节），链路疑似中断",
				assetIdleTimeout, written, assetSize)
		}
		return "", fmt.Errorf("download asset: %w", copyErr)
	}
	if watchdog.fired() {
		return "", fmt.Errorf("download asset: 连续 %v 无数据（已下载 %d/%d 字节）",
			assetIdleTimeout, written, assetSize)
	}
	if written > maxReleaseBytes {
		return "", fmt.Errorf("asset exceeds %d bytes", maxReleaseBytes)
	}
	if assetSize > 0 && written != assetSize {
		return "", fmt.Errorf("asset size mismatch: want %d got %d", assetSize, written)
	}
	sum := hex.EncodeToString(hasher.Sum(nil))
	if want != "" && !strings.EqualFold(want, sum) {
		return "", fmt.Errorf("sha256 mismatch: want %s got %s", want, sum)
	}
	if err := file.Chmod(0o755); err != nil {
		return "", err
	}
	if err := file.Sync(); err != nil {
		return "", err
	}
	if err := file.Close(); err != nil {
		return "", err
	}
	if err := os.Rename(tmp, target); err != nil {
		return "", err
	}
	return sum, nil
}

// runtimeAssetName 完整运行包（网关 + 辅助程序 + 脚本）的资产名。
//
// 只有 Linux 部署用完整运行包（它的运行目录就是解压出来的那个目录）。
// 桌面与安卓用的是单文件资产：桌面版的数据与程序在同一个文件夹里、由启动器管；
// 安卓的程序在 APK 的 nativeLibraryDir 里，完整包那套「解压到目录再整体替换」
// 的前提不成立。
func runtimeAssetName() string {
	return "wb2api-runtime-linux-" + runtimeArchName() + ".tar.gz"
}

// runtimeArchName 运行包名里用的架构标识。
//
// 与裸二进制的命名规则**一致**（armv7 而不是 arm）：两边必须能互相推导，
// 否则「按平台找资产」的代码会在 arm 上找到裸二进制却找不到运行包，
// 表现为「下载成功但解包时找不到本机架构的资产」。
func runtimeArchName() string {
	if runtime.GOARCH == "arm" {
		return "armv" + goarmVersion()
	}
	return runtime.GOARCH
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
		client = &http.Client{Timeout: apiTimeout}
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

// progressReader 统计已读字节并按块回调进度。
//
// 每读一块报一次：更新包十几 MB、块 32KB，也就是几百次回调，开销可以忽略；
// 而按固定字节间隔（如每 1MB）报会让小文件完全看不到进度。
type progressReader struct {
	reader io.Reader
	total  int64
	read   int64
	report func(written, total int64)
}

func (p *progressReader) Read(buffer []byte) (int, error) {
	n, err := p.reader.Read(buffer)
	if n > 0 {
		p.read += int64(n)
		p.report(p.read, p.total)
	}
	return n, err
}

// idleWatchdog 是下载的「无进展」看门狗：每次读到字节就把计时器推后，连续
// timeout 没有任何字节则取消下载上下文。
//
// 为什么需要它：HTTP client 的 Timeout 是**整段墙钟**，对十几 MB 的资产下载
// 等于给了一个与体积挂钩的最低速率要求（19MB/60s = 320KB/s），慢链路必然被误杀。
// 真正该判死的是「链路断了」——表现为长时间收不到任何字节。两者分开之后，慢但
// 活着的链路能跑完，卡死的链路不用等满总超时。
type idleWatchdog struct {
	ctx     context.Context
	cancel  context.CancelFunc
	timeout time.Duration
	mu      sync.Mutex
	timer   *time.Timer
	firedAt bool
	stopped bool
}

// newIdleWatchdog 基于 parent 派生一个可取消的上下文，并启动看门狗。
//
// 调用方必须用返回的 w.ctx 去建请求——看门狗取消的就是它。
func newIdleWatchdog(parent context.Context, timeout time.Duration) *idleWatchdog {
	ctx, cancel := context.WithCancel(parent)
	w := &idleWatchdog{ctx: ctx, cancel: cancel, timeout: timeout}
	w.timer = time.AfterFunc(timeout, func() {
		w.mu.Lock()
		// 已完成（stop 过）的下载不该被这次迟到触发标成失败，用 firedAt 区分。
		if w.stopped {
			w.mu.Unlock()
			return
		}
		w.firedAt = true
		w.mu.Unlock()
		cancel()
	})
	// parent 先取消时（用户中断/总超时）一并释放计时器，避免泄漏。
	go func() {
		<-ctx.Done()
		w.stop()
	}()
	return w
}

// wrap 返回一个每读到字节就重置计时器的 Reader。
func (w *idleWatchdog) wrap(r io.Reader) io.Reader {
	return &idleReader{reader: r, watchdog: w}
}

// fired 报告是否因无进展而触发过取消。
func (w *idleWatchdog) fired() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.firedAt
}

// stop 停止看门狗；重复调用安全。
func (w *idleWatchdog) stop() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.stopped {
		return
	}
	w.stopped = true
	if w.timer != nil {
		w.timer.Stop()
	}
	w.cancel()
}

// idleReader 每次成功读到字节就重置看门狗的计时器。
type idleReader struct {
	reader   io.Reader
	watchdog *idleWatchdog
}

func (r *idleReader) Read(buffer []byte) (int, error) {
	n, err := r.reader.Read(buffer)
	if n > 0 {
		r.watchdog.reset()
	}
	return n, err
}

// reset 把看门狗的截止时间推后一个 timeout。
func (w *idleWatchdog) reset() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.stopped || w.timer == nil {
		return
	}
	w.timer.Reset(w.timeout)
}
