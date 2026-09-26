// ═══ 更新日志 ═══
// 2026-09-26：锁定自更新验签：有效签名放行、被篡改或缺失签名一律拒绝、开发构建跳过并说明。
package hotupdate

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"workbuddy2api/internal/version"
)

func TestSumsDigestParsing(t *testing.T) {
	sums := "# generated\n" +
		strings.Repeat("a", 64) + "  wb2api-runtime-linux-arm64.tar.gz\n" +
		strings.Repeat("b", 64) + " *wb2api-linux-arm64\n" +
		"not-a-digest  other.txt\n"
	if got, ok := sumsDigest(sums, "wb2api-runtime-linux-arm64.tar.gz"); !ok || got != strings.Repeat("a", 64) {
		t.Fatalf("plain entry = %q %v", got, ok)
	}
	if got, ok := sumsDigest(sums, "wb2api-linux-arm64"); !ok || got != strings.Repeat("b", 64) {
		t.Fatalf("binary marker entry = %q %v", got, ok)
	}
	if _, ok := sumsDigest(sums, "missing"); ok {
		t.Fatal("missing entry reported as present")
	}
}

func TestVerifySignedRelease(t *testing.T) {
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	asset := []byte("fixture runtime")
	assetSum := sha256.Sum256(asset)
	assetDigest := hex.EncodeToString(assetSum[:])

	makeSums := func(digest string) []byte {
		return []byte(digest + "  " + runtimeAssetName() + "\n")
	}
	sums := makeSums(assetDigest)
	signature := hex.EncodeToString(ed25519.Sign(privateKey, sums))

	state := struct {
		sums       []byte
		signature  string
		includeSig bool
	}{sums: sums, signature: signature, includeSig: true}

	var api *httptest.Server
	api = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/owner/private/releases/latest":
			assets := []any{
				map[string]any{"name": runtimeAssetName(), "size": len(asset), "digest": "sha256:" + assetDigest,
					"url": api.URL + "/repos/owner/private/releases/assets/1"},
				map[string]any{"name": sumsAssetName, "size": len(state.sums),
					"url": api.URL + "/repos/owner/private/releases/assets/2"},
			}
			if state.includeSig {
				assets = append(assets, map[string]any{"name": sumsSigAssetName, "size": len(state.signature),
					"url": api.URL + "/repos/owner/private/releases/assets/3"})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"tag_name": "v9.9.9", "assets": assets})
		case "/repos/owner/private/releases/assets/2":
			_, _ = w.Write(state.sums)
		case "/repos/owner/private/releases/assets/3":
			_, _ = w.Write([]byte(state.signature + "\n"))
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer api.Close()

	original := version.ReleaseKey
	t.Cleanup(func() { version.ReleaseKey = original })
	client := NewClient("owner/private", "token")
	client.APIBase = api.URL
	client.Bundle = true

	load := func(t *testing.T) Release {
		t.Helper()
		release, err := client.Latest(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		return release
	}

	t.Run("valid signature returns the signed digest", func(t *testing.T) {
		version.ReleaseKey = hex.EncodeToString(publicKey)
		digest, verified, err := client.VerifySignedRelease(context.Background(), load(t))
		if err != nil || !verified || digest != assetDigest {
			t.Fatalf("digest=%q verified=%v err=%v", digest, verified, err)
		}
	})

	t.Run("tampered sums are refused", func(t *testing.T) {
		version.ReleaseKey = hex.EncodeToString(publicKey)
		state.sums = makeSums(strings.Repeat("c", 64))
		defer func() { state.sums = sums }()
		if _, _, err := client.VerifySignedRelease(context.Background(), load(t)); err == nil {
			t.Fatal("tampered checksum list accepted")
		}
	})

	t.Run("signature from another key is refused", func(t *testing.T) {
		other, _, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		version.ReleaseKey = hex.EncodeToString(other)
		if _, _, err := client.VerifySignedRelease(context.Background(), load(t)); err == nil {
			t.Fatal("foreign signature accepted")
		}
	})

	t.Run("missing signature asset is refused", func(t *testing.T) {
		version.ReleaseKey = hex.EncodeToString(publicKey)
		state.includeSig = false
		defer func() { state.includeSig = true }()
		if _, _, err := client.VerifySignedRelease(context.Background(), load(t)); err == nil {
			t.Fatal("unsigned release accepted")
		}
	})

	t.Run("github digest must match the signed list", func(t *testing.T) {
		version.ReleaseKey = hex.EncodeToString(publicKey)
		state.sums = makeSums(strings.Repeat("d", 64))
		defer func() { state.sums = sums }()
		release := load(t)
		release.Digest = "sha256:" + strings.Repeat("d", 64)
		valid, err := ed25519.Sign(privateKey, state.sums), error(nil)
		state.signature = hex.EncodeToString(valid)
		defer func() { state.signature = signature }()
		if _, _, err = client.VerifySignedRelease(context.Background(), release); err != nil {
			t.Fatalf("consistent signed digest rejected: %v", err)
		}
		release.Digest = "sha256:" + strings.Repeat("e", 64)
		if _, _, err = client.VerifySignedRelease(context.Background(), release); err == nil {
			t.Fatal("mismatched github digest accepted")
		}
	})

	t.Run("dev build without a key skips and reports", func(t *testing.T) {
		version.ReleaseKey = ""
		_, verified, err := client.VerifySignedRelease(context.Background(), load(t))
		if err != nil || verified {
			t.Fatalf("dev build verified=%v err=%v", verified, err)
		}
	})
}
