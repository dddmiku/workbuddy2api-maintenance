// ═══ 更新日志 ═══
// 2026-09-25：覆盖流内超限与循环重试后的超限，锁定真实错误、已观测用量及会话绑定。
package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"workbuddy2api/internal/session"
)

const contextStreamSession = "context-stream-session"
const contextStreamMessage = "prompt is too long: 1048691 tokens > 1048576 maximum"

func contextStreamBind(h *Handler) *bindStore {
	bindings := newBindStore()
	h.cfg.Session = session.New(session.Config{TTL: time.Hour, Store: bindings, Available: h.cfg.Pool.AvailableUIDs})
	h.cfg.Session.Bind(contextStreamSession, "audit-only")
	return bindings
}

func contextStreamRequest(path string, stream bool) *http.Request {
	return postreleaseUsageRequest(path, stream, `,"metadata":{"conversation_id":"`+contextStreamSession+`"}`)
}

func assertContextStreamAccount(t *testing.T, h *Handler, bindings *bindStore) {
	t.Helper()
	if uid, ok := bindings.lastUID(contextStreamSession); !ok || uid != "audit-only" {
		t.Error("context failure discarded the existing session binding")
	}
	state, ok := h.cfg.Pool.Status("audit-only")
	if !ok || state.ErrTotal != 0 || state.BreakerFails != 0 || state.SuccessCount != 0 || state.InFlight != 0 || state.Cooling || state.Disabled {
		t.Errorf("context failure changed account health, success count or in-flight lease: %+v", state)
	}
	if _, known := h.cfg.Pool.ModelCost("audit-only", "deepseek-v4.1-flash"); known {
		t.Error("a failed context request polluted the model cost observation")
	}
}

func contextStreamFailedResponse(t *testing.T, recorder *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var failed map[string]any
	count := 0
	for _, line := range strings.Split(recorder.Body.String(), "\n") {
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var event map[string]any
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &event); err != nil {
			t.Errorf("invalid Responses event: %v", err)
			continue
		}
		if event["type"] == "response.completed" {
			t.Error("context failure emitted a completed response")
		}
		if event["type"] == "response.failed" {
			count++
			failed, _ = event["response"].(map[string]any)
		}
	}
	if count != 1 || failed == nil || failed["status"] != "failed" {
		t.Fatalf("expected exactly one failed response, got %d", count)
	}
	failure, _ := failed["error"].(map[string]any)
	if failure["code"] != "context_length_exceeded" || failure["message"] != contextStreamMessage {
		t.Errorf("context recovery signal was lost: %+v", failure)
	}
	return failed
}

func TestContextStreamFailurePreservesBindingAndActualUsage(t *testing.T) {
	for _, shape := range []struct{ name, payload string }{
		{"openai", `data: {"error":{"code":"context_length_exceeded","message":"` + contextStreamMessage + `"}}`},
		{"vendor", "event: error\ndata: {\"code\":11115,\"msg\":\"" + contextStreamMessage + "\"}"},
	} {
		for _, path := range []string{"/v1/chat/completions", "/v1/responses"} {
			for _, stream := range []bool{false, true} {
				for _, observed := range []bool{false, true} {
					t.Run(fmt.Sprintf("%s/%s/stream=%t/observed=%t", shape.name, path, stream, observed), func(t *testing.T) {
						payload := ""
						if observed {
							payload = reasoningGuardFrame(map[string]any{"content": "visible-before-context-failure: this output is already delivered to the caller."}) + postreleaseUsageOnly
						}
						payload += shape.payload + "\n\n"
						h, ledger := postreleaseUsageHandler(t, payload)
						bindings := contextStreamBind(h)
						calls := 0
						h.cfg.Upstream = newFakeUpstream(t, func(string) (int, string, bool) {
							calls++
							return http.StatusOK, payload, true
						})
						recorder := httptest.NewRecorder()
						h.ServeHTTP(recorder, contextStreamRequest(path, stream))
						if calls != 1 {
							t.Errorf("context error retried the request: calls=%d", calls)
						}
						wantStatus := http.StatusBadRequest
						if stream {
							wantStatus = http.StatusOK
						}
						if recorder.Code != wantStatus {
							t.Errorf("status=%d, want=%d", recorder.Code, wantStatus)
						}
						if stream && path == "/v1/responses" {
							response := contextStreamFailedResponse(t, recorder)
							if observed {
								value, _ := response["usage"].(map[string]any)
								if value["input_tokens"] != float64(5000) || value["output_tokens"] != float64(120) || value["total_tokens"] != float64(5120) {
									t.Errorf("context failure changed observed usage: %+v", value)
								}
							} else if response["usage"] != nil {
								t.Errorf("context failure fabricated usage before any observation: %+v", response["usage"])
							}
						} else if !stream {
							assertJSONErrorCode(t, recorder.Body.String(), "context_length_exceeded")
						}
						if stream && observed && !strings.Contains(recorder.Body.String(), "visible-before-context-failure") {
							t.Error("context failure removed output already delivered to the caller")
						}
						assertContextStreamAccount(t, h, bindings)
						got := ledger.Snapshot().Totals
						if got.Requests != 1 || got.FailedRequests != 1 {
							t.Errorf("context failure was accounted as a successful request: %+v", got)
						}
						if observed {
							if got.PromptTokens != 5000 || got.CompletionTokens != 120 || got.TotalTokens != 5120 || got.Credit != 1.25 || got.UnreportedRequests != 0 {
								t.Errorf("context failure lost observed usage: %+v", got)
							}
						} else if got.TotalTokens != 0 || got.Credit != 0 || got.UnreportedRequests != 1 {
							t.Errorf("context failure invented unobserved usage: %+v", got)
						}
					})
				}
			}
		}
	}
}

func TestContextLoopRetryHTTPFailurePreservesRecovery(t *testing.T) {
	loop := postreleaseUsageOnly + reasoningGuardFrame(map[string]any{"reasoning_content": reasoningGuardLines(300)})
	for _, path := range []string{"/v1/chat/completions", "/v1/responses"} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%t", path, stream), func(t *testing.T) {
				h, ledger := postreleaseUsageHandler(t, loop)
				bindings := contextStreamBind(h)
				calls := 0
				h.cfg.Upstream = newFakeUpstream(t, func(string) (int, string, bool) {
					calls++
					if calls == 1 {
						return http.StatusOK, loop, true
					}
					return http.StatusBadRequest, `{"code":11115,"msg":"` + contextStreamMessage + `"}`, false
				})
				recorder := httptest.NewRecorder()
				h.ServeHTTP(recorder, contextStreamRequest(path, stream))
				if calls != 2 {
					t.Errorf("expected one guarded attempt and one context failure, calls=%d", calls)
				}
				if path == "/v1/responses" && stream {
					if recorder.Code != http.StatusOK {
						t.Errorf("Responses context recovery requires SSE status 200, got %d", recorder.Code)
					}
					response := contextStreamFailedResponse(t, recorder)
					if response["usage"] != nil {
						t.Error("rejected retry leaked previous attempt usage as successful context usage")
					}
				} else {
					if recorder.Code != http.StatusBadRequest {
						t.Errorf("context rejection status=%d, want=400", recorder.Code)
					}
					assertJSONErrorCode(t, recorder.Body.String(), "context_length_exceeded")
				}
				if strings.Contains(recorder.Body.String(), "checking the same step") || strings.Contains(recorder.Body.String(), "upstream_reasoning_loop") {
					t.Error("the discarded loop masked the actual context rejection")
				}
				assertContextStreamAccount(t, h, bindings)
				got := ledger.Snapshot().Totals
				if got.Requests != 1 || got.FailedRequests != 1 || got.PromptTokens != 5000 || got.CompletionTokens != 120 || got.Credit != 1.25 || got.UnreportedRequests != 1 {
					t.Errorf("retry context failure lost the guarded attempt's measured usage: %+v", got)
				}
			})
		}
	}
}

func TestContextStreamRecoveryDoesNotRewriteOtherCodes(t *testing.T) {
	for _, code := range []string{"invalid_api_key", "rate_limit_exceeded", "upstream_reasoning_loop"} {
		t.Run(code, func(t *testing.T) {
			payload := `data: {"error":{"code":"` + code + `","message":"context_length_exceeded is only text here"}}` + "\n\n"
			h, _ := postreleaseUsageHandler(t, payload)
			recorder := httptest.NewRecorder()
			h.ServeHTTP(recorder, contextStreamRequest("/v1/responses", true))
			if !strings.Contains(recorder.Body.String(), `"code":"`+code+`"`) || strings.Contains(recorder.Body.String(), `"code":"context_length_exceeded"`) {
				t.Errorf("unrelated upstream code was changed: %s", recorder.Body.String())
			}
		})
	}
}
