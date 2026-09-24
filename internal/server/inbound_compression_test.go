// ═══ 更新日志 ═══
// 2026-09-25：从真实HTTP入口验证gzip/zstd请求、双重体积限制及鉴权顺序，防压缩载荷误报或绕过限制。
package server

import (
	"bytes"
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/klauspost/compress/zstd"
	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/upstream"
)

func encodedRequest(t *testing.T, encoding string, body []byte) []byte {
	t.Helper()
	var out bytes.Buffer
	if encoding == "gzip" {
		w := gzip.NewWriter(&out)
		_, _ = w.Write(body)
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
	} else {
		w, err := zstd.NewWriter(&out)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = w.Write(body)
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
	}
	return out.Bytes()
}

func compressedHandler(t *testing.T, limit int64, calls *atomic.Int32) *Handler {
	t.Helper()
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		data, _ := io.ReadAll(r.Body)
		if r.Header.Get("Content-Encoding") != "" {
			t.Error("inbound compression leaked to the uncompressed upstream request")
		}
		if !bytes.Contains(data, []byte("compression-canary")) {
			t.Error("decompressed request data was lost")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, sseOK)
	}))
	t.Cleanup(remote.Close)
	return NewHandler(Config{Pool: testPoolWith(&auth.Auth{UID: "compression-fixture", AccessToken: "fixture", ExpiresAt: 9999999999}),
		Upstream: &upstream.Client{ChatBaseCN: remote.URL, HTTP: remote.Client(), ChatHTTP: remote.Client()}, APIKey: "fixture-key", MaxBodyBytes: limit})
}

func TestCompressedInferenceRequests(t *testing.T) {
	for _, path := range []string{"/v1/chat/completions", "/v1/responses"} {
		for _, encoding := range []string{"gzip", "zstd"} {
			t.Run(path+"/"+encoding, func(t *testing.T) {
				var calls atomic.Int32
				h := compressedHandler(t, 1<<20, &calls)
				body := `{"model":"cn:deepseek-v4.1-flash","messages":[{"role":"user","content":"compression-canary"}]}`
				if path == "/v1/responses" {
					body = `{"model":"cn:deepseek-v4.1-flash","input":"compression-canary"}`
				}
				request := httptest.NewRequest("POST", path, bytes.NewReader(encodedRequest(t, encoding, []byte(body))))
				request.Header.Set("Authorization", "Bearer fixture-key")
				request.Header.Set("Content-Encoding", encoding)
				result := httptest.NewRecorder()
				h.ServeHTTP(result, request)
				if result.Code != 200 || calls.Load() != 1 {
					t.Fatalf("compressed request failed: status=%d calls=%d body=%s", result.Code, calls.Load(), result.Body.String())
				}
			})
		}
	}
}

func TestCompressedBodyCannotBypassDecodedLimit(t *testing.T) {
	for _, encoding := range []string{"gzip", "zstd"} {
		t.Run(encoding, func(t *testing.T) {
			var calls atomic.Int32
			h := compressedHandler(t, 1024, &calls)
			body := `{"model":"cn:deepseek-v4.1-flash","input":"` + strings.Repeat("compression-canary", 200) + `"}`
			request := httptest.NewRequest("POST", "/v1/responses", bytes.NewReader(encodedRequest(t, encoding, []byte(body))))
			request.Header.Set("Authorization", "Bearer fixture-key")
			request.Header.Set("Content-Encoding", encoding)
			result := httptest.NewRecorder()
			h.ServeHTTP(result, request)
			if result.Code != 413 || calls.Load() != 0 {
				t.Fatalf("decoded limit not enforced: status=%d calls=%d", result.Code, calls.Load())
			}
		})
	}
}

func TestInvalidRequestCompressionStopsBeforeUpstream(t *testing.T) {
	for _, encoding := range []string{"gzip", "zstd", "br", "gzip, zstd"} {
		t.Run(encoding, func(t *testing.T) {
			var calls atomic.Int32
			h := compressedHandler(t, 1024, &calls)
			request := httptest.NewRequest("POST", "/v1/responses", strings.NewReader("not encoded data"))
			request.Header.Set("Authorization", "Bearer fixture-key")
			request.Header.Set("Content-Encoding", encoding)
			result := httptest.NewRecorder()
			h.ServeHTTP(result, request)
			want := 400
			if encoding == "br" || strings.Contains(encoding, ",") {
				want = 415
			}
			if result.Code != want || calls.Load() != 0 {
				t.Fatalf("bad compressed body: status=%d want=%d calls=%d", result.Code, want, calls.Load())
			}
		})
	}
}

type unreadAuthenticatedBody struct{ reads int }

func (b *unreadAuthenticatedBody) Read(p []byte) (int, error) { b.reads++; return 0, io.EOF }
func (b *unreadAuthenticatedBody) Close() error               { return nil }

func TestAuthenticationPrecedesDecompression(t *testing.T) {
	var calls atomic.Int32
	h := compressedHandler(t, 1024, &calls)
	body := &unreadAuthenticatedBody{}
	request := httptest.NewRequest("POST", "/v1/responses", nil)
	request.Body = body
	request.Header.Set("Authorization", "Bearer wrong")
	request.Header.Set("Content-Encoding", "gzip")
	result := httptest.NewRecorder()
	h.ServeHTTP(result, request)
	if result.Code != 401 || body.reads != 0 {
		t.Fatalf("unauthorized compressed body was read: status=%d reads=%d", result.Code, body.reads)
	}
}
