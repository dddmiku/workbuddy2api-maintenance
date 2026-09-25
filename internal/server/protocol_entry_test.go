// ═══ 更新日志 ═══
// 2026-09-25：锁定Gemini前置鉴权/压缩错误、协议发现与所有输出适配器记账前收尾。
package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestGeminiPublicAuthenticationBeforeDecoding(t *testing.T) {
	h := NewHandler(Config{APIKey: "local-only-key"})
	for _, tc := range []struct {
		name, path, authorization, googleKey string
		status                               int
	}{
		{"missing", "/v1beta/models/global:hy3:generateContent", "", "", 401},
		{"wrong_google_key", "/v1beta/models/global:hy3:generateContent", "", "wrong", 401},
		{"google_key", "/v1beta/models/global:hy3:generateContent", "", "local-only-key", 415},
		{"query_key", "/v1beta/models/global:hy3:generateContent?key=local-only-key", "", "", 415},
		{"v1_alias", "/v1/models/global:hy3:streamGenerateContent?alt=sse", "", "local-only-key", 415},
		{"bearer_priority", "/v1beta/models/global:hy3:generateContent", "Bearer wrong", "local-only-key", 401},
		{"valid_bearer_priority", "/v1beta/models/global:hy3:generateContent", "Bearer local-only-key", "wrong", 415},
		{"header_priority_over_query", "/v1beta/models/global:hy3:generateContent?key=local-only-key", "", "wrong", 401},
		{"duplicate_query_key", "/v1beta/models/global:hy3:generateContent?key=one&key=two", "", "", 400},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader("not encoded data"))
			r.Header.Set("Content-Encoding", "unsupported")
			if tc.authorization != "" {
				r.Header.Set("Authorization", tc.authorization)
			}
			if tc.googleKey != "" {
				r.Header.Set("X-Goog-Api-Key", tc.googleKey)
			}
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			var body struct {
				Error struct {
					Code            int `json:"code"`
					Status, Message string
				} `json:"error"`
			}
			if w.Code != tc.status || json.Unmarshal(w.Body.Bytes(), &body) != nil || body.Error.Code != tc.status || body.Error.Status == "" || body.Error.Message == "" {
				t.Fatalf("status/error contract: got %d %s, want Google error %d", w.Code, w.Body, tc.status)
			}
			if strings.Contains(w.Body.String(), "local-only-key") {
				t.Fatal("error exposed request credential")
			}
		})
	}
}

func TestGeminiDiscoverySharesKeyScope(t *testing.T) {
	seedDiscoveryModels(t)
	h, key, calls := boundKeyHandler(t, []string{"cn:known/model"})
	h.cfg.GlobalEnabled = false
	for _, path := range []string{"/v1beta/models?pageSize=200", "/v1/models", "/v1beta/models/" + url.PathEscape("cn:known/model")} {
		r := httptest.NewRequest(http.MethodGet, path, nil)
		r.Header.Set("X-Goog-Api-Key", key)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != 200 || !strings.Contains(w.Body.String(), "models/cn:known/model") || strings.Contains(w.Body.String(), "unknown-limits") {
			t.Fatalf("model permissions/format differ for %s: %d %s", path, w.Code, w.Body)
		}
	}
	r := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	r.Header.Set("Authorization", "Bearer "+key)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"object":"list"`) || strings.Contains(w.Body.String(), `"models":`) {
		t.Fatalf("OpenAI discovery was changed: %d %s", w.Code, w.Body)
	}
	if *calls != 0 {
		t.Fatalf("discovery contacted upstream %d times", *calls)
	}
}

func TestGeminiInteractionsDoesNotSilentlyFallback(t *testing.T) {
	h := NewHandler(Config{APIKey: "local-only-key"})
	r := httptest.NewRequest(http.MethodPost, "/v1beta/interactions", strings.NewReader(`{"model":"global:hy3","input":"hello"}`))
	r.Header.Set("X-Goog-Api-Key", "local-only-key")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 501 || !strings.Contains(w.Body.String(), "UNIMPLEMENTED") {
		t.Fatalf("unavailable transport was not explicit: %d %s", w.Code, w.Body)
	}
}

type delayedProtocolFailure struct {
	inner http.ResponseWriter
	calls int
}

func (w *delayedProtocolFailure) Header() http.Header         { return w.inner.Header() }
func (w *delayedProtocolFailure) WriteHeader(status int)      { w.inner.WriteHeader(status) }
func (w *delayedProtocolFailure) Write(p []byte) (int, error) { return w.inner.Write(p) }
func (w *delayedProtocolFailure) Unwrap() http.ResponseWriter { return w.inner }
func (w *delayedProtocolFailure) FinishResponse() error {
	w.calls++
	return errors.New("terminal write failed")
}

type opaqueProtocolWrapper struct{ inner http.ResponseWriter }

func (w *opaqueProtocolWrapper) Header() http.Header         { return w.inner.Header() }
func (w *opaqueProtocolWrapper) WriteHeader(status int)      { w.inner.WriteHeader(status) }
func (w *opaqueProtocolWrapper) Write(p []byte) (int, error) { return w.inner.Write(p) }
func (w *opaqueProtocolWrapper) Unwrap() http.ResponseWriter { return w.inner }

func TestEveryProtocolFinishesBeforeAccounting(t *testing.T) {
	for _, stream := range []bool{false, true} {
		h, ledger := postreleaseUsageHandler(t, postreleaseUsageContent+postreleaseFinish("stop")+postreleaseUsageOnly+"data: [DONE]\n\n")
		output := &delayedProtocolFailure{inner: httptest.NewRecorder()}
		wrapped := &opaqueProtocolWrapper{inner: output}
		h.chatCompletions(wrapped, postreleaseUsageRequest("/v1/chat/completions", stream, ""))
		got := ledger.Snapshot().Totals
		if output.calls != 1 || got.FailedRequests != 1 || got.Requests != 1 || got.TotalTokens != 5120 || got.UnreportedRequests != 0 {
			t.Fatalf("stream=%v terminal failure bypassed accounting: finishes=%d totals=%+v", stream, output.calls, got)
		}
	}
}
