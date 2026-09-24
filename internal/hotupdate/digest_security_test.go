// ═══ 更新日志 ═══
// 2026-09-24：复现非空但畸形的 sha256 摘要跳过校验，防止未验证的更新覆盖文件。
package hotupdate

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDownloadRejectsMalformedDigestBeforeReplacingBinary(t *testing.T) {
	for name, digest := range map[string]string{
		"empty hash": "sha256:", "whitespace": " ", "short hash": "sha256:xyz",
		"wrong algorithm": "sha1:abc", "nonhex": "sha256:" + strings.Repeat("z", 64),
	} {
		t.Run(name, func(t *testing.T) {
			client, release, _ := downloadFixture(t, "v99.0.0", "wb2api")
			dir := t.TempDir()
			target := filepath.Join(dir, "v99.0.0-wb2api")
			const original = "existing verified binary"
			if err := os.WriteFile(target, []byte(original), 0o700); err != nil {
				t.Fatal(err)
			}
			release.Digest = digest
			if _, _, err := client.Download(context.Background(), release, dir); err == nil {
				t.Error("malformed digest was accepted as a verified download")
			}
			got, err := os.ReadFile(target)
			if err != nil || string(got) != original {
				t.Fatal("rejected download replaced the existing binary")
			}
		})
	}
}
