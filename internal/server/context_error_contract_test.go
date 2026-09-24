// ═══ 更新日志 ═══
// 2026-09-25：从实际请求入口核对首次与重试超限的错误码，防止文案误判、格式漏判与错误解绑。
// 2026-09-25：核对错误同帧用量及其最终更新，防止错误解析跳过真实统计或重复累计。
package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestContextHTTPClassificationContract(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		context    bool
	}{
		{"numeric-spaced", `{"code": 11115,"msg":"context capacity exceeded"}`, true},
		{"numeric-string", `{"code":"11115","msg":"context capacity exceeded"}`, true},
		{"standard-code", `{"error":{"code":"context_length_exceeded","message":"context capacity exceeded"}}`, true},
		{"extended-code", `{"extError":{"code":"context_length_exceeded","message":"context capacity exceeded"}}`, true},
		{"legacy-text", `prompt is too long: 1048691 tokens > 1048576 maximum`, true},
		{"auth-quoted-code", `{"error":{"code":"invalid_api_key","message":"context_length_exceeded is only text here"}}`, false},
		{"auth-quoted-text", `{"error":{"code":"invalid_api_key","message":"prompt is too long is only text here"}}`, false},
		{"code-prefix", `{"code":111150,"msg":"unrelated request error"}`, false},
		{"bare-code-text", `context_length_exceeded is only text here`, false},
	} {
		for _, path := range []string{"/v1/chat/completions", "/v1/responses"} {
			for _, stream := range []bool{false, true} {
				for _, retried := range []bool{false, true} {
					t.Run(fmt.Sprintf("%s/%s/stream=%t/retried=%t", tc.name, path, stream, retried), func(t *testing.T) {
						h, ledger := postreleaseUsageHandler(t, "")
						bindings := contextStreamBind(h)
						calls := 0
						h.cfg.Upstream = newFakeUpstream(t, func(string) (int, string, bool) {
							calls++
							if retried && calls == 1 {
								return http.StatusOK, postreleaseUsageOnly + reasoningGuardFrame(map[string]any{"reasoning_content": reasoningGuardLines(300)}), true
							}
							return http.StatusBadRequest, tc.body, false
						})
						recorder := httptest.NewRecorder()
						h.ServeHTTP(recorder, contextStreamRequest(path, stream))
						wantCalls := 1
						if retried {
							wantCalls++
						}
						if calls != wantCalls {
							t.Errorf("rejected request retried again: calls=%d want=%d", calls, wantCalls)
						}
						var failure map[string]any
						if path == "/v1/responses" && strings.HasPrefix(recorder.Header().Get("Content-Type"), "text/event-stream") {
							names, data := eventsOf(t, recorder.Body.String())
							failures := 0
							for index, name := range names {
								if name == evCompleted {
									t.Error("HTTP rejection emitted a successful response")
								}
								if name == evFailed {
									failures++
									response, _ := data[index]["response"].(map[string]any)
									failure, _ = response["error"].(map[string]any)
									if tc.context && (response["usage"] != nil || len(response["output"].([]any)) != 0) {
										t.Error("rejected context invented output or usage")
									}
								}
							}
							if failures != 1 {
								t.Errorf("failed response count=%d want=1", failures)
							}
						} else if strings.HasPrefix(recorder.Header().Get("Content-Type"), "application/json") {
							var response map[string]any
							if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
								t.Fatal(err)
							}
							failure, _ = response["error"].(map[string]any)
						}
						if got := failure["code"] == "context_length_exceeded"; got != tc.context {
							t.Errorf("context classification=%t want=%t: status=%d body=%s", got, tc.context, recorder.Code, recorder.Body.String())
						}
						if tc.context {
							wantStatus := http.StatusBadRequest
							if path == "/v1/responses" && stream {
								wantStatus = http.StatusOK
							}
							if recorder.Code != wantStatus {
								t.Errorf("context status=%d want=%d", recorder.Code, wantStatus)
							}
							assertContextStreamAccount(t, h, bindings)
						}
						got := ledger.Snapshot().Totals
						if got.Requests != 1 || got.FailedRequests != 1 {
							t.Errorf("HTTP rejection was counted as success: %+v", got)
						}
						wantTokens := int64(0)
						if retried {
							wantTokens = 5120
						}
						if got.TotalTokens != wantTokens {
							t.Errorf("HTTP classification changed actual attempt usage: %+v", got)
						}
					})
				}
			}
		}
	}
}

func TestContextFailureSameFrameUsage(t *testing.T) {
	checkContextFailureSameFrameUsage(t, false)
}

func TestContextFailureVendorSameFrameUsage(t *testing.T) {
	checkContextFailureSameFrameUsage(t, true)
}

func checkContextFailureSameFrameUsage(t *testing.T, vendor bool) {
	t.Helper()
	for _, previous := range []bool{false, true} {
		for _, path := range []string{"/v1/chat/completions", "/v1/responses"} {
			t.Run(fmt.Sprintf("%s/previous=%t", path, previous), func(t *testing.T) {
				payload := postreleaseUsageContent
				usage := `{"prompt_tokens":5000,"completion_tokens":150,"total_tokens":5150,"prompt_cache_hit_tokens":4096,"completion_thinking_tokens":100,"credit":1.5}`
				if previous {
					payload += postreleaseUsageOnly
					usage = `{"completion_tokens":150,"total_tokens":5150,"credit":1.5}`
				}
				if vendor {
					payload += `event: error` + "\n" + `data: {"code":11115,"msg":"` + contextStreamMessage + `","usage":` + usage + "}\n\n"
				} else {
					payload += `data: {"error":{"code":"context_length_exceeded","message":"` + contextStreamMessage + `"},"usage":` + usage + "}\n\n"
				}
				h, ledger := postreleaseUsageHandler(t, payload)
				bindings := contextStreamBind(h)
				recorder := httptest.NewRecorder()
				h.ServeHTTP(recorder, contextStreamRequest(path, true))
				if recorder.Code != http.StatusOK {
					t.Fatalf("context stream status=%d body=%s", recorder.Code, recorder.Body.String())
				}
				if path == "/v1/responses" {
					response := contextStreamFailedResponse(t, recorder)
					got, _ := response["usage"].(map[string]any)
					if got["input_tokens"] != float64(5000) || got["output_tokens"] != float64(150) || got["total_tokens"] != float64(5150) {
						t.Errorf("failed response dropped same-frame usage or its final update: %+v", got)
					}
				} else {
					found := false
					for _, line := range strings.Split(recorder.Body.String(), "\n") {
						if !strings.HasPrefix(line, "data: ") || line == "data: [DONE]" {
							continue
						}
						var frame map[string]any
						if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &frame); err != nil {
							t.Fatal(err)
						}
						if frame["error"] != nil {
							found = true
							got, _ := frame["usage"].(map[string]any)
							if got["completion_tokens"] != float64(150) || got["total_tokens"] != float64(5150) || got["credit"] != 1.5 {
								t.Errorf("chat stream dropped the failure's top-level usage: %+v", got)
							}
						}
					}
					if !found {
						t.Fatal("context error was not delivered")
					}
				}
				got := ledger.Snapshot().Totals
				if got.Requests != 1 || got.FailedRequests != 1 || got.UnreportedRequests != 0 || got.PromptTokens != 5000 || got.CompletionTokens != 150 || got.TotalTokens != 5150 || got.CachedTokens != 4096 || got.Credit != 1.5 {
					t.Errorf("same-frame usage was lost or counted twice: %+v", got)
				}
				assertContextStreamAccount(t, h, bindings)
			})
		}
	}
}
