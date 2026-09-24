// ═══ 更新日志 ═══
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
}

// NewClient 构造取件客户端；repo 为空回落默认仓库。
func NewClient(repo, token string) *Client {
	if strings.TrimSpace(repo) == "" {
		repo = DefaultRepo
	}
	return &Client{Repo: repo, Token: token, HTTP: &http.Client{Timeout: httpTimeout}}
}

// apiBase 返回生效的 API 根地址。
func (c *Client) apiBase() string {
	if strings.TrimSpace(c.APIBase) == "" {
		return defaultAPIBase
	}
	return strings.TrimRight(c.APIBase, "/")
}

// assetName 本机架构对应的发布资产名。
func assetName() (string, error) {
	switch runtime.GOARCH {
	case "amd64":
		return "wb2api-linux-amd64", nil
	case "arm64":
		return "wb2api-linux-arm64", nil
	default:
		return "", fmt.Errorf("unsupported architecture %q for self-update", runtime.GOARCH)
	}
}

// Latest 查询最新发布版本（含本机架构的二进制资产）。
func (c *Client) Latest(ctx context.Context) (Release, error) {
	name, err := assetName()
	if err != nil {
		return Release{}, err
	}
	if c.Bundle {
		name = runtimeAssetName()
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
		if asset.Name != name {
			continue
		}
		release.AssetName = asset.Name
		release.AssetURL = asset.BrowserDownloadURL
		release.AssetAPIURL = asset.URL
		release.AssetSize = asset.Size
		release.Digest = asset.Digest
		break
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
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, release.AssetURL, nil)
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

func runtimeAssetName() string { return "wb2api-runtime-linux-" + runtime.GOARCH + ".tar.gz" }

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
	return err == nil && validRepo(c.Repo) && target.User == nil && target.Scheme == base.Scheme && target.Host == base.Host && strings.HasPrefix(target.Path, strings.TrimRight(base.Path, "/")+"/repos/"+c.Repo+"/")
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
