// ═══ 更新日志 ═══
// 2026-09-25：验证完整运行包、越界/重复/链接/缺件/摘要/架构拒绝，以及私有资产重定向凭据隔离。
package hotupdate

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func testBundle(t *testing.T, alter func(*runtimeManifest, map[string][]byte), extra *tar.Header) string {
	t.Helper()
	files := map[string][]byte{}
	manifest := runtimeManifest{Format: 1, Version: "v2.2.0", OS: "linux", Arch: runtime.GOARCH, Files: map[string]string{}}
	for name := range runtimeFiles {
		data := []byte("fixture:" + name)
		files[name] = data
		sum := sha256.Sum256(data)
		manifest.Files[name] = hex.EncodeToString(sum[:])
	}
	if alter != nil {
		alter(&manifest, files)
	}
	var buffer bytes.Buffer
	gz := gzip.NewWriter(&buffer)
	tw := tar.NewWriter(gz)
	encoded, _ := json.Marshal(manifest)
	files["manifest.json"] = encoded
	for name, data := range files {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0600, Size: int64(len(data))}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(data); err != nil {
			t.Fatal(err)
		}
	}
	if extra != nil {
		if err := tw.WriteHeader(extra); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "runtime.tar.gz")
	if err := os.WriteFile(path, buffer.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestRuntimeBundleContract(t *testing.T) {
	archive := testBundle(t, nil, nil)
	dir := t.TempDir()
	binary, err := extractRuntimeBundle(archive, Release{Tag: "v2.2.0"}, dir)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(binary) != "wb2api" {
		t.Fatalf("unexpected executable: %s", binary)
	}
	for name := range runtimeFiles {
		if _, err := os.Stat(filepath.Join(filepath.Dir(binary), filepath.FromSlash(name))); err != nil {
			t.Fatal(err)
		}
	}
}

func TestRuntimeBundleRejectsPartialAndUnsafePackages(t *testing.T) {
	tests := []struct {
		name  string
		alter func(*runtimeManifest, map[string][]byte)
		extra *tar.Header
	}{
		{"missing", func(_ *runtimeManifest, files map[string][]byte) { delete(files, "credit") }, nil},
		{"digest", func(m *runtimeManifest, _ map[string][]byte) { m.Files["wb2api"] = strings.Repeat("0", 64) }, nil},
		{"arch", func(m *runtimeManifest, _ map[string][]byte) { m.Arch = "wrong" }, nil},
		{"version", func(m *runtimeManifest, _ map[string][]byte) { m.Version = "v1.0.0" }, nil},
		{"traversal", nil, &tar.Header{Name: "../outside", Typeflag: tar.TypeReg}},
		{"duplicate", nil, &tar.Header{Name: "wb2api", Typeflag: tar.TypeReg}},
		{"symlink", nil, &tar.Header{Name: "credit", Typeflag: tar.TypeSymlink, Linkname: "/etc/passwd"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			_, err := extractRuntimeBundle(testBundle(t, tc.alter, tc.extra), Release{Tag: "v2.2.0"}, dir)
			if err == nil {
				t.Fatal("unsafe/partial runtime was accepted")
			}
			entries, _ := os.ReadDir(dir)
			if len(entries) != 0 {
				t.Fatal("failed runtime stage left executable residue")
			}
		})
	}
}

func TestRuntimeBundleRejectsUncheckedTrailers(t *testing.T) {
	base := testBundle(t, nil, nil)
	compressed, err := os.ReadFile(base)
	if err != nil {
		t.Fatal(err)
	}
	reader, err := gzip.NewReader(bytes.NewReader(compressed))
	if err != nil {
		t.Fatal(err)
	}
	tarBytes, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	_ = reader.Close()
	for _, kind := range []string{"nonzero", "large-padding", "extra-member", "crc"} {
		t.Run(kind, func(t *testing.T) {
			var raw []byte
			if kind == "crc" {
				raw = append([]byte{}, compressed...)
				raw[len(raw)-1] ^= 255
			} else if kind == "extra-member" {
				raw = append(append([]byte{}, compressed...), compressed...)
			} else {
				var output bytes.Buffer
				writer := gzip.NewWriter(&output)
				_, _ = writer.Write(tarBytes)
				if kind == "nonzero" {
					_, _ = writer.Write([]byte{1})
				} else {
					_, _ = writer.Write(make([]byte, 20000))
				}
				_ = writer.Close()
				raw = output.Bytes()
			}
			archive := filepath.Join(t.TempDir(), "invalid.tar.gz")
			if err := os.WriteFile(archive, raw, 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := extractRuntimeBundle(archive, Release{Tag: "v2.2.0"}, t.TempDir()); err == nil {
				t.Fatal("unchecked archive trailer accepted")
			}
		})
	}
}

func TestPrivateAssetAPIAndRedirectCredentialScope(t *testing.T) {
	data := []byte("fixture runtime")
	sum := sha256.Sum256(data)
	var api *httptest.Server
	api = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/owner/private/releases/latest":
			if r.Header.Get("Authorization") != "Bearer fixture-token" {
				t.Error("API authentication missing")
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"tag_name": "v2.2.0", "assets": []any{map[string]any{"name": runtimeAssetName(), "size": len(data), "digest": "sha256:" + hex.EncodeToString(sum[:]), "url": api.URL + "/repos/owner/private/releases/assets/1", "browser_download_url": "https://github.com/owner/private/releases/download/v2.2.0/runtime"}}})
		case "/repos/owner/private/releases/assets/1":
			if r.Header.Get("Authorization") != "Bearer fixture-token" || r.Header.Get("Accept") != "application/octet-stream" {
				t.Error("private asset contract not followed")
			}
			http.Redirect(w, r, "/download", http.StatusFound)
		case "/download":
			if r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" {
				t.Error("credential leaked to download redirect")
			}
			_, _ = w.Write(data)
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	defer api.Close()
	client := NewClient("owner/private", "fixture-token")
	client.APIBase = api.URL
	client.Bundle = true
	release, err := client.Latest(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	path, _, err := client.Download(context.Background(), release, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	actual, _ := os.ReadFile(path)
	if !bytes.Equal(actual, data) {
		t.Fatal("asset bytes changed")
	}
	release.Digest = ""
	if _, _, err := client.Download(context.Background(), release, t.TempDir()); err == nil {
		t.Fatal("unsigned runtime accepted")
	}
}
