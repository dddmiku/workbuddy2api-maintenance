// ═══ 更新日志 ═══
// 2026-09-26：n>1 拒绝、零参数工具交付、用量帧合并、deflate 请求体。
// 2026-09-26：用量帧默认隐藏，仅显式请求时下发。
// 2026-09-25：客户端隐藏用量不再影响内部计量，覆盖普通流和带工具契约的流式输出。
package server

import (
	"bytes"
	"compress/flate"
	"compress/zlib"
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

// n>1 明确拒绝：上游只返回一个选择，静默降级会让按 n 取值的客户端越界。
func TestChatRejectsMultipleChoices(t *testing.T) {
	calls := 0
	up := newFakeUpstream(t, func(string) (int, string, bool) {
		calls++
		return 200, string(sseStream(`{"choices":[{"index":0,"delta":{"content":"hi"},"finish_reason":"stop"}]}`)), true
	})
	pool := testPoolWith(&auth.Auth{UID: "chat-n", AccessToken: "at", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: pool, Upstream: up})
	for _, body := range []string{
		`{"model":"cn:fixture","n":2,"messages":[{"role":"user","content":"hi"}]}`,
		`{"model":"cn:fixture","n":5,"stream":true,"messages":[{"role":"user","content":"hi"}]}`,
	} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body)))
		if rec.Code != 400 || !strings.Contains(rec.Body.String(), "is not supported") {
			t.Fatalf("n>1 accepted: %d %s", rec.Code, rec.Body)
		}
	}
	if calls != 0 {
		t.Fatalf("n>1 reached upstream %d times", calls)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"cn:fixture","n":1,"messages":[{"role":"user","content":"hi"}]}`)))
	if rec.Code != 200 || calls != 1 {
		t.Fatalf("n=1 rejected: %d calls=%d %s", rec.Code, calls, rec.Body)
	}
}

// 零参数工具调用（arguments 为空串）必须正常交付：上游层认定空串合法，
// 契约层此前把它当不完整参数判死，客户端只收到一个错误帧。
func TestChatDeliversZeroArgumentToolCall(t *testing.T) {
	raw := "data: " + nfBoundaryToolFrame(0, "get_time", "") + "\n\n" + string(sseStream(`{"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`))

	up := newFakeUpstream(t, func(string) (int, string, bool) { return 200, raw, true })
	pool := testPoolWith(&auth.Auth{UID: "zero-arg", AccessToken: "at", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: pool, Upstream: up})
	body := `{"model":"cn:fixture","stream":true,"messages":[{"role":"user","content":"time?"}],"tools":[{"type":"function","function":{"name":"get_time","parameters":{"type":"object","properties":{}}}}]}`
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body)))
	out := rec.Body.String()
	if rec.Code != 200 || strings.Contains(out, "response_contract_violation") {
		t.Fatalf("zero-argument tool call failed: %d %s", rec.Code, out)
	}
	if !strings.Contains(out, `"arguments":"{}"`) {
		t.Fatalf("empty arguments were not normalized for the client: %s", out)
	}
	if !strings.Contains(out, `"finish_reason":"tool_calls"`) {
		t.Fatalf("terminal frame did not report tool_calls: %s", out)
	}
}

// 上游把用量拆成多帧时，客户端只应收到一条合并后的最终快照。
func TestChatMergesSplitUsageIntoOneFrame(t *testing.T) {
	raw := string(sseStream(
		`{"choices":[{"index":0,"delta":{"content":"hi"}}],"usage":{"prompt_tokens":10}}`,
		`{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"completion_tokens":4,"total_tokens":14,"prompt_cache_hit_tokens":6}}`,
	))
	up := newFakeUpstream(t, func(string) (int, string, bool) { return 200, raw, true })
	pool := testPoolWith(&auth.Auth{UID: "usage-merge", AccessToken: "at", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: pool, Upstream: up})
	body := `{"model":"cn:fixture","stream":true,"stream_options":{"include_usage":true},"messages":[{"role":"user","content":"hi"}]}`
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body)))
	usageFrames := 0
	merged := map[string]any{}
	for _, line := range strings.Split(rec.Body.String(), "\n") {
		if !strings.HasPrefix(line, "data: ") || strings.Contains(line, "[DONE]") {
			continue
		}
		var frame map[string]any
		if json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &frame) != nil {
			continue
		}
		usage, ok := frame["usage"].(map[string]any)
		if !ok {
			continue
		}
		choices, _ := frame["choices"].([]any)
		if len(choices) != 0 {
			continue
		}
		usageFrames++
		merged = usage
	}
	if usageFrames != 1 {
		t.Fatalf("client received %d usage frames, want exactly one: %s", usageFrames, rec.Body.String())
	}
	if merged["prompt_tokens"] != float64(10) || merged["completion_tokens"] != float64(4) || merged["total_tokens"] != float64(14) {
		t.Fatalf("usage snapshot lost fields: %v", merged)
	}
}

// deflate 请求体（zlib 与裸 deflate 两种写法）都应被接受：Java/OkHttp 客户端会发。
func TestChatAcceptsDeflateRequestBody(t *testing.T) {
	up := newFakeUpstream(t, func(string) (int, string, bool) {
		return 200, string(sseStream(`{"choices":[{"index":0,"delta":{"content":"ok"},"finish_reason":"stop"}]}`)), true
	})
	pool := testPoolWith(&auth.Auth{UID: "deflate", AccessToken: "at", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: pool, Upstream: up})
	payload := []byte(`{"model":"cn:fixture","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	var zlibBuffer bytes.Buffer
	zw := zlib.NewWriter(&zlibBuffer)
	_, _ = zw.Write(payload)
	_ = zw.Close()
	var rawBuffer bytes.Buffer
	fw, _ := flate.NewWriter(&rawBuffer, flate.DefaultCompression)
	_, _ = fw.Write(payload)
	_ = fw.Close()
	for name, encoded := range map[string][]byte{"zlib": zlibBuffer.Bytes(), "raw": rawBuffer.Bytes()} {
		t.Run(name, func(t *testing.T) {
			request := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader(encoded))
			request.Header.Set("Content-Encoding", "deflate")
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, request)
			if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"content":"ok"`) {
				t.Fatalf("deflate body rejected: %d %s", rec.Code, rec.Body)
			}
		})
	}
}
