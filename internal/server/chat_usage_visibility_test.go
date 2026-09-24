// ═══ 更新日志 ═══
// 2026-09-25：客户端隐藏用量不再影响内部计量，覆盖普通流和带工具契约的流式输出。
package server

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/upstream"
	"workbuddy2api/internal/usage"
)

const usageVisibilityFixture = `data: {"id":"chatcmpl-usage","model":"m","choices":[{"index":0,"delta":{"content":"hello"}}],"usage":{"prompt_tokens":10,"completion_tokens":1}}

data: {"id":"chatcmpl-usage","model":"m","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}

data: {"id":"chatcmpl-usage","model":"m","choices":[],"usage":{"prompt_tokens":10,"completion_tokens":2,"total_tokens":12,"prompt_tokens_details":{"cached_tokens":3}}}

data: [DONE]

`

func TestChatUsageVisibilityDoesNotChangeAccounting(t *testing.T) {
	for _, contract := range []bool{false, true} {
		for _, option := range []string{"false", "true", "omitted"} {
			name := option
			if contract {
				name += "/tool_contract"
			}
			t.Run(name, func(t *testing.T) {
				ledger, err := usage.Open(filepath.Join(t.TempDir(), "usage.json"), time.Hour)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() {
					if err := ledger.Close(); err != nil {
						t.Error(err)
					}
				})
				var actual map[string]any
				up := &upstream.Client{HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
					raw, _ := io.ReadAll(r.Body)
					if err := json.Unmarshal(raw, &actual); err != nil {
						t.Error(err)
					}
					return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(usageVisibilityFixture))}, nil
				})}, ChatBaseCN: "https://fake.example"}
				h := NewHandler(Config{Pool: testPoolWith(&auth.Auth{UID: "u1", AccessToken: "a1", ExpiresAt: 9999999999}), Upstream: up, Usage: ledger})
				request := map[string]any{"model": "cn:m", "stream": true, "messages": []any{map[string]any{"role": "user", "content": "hi"}}}
				if option != "omitted" {
					request["stream_options"] = map[string]any{"include_usage": option == "true"}
				}
				if contract {
					request["tools"] = []any{map[string]any{"type": "function", "function": map[string]any{"name": "safe", "parameters": map[string]any{"type": "object"}}}}
				}
				raw, _ := json.Marshal(request)
				rec := httptest.NewRecorder()
				h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(string(raw))))
				if rec.Code != 200 || !strings.Contains(rec.Body.String(), "hello") || !strings.Contains(rec.Body.String(), "[DONE]") {
					t.Fatalf("stream lost output: %d %s", rec.Code, rec.Body)
				}
				hasUsage := strings.Contains(rec.Body.String(), `"usage"`)
				if (option == "false") == hasUsage {
					t.Fatalf("client usage visibility option=%s hasUsage=%t: %s", option, hasUsage, rec.Body)
				}
				options, _ := actual["stream_options"].(map[string]any)
				if options["include_usage"] != true {
					t.Fatalf("client display option suppressed upstream accounting: %v", actual["stream_options"])
				}
				got := ledger.Snapshot().Totals
				if got.Requests != 1 || got.PromptTokens != 10 || got.CompletionTokens != 2 || got.TotalTokens != 12 || got.CachedTokens != 3 || got.FailedRequests != 0 || got.UnreportedRequests != 0 {
					t.Fatalf("client display option changed ledger: %+v", got)
				}
			})
		}
	}
}

func TestUsageVisibilityLeavesOtherProtocolsIntact(t *testing.T) {
	for _, path := range []string{"/v1/responses", "/v1/messages"} {
		t.Run(path, func(t *testing.T) {
			up := newFakeUpstream(t, func(string) (int, string, bool) { return 200, usageVisibilityFixture, true })
			h := NewHandler(Config{Pool: testPoolWith(&auth.Auth{UID: "u1", AccessToken: "a1", ExpiresAt: 9999999999}), Upstream: up})
			body := `{"model":"cn:m","stream":true,"input":"hi","stream_options":{"include_usage":false}}`
			if path == "/v1/messages" {
				body = `{"model":"cn:m","stream":true,"max_tokens":64,"messages":[{"role":"user","content":"hi"}],"stream_options":{"include_usage":false}}`
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, path, strings.NewReader(body)))
			if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"usage"`) || !strings.Contains(rec.Body.String(), "hello") {
				t.Fatalf("chat-specific display filter changed %s: %d %s", path, rec.Code, rec.Body)
			}
		})
	}
}

func TestUsageVisibilityPreservesErrorAndExactNumbers(t *testing.T) {
	frame := []byte("data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"}}],\"created\":9007199254740993,\"usage\":{\"prompt_tokens\":1}}\n\n")
	filtered := string(hideUsageFrame(frame))
	if strings.Contains(filtered, `"usage"`) || !strings.Contains(filtered, "9007199254740993") {
		t.Fatalf("usage filtering changed unrelated numeric data: %s", filtered)
	}
	failure := string(hideUsageFrame([]byte("data: {\"error\":{\"code\":\"context_length_exceeded\"},\"usage\":{\"prompt_tokens\":1}}\n\n")))
	if !strings.Contains(failure, "context_length_exceeded") || strings.Contains(failure, `"usage"`) {
		t.Fatalf("usage filtering lost error or revealed usage: %s", failure)
	}
}

func TestJSONFlushFailureIsAccountedAsFailed(t *testing.T) {
	for _, path := range []string{"/v1/chat/completions", "/v1/responses", "/v1/messages"} {
		t.Run(path, func(t *testing.T) {
			ledger, err := usage.Open(filepath.Join(t.TempDir(), "usage.json"), time.Hour)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := ledger.Close(); err != nil {
					t.Error(err)
				}
			})
			up := newFakeUpstream(t, func(string) (int, string, bool) { return 200, usageVisibilityFixture, true })
			h := NewHandler(Config{Pool: testPoolWith(&auth.Auth{UID: "u1", AccessToken: "a1", ExpiresAt: 9999999999}), Upstream: up, Usage: ledger})
			body := `{"model":"cn:m","stream":false,"max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`
			if path == "/v1/responses" {
				body = `{"model":"cn:m","stream":false,"input":"hi"}`
			}
			w := &deadlineObservationWriter{ResponseRecorder: httptest.NewRecorder(), flushErr: errors.New("client stopped reading final JSON")}
			h.ServeHTTP(w, httptest.NewRequest(http.MethodPost, path, strings.NewReader(body)))
			got := ledger.Snapshot().Totals
			if got.Requests != 1 || got.FailedRequests != 1 || got.TotalTokens != 12 {
				t.Fatalf("final buffered JSON failure was reported as success or lost usage: %+v", got)
			}
		})
	}
}
