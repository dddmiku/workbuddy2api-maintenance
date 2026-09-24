// ═══ 更新日志 ═══
// 2026-09-25：超限不得删除历史或伪造成功；Responses流式失败必须触达客户端上下文恢复分支。
package server

import (
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"workbuddy2api/internal/apikeys"
	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/session"
	"workbuddy2api/internal/upstream"
)

func contextRecoveryUsage(prompt, completion int) string {
	return fmt.Sprintf("data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\ndata: {\"usage\":{\"prompt_tokens\":%d,\"completion_tokens\":%d,\"total_tokens\":%d}}\n\ndata: [DONE]\n\n", prompt, completion, prompt+completion)
}

func contextRecoveryBody(t *testing.T, path string, stream bool) []byte {
	t.Helper()
	messages := []any{map[string]any{"role": "system", "content": "retain all history"}}
	for index := 0; index < 12; index++ {
		messages = append(messages,
			map[string]any{"role": "user", "content": "keep-earliest-canary"},
			map[string]any{"role": "assistant", "content": "keep-answer-canary"})
	}
	messages = append(messages, map[string]any{"role": "user", "content": "keep-latest-canary"})
	body := map[string]any{"model": "cn:deepseek-v4.1-flash", "stream": stream}
	if path == "/v1/responses" {
		body["input"] = messages
	} else {
		body["messages"] = messages
	}
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestContextLimitPreservesHistoryAndSignalsRecovery(t *testing.T) {
	errors := []string{
		"{\"code\":11115,\"msg\":\"prompt is too long: 1048691 tokens > 1048576 maximum\"}",
		"{\"code\":11115,\"msg\":\"prompt is too long: 1048691 tokens \\u003e 1048576 maximum\"}",
		"{\"code\":11115,\"msg\":\"prompt is too long: 2015759 tokens > 1048576 maximum\"}",
		"{\"code\":11115,\"msg\":\"prompt is too long\"}",
	}
	for index, sourceError := range errors {
		for _, path := range []string{"/v1/chat/completions", "/v1/responses"} {
			for _, stream := range []bool{false, true} {
				name := path + "/" + map[bool]string{true: "stream", false: "json"}[stream] + "/" + strings.Repeat("x", index+1)
				t.Run(name, func(t *testing.T) {
					var calls atomic.Int32
					remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						data, err := io.ReadAll(r.Body)
						if err != nil {
							t.Errorf("read request: %v", err)
						}
						if calls.Add(1) == 1 {
							if !strings.Contains(string(data), "keep-earliest-canary") || !strings.Contains(string(data), "keep-latest-canary") {
								t.Error("initial request lost context")
							}
							w.Header().Set("Content-Type", "application/json")
							w.WriteHeader(http.StatusBadRequest)
							_, _ = io.WriteString(w, sourceError)
							return
						}
						w.Header().Set("Content-Type", "text/event-stream")
						_, _ = io.WriteString(w, contextRecoveryUsage(5000, 120))
					}))
					defer remote.Close()
					p := testPoolWith(&auth.Auth{UID: "context-fixture", AccessToken: "fixture", ExpiresAt: 9999999999})
					client := &upstream.Client{ChatBaseCN: remote.URL, HTTP: remote.Client(), ChatHTTP: remote.Client()}
					handler := NewHandler(Config{Pool: p, Upstream: client})
					recorder := httptest.NewRecorder()
					handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, path, strings.NewReader(string(contextRecoveryBody(t, path, stream)))))
					if calls.Load() != 1 {
						t.Errorf("context limit must not retry shortened history: calls=%d", calls.Load())
					}
					if path == "/v1/responses" && stream {
						if recorder.Code != http.StatusOK || !strings.HasPrefix(recorder.Header().Get("Content-Type"), "text/event-stream") {
							t.Fatalf("stream client cannot classify the context limit: status=%d type=%s", recorder.Code, recorder.Header().Get("Content-Type"))
						}
						failed := 0
						for _, line := range strings.Split(recorder.Body.String(), "\n") {
							if !strings.HasPrefix(line, "data: ") {
								continue
							}
							var event map[string]any
							if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &event); err != nil {
								t.Fatal(err)
							}
							if event["type"] == "response.completed" || event["type"] == "response.output_item.added" {
								t.Fatalf("context failure emitted a success/output event: %s", line)
							}
							if event["type"] == "response.failed" {
								failed++
								response := event["response"].(map[string]any)
								failure := response["error"].(map[string]any)
								if response["status"] != "failed" || failure["code"] != "context_length_exceeded" ||
									response["usage"] != nil || len(response["output"].([]any)) != 0 {
									t.Fatalf("invalid failure contract: %s", line)
								}
							}
						}
						if failed != 1 {
							t.Fatalf("failed events=%d, want exactly one", failed)
						}
					} else {
						if recorder.Code != http.StatusBadRequest {
							t.Fatalf("context limit became successful: status=%d", recorder.Code)
						}
						var result struct{ Error struct{ Code string } }
						if json.Unmarshal(recorder.Body.Bytes(), &result) != nil || result.Error.Code != "context_length_exceeded" {
							t.Fatalf("context error was lost: %s", recorder.Body.String())
						}
					}
					state, _ := p.Status("context-fixture")
					if state.ErrTotal != 0 || !state.Until.IsZero() || state.InFlight != 0 {
						t.Fatalf("context error changed account health or leaked a lease: %+v", state)
					}
				})
			}
		}
	}
}

func TestResponsesContextRecoveryDoesNotRewriteOtherHTTPFailures(t *testing.T) {
	for _, code := range []string{"invalid_request", "invalid_api_key", "rate_limit_exceeded"} {
		recorder := httptest.NewRecorder()
		writer := newResponsesWriter(recorder, &responsesRequest{Stream: true, Model: "fixture"})
		writeOpenAIError(writer, http.StatusBadRequest, code, "context_length_exceeded is only text here")
		writer.finish()
		if recorder.Code != http.StatusBadRequest || strings.HasPrefix(recorder.Header().Get("Content-Type"), "text/event-stream") {
			t.Fatalf("unrelated error was converted: code=%s status=%d", code, recorder.Code)
		}
	}
}

func TestContextFailureKeepsCallerBinding(t *testing.T) {
	store, err := apikeys.Open(filepath.Join(t.TempDir(), "keys.json"), "")
	if err != nil {
		t.Fatal(err)
	}
	info, key, err := store.Create("fixture", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	up := newFakeUpstream(t, func(string) (int, string, bool) {
		return 400, "{\"code\":11115,\"msg\":\"prompt is too long\"}", false
	})
	p := testPoolWith(&auth.Auth{UID: "bound-account", AccessToken: "fixture", ExpiresAt: 9999999999})
	router := session.New(session.Config{TTL: time.Hour, Available: p.AvailableUIDs})
	scoped := session.ScopeKey(info.ID, "context-session")
	router.Bind(scoped, "bound-account")
	h := NewHandler(Config{Pool: p, Upstream: up, APIKeys: store, Session: router})
	body, _ := json.Marshal(map[string]any{"model": "cn:deepseek-v4.1-flash", "input": "continue",
		"stream": true, "metadata": map[string]any{"conversation_id": "context-session"}})
	request := httptest.NewRequest("POST", "/v1/responses", strings.NewReader(string(body)))
	request.Header.Set("Authorization", "Bearer "+key)
	h.ServeHTTP(httptest.NewRecorder(), request)
	if router.Count() != 1 {
		t.Fatal("a request-size error removed the caller's account binding")
	}
	if uid, ok := router.ResolveForModel(scoped, "cn:deepseek-v4.1-flash"); !ok || uid != "bound-account" {
		t.Fatalf("binding changed after context error: %s %t", uid, ok)
	}
}

func TestSuccessfulUsageAndCostRemainRawAcrossProtocols(t *testing.T) {
	streamBody := "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\n" +
		"data: {\"usage\":{\"prompt_tokens\":5000,\"completion_tokens\":120,\"prompt_tokens_details\":{\"cached_tokens\":4096},\"credit\":1.25}}\n\n" +
		"data: {\"usage\":{\"total_tokens\":5120}}\n\ndata: [DONE]\n\n"
	for _, path := range []string{"/v1/chat/completions", "/v1/responses"} {
		for _, stream := range []bool{false, true} {
			t.Run(path+"/"+map[bool]string{true: "stream", false: "json"}[stream], func(t *testing.T) {
				up := newFakeUpstream(t, func(string) (int, string, bool) { return 200, streamBody, true })
				p := testPoolWith(&auth.Auth{UID: "cost-fixture", AccessToken: "fixture", ExpiresAt: 9999999999})
				h := NewHandler(Config{Pool: p, Upstream: up})
				recorder := httptest.NewRecorder()
				h.ServeHTTP(recorder, httptest.NewRequest("POST", path, strings.NewReader(string(contextRecoveryBody(t, path, stream)))))
				if recorder.Code != 200 {
					t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
				}
				var last map[string]any
				if stream {
					for _, line := range strings.Split(recorder.Body.String(), "\n") {
						if !strings.HasPrefix(line, "data: ") {
							continue
						}
						var event map[string]any
						if json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &event) != nil {
							continue
						}
						if response, ok := event["response"].(map[string]any); ok {
							event = response
						}
						if usage, ok := event["usage"].(map[string]any); ok {
							last = usage
						}
					}
				} else {
					var response map[string]any
					if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
						t.Fatal(err)
					}
					last, _ = response["usage"].(map[string]any)
				}
				in, out := "prompt_tokens", "completion_tokens"
				if path == "/v1/responses" {
					in, out = "input_tokens", "output_tokens"
				}
				if last[in] != float64(5000) || last[out] != float64(120) || last["total_tokens"] != float64(5120) {
					t.Fatalf("usage changed: %+v", last)
				}
				cost, ok := p.ModelCost("cost-fixture", "deepseek-v4.1-flash")
				if !ok || math.Abs(cost-1.25/5120*1000) > 1e-12 {
					t.Fatalf("cost did not use raw tokens: %g %t", cost, ok)
				}
			})
		}
	}
}
