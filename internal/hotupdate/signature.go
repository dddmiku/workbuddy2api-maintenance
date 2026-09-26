// ═══ 更新日志 ═══
// 2026-09-26：自更新强制校验发布签名（ed25519 over SHA256SUMS.txt），拒绝接受未签名或签名不符的发布。
package hotupdate

import (
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"strings"

	"workbuddy2api/internal/version"
)

const (
	sumsAssetName    = "SHA256SUMS.txt"
	sumsSigAssetName = "SHA256SUMS.txt.sig"
	// signatureBytes ed25519 签名长度；签名文件为十六进制文本，允许尾随换行。
	signatureBytes = ed25519.SignatureSize
)

// SignedSums 发布端签名的校验和清单及其签名。
type SignedSums struct {
	SumsAPIURL string
	SumsURL    string
	SigAPIURL  string
	SigURL     string
}

// VerifySignedRelease 校验发布资产：用内置公钥验证 SHA256SUMS.txt 的 ed25519 签名，
// 并返回该清单里本机资产对应的摘要。任何一步不成立都返回错误——自更新不接受
// 「拿不到签名所以放行」，否则拿到 GitHub 令牌的人就能向所有部署下发代码。
//
// 开发构建（未注入公钥）跳过校验，并在返回值里说明，供调用方记日志。
func (c *Client) VerifySignedRelease(ctx context.Context, release Release) (digest string, verified bool, err error) {
	if release.AssetName == "" {
		return "", false, fmt.Errorf("发布 %s 没有匹配本机架构的资产", release.Tag)
	}
	if !version.ReleaseVerificationEnabled() {
		return "", false, nil
	}
	if release.SumsAPIURL == "" && release.SumsURL == "" {
		return "", false, fmt.Errorf("发布 %s 没有 %s，拒绝执行未签名的更新", release.Tag, sumsAssetName)
	}
	if release.SumsSigAPIURL == "" && release.SumsSigURL == "" {
		return "", false, fmt.Errorf("发布 %s 没有 %s，拒绝执行未签名的更新", release.Tag, sumsSigAssetName)
	}
	publicKey, err := hex.DecodeString(strings.TrimSpace(version.ReleaseKey))
	if err != nil || len(publicKey) != ed25519.PublicKeySize {
		return "", false, fmt.Errorf("内置发布公钥无效（长度 %d）", len(publicKey))
	}
	signature, err := c.fetchSmall(ctx, release.SumsSigAPIURL, release.SumsSigURL, 4096)
	if err != nil {
		return "", false, fmt.Errorf("下载发布签名: %w", err)
	}
	sums, err := c.fetchSmall(ctx, release.SumsAPIURL, release.SumsURL, 1<<20)
	if err != nil {
		return "", false, fmt.Errorf("下载校验和清单: %w", err)
	}
	raw := decodeSignature(signature)
	if len(raw) != signatureBytes {
		return "", false, fmt.Errorf("发布签名长度异常（%d 字节）", len(raw))
	}
	if !ed25519.Verify(ed25519.PublicKey(publicKey), sums, raw) {
		return "", false, fmt.Errorf("发布 %s 的签名校验失败，拒绝更新", release.Tag)
	}
	digest, ok := sumsDigest(string(sums), release.AssetName)
	if !ok {
		return "", false, fmt.Errorf("签名清单里没有 %s", release.AssetName)
	}
	if release.Digest != "" && !strings.EqualFold("sha256:"+digest, release.Digest) {
		return "", false, fmt.Errorf("发布摘要与签名清单不一致（GitHub %s ≠ 签名 %s）", release.Digest, digest)
	}
	return digest, true, nil
}

// decodeSignature 解析签名文件：十六进制文本（允许空白与换行），也接受原始二进制。
func decodeSignature(raw []byte) []byte {
	text := strings.TrimSpace(string(raw))
	if len(text) == signatureBytes*2 {
		if decoded, err := hex.DecodeString(text); err == nil {
			return decoded
		}
	}
	return raw
}

// sumsDigest 从 `sha256sum` 风格的清单里取指定文件名的摘要。
func sumsDigest(sums, name string) (string, bool) {
	for _, line := range strings.Split(sums, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		digest := strings.ToLower(fields[0])
		if len(digest) != 64 {
			continue
		}
		file := strings.TrimPrefix(strings.Join(fields[1:], " "), "*")
		if file == name {
			return digest, true
		}
	}
	return "", false
}

// fetchSmall 下载小文件（清单与签名）：限长、限次，避免用大文件把内存撑爆。
func (c *Client) fetchSmall(ctx context.Context, apiURL, browserURL string, limit int64) ([]byte, error) {
	target := apiURL
	if target == "" {
		target = browserURL
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/octet-stream")
	req.Header.Set("User-Agent", "workbuddy2api-self-update")
	resp, err := c.do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > limit {
		return nil, fmt.Errorf("文件超过 %d 字节", limit)
	}
	return body, nil
}
