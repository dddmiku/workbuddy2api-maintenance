// ═══ 更新日志 ═══
// 2026-09-24：复现自动更新误将旧发布识别为升级，并覆盖显式指定已发布版本的回滚能力。
package hotupdate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	versionpkg "workbuddy2api/internal/version"
)

func TestUpdateAvailableDoesNotRecommendDowngrade(t *testing.T) {
	previous := versionpkg.Version
	t.Cleanup(func() { versionpkg.Version = previous })
	versionpkg.Version = "v2.1.26"
	for tag, want := range map[string]bool{
		"v2.1.25": false, "v2.0.99": false, "v1.9.99": false,
		"v2.1.26": false, "2.1.26": false, "v2.1.27": true,
		"v2.2.0": true, "v3.0.0": true, "v2.1.100": true,
	} {
		if got := (Release{Tag: tag}).UpdateAvailable(); got != want {
			t.Errorf("update from %s to %s = %v, want %v", versionpkg.Version, tag, got, want)
		}
	}
}

func TestExplicitRollbackStillDownloadsSelectedLatestRelease(t *testing.T) {
	previous := versionpkg.Version
	t.Cleanup(func() { versionpkg.Version = previous })
	versionpkg.Version = "v2.1.26"
	content := []byte("invalid executable; this test only checks update selection")
	digest := sha256.Sum256(content)
	name, err := assetName()
	if err != nil {
		t.Skip(err)
	}
	for _, explicit := range []bool{false, true} {
		t.Run(map[bool]string{true: "explicit", false: "automatic"}[explicit], func(t *testing.T) {
			var downloaded atomic.Bool
			var remote *httptest.Server
			remote = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/asset" {
					downloaded.Store(true)
					_, _ = w.Write(content)
					return
				}
				_ = json.NewEncoder(w).Encode(map[string]any{
					"tag_name": "v2.1.25", "assets": []map[string]any{{
						"name": name, "size": len(content), "browser_download_url": remote.URL + "/asset",
						"digest": "sha256:" + hex.EncodeToString(digest[:]),
					}},
				})
			}))
			defer remote.Close()
			running := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
			defer running.Close()
			manager := NewManager(Options{Enabled: true, Dir: t.TempDir(), Listener: running.Listener})
			manager.client.APIBase = remote.URL
			target := ""
			if explicit {
				target = "v2.1.25"
			}
			if _, err := manager.Apply(context.Background(), target); err == nil {
				t.Fatal("fixture must fail before handover")
			}
			if downloaded.Load() != explicit {
				t.Fatalf("downloaded = %v; downgrade must require an explicit target", downloaded.Load())
			}
		})
	}
}
