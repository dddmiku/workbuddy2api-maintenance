// ═══ 更新日志 ═══
// 2026-09-25：使用真实 HTTP 上游尝试和 Handler 验证请求明细的错误归因与已知消费，禁止回退成成功或下行错误。
// 2026-09-25：畸形 finish_reason 不能使同帧完整 usage 丢失，错误请求与消费明细分别校验。
// 2026-09-25：逐次核对循环重发、重发建立失败和缺失用量，账本只记一次请求并汇总各次已知消耗。
// 2026-09-25：真实客户端取消及最终写出/flush失败分别归因，完整上游响应不能被下行故障改成失败。
// 2026-09-25：补聚合成功后工具 schema 拒绝的调用点，完整 JSON 不代表上游输出满足客户端契约。
package server

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"workbuddy2api/internal/requestlog"
	"workbuddy2api/internal/upstream"
	"workbuddy2api/internal/usage"
)

type opsTraceFixture struct {
	handler *Handler
	ledger  *usage.Store
	details *requestlog.Store
	calls   atomic.Int32
}

func newOpsTraceFixture(t *testing.T, respond func(int, http.ResponseWriter, *http.Request)) *opsTraceFixture {
	t.Helper()
	h, ledger := postreleaseUsageHandler(t, "")
	details, err := requestlog.Open(filepath.Join(t.TempDir(), "requests.jsonl"), requestlog.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = details.Close() })
	f := &opsTraceFixture{handler: h, ledger: ledger, details: details}
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		index := int(f.calls.Add(1)) - 1
		if r.Method != http.MethodPost || r.URL.Path != "/v2/chat/completions" {
			t.Errorf("unexpected synthetic upstream operation %s %s", r.Method, r.URL.Path)
			http.Error(w, "unexpected fixture route", http.StatusBadRequest)
			return
		}
		respond(index, w, r)
	}))
	t.Cleanup(remote.Close)
	origin, _ := url.Parse(remote.URL)
	transport := remote.Client().Transport
	if transport == nil {
		transport = http.DefaultTransport
	}
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Scheme != origin.Scheme || r.URL.Host != origin.Host {
			return nil, fmt.Errorf("ops trace fixture blocked non-loopback upstream")
		}
		return transport.RoundTrip(r)
	})}
	h.cfg.Upstream = &upstream.Client{HTTP: client, ChatHTTP: client, ChatBaseCN: remote.URL, BillingBaseCN: remote.URL}
	h.cfg.MaxRotate = 1
	h.cfg.Requests = details
	return f
}

func opsTraceUsage(input, output, cached, reasoning int, credit float64) string {
	data, _ := json.Marshal(map[string]any{"choices": []any{}, "usage": map[string]any{
		"prompt_tokens": input, "completion_tokens": output, "total_tokens": input + output,
		"prompt_tokens_details":     map[string]any{"cached_tokens": cached},
		"completion_tokens_details": map[string]any{"reasoning_tokens": reasoning}, "credit": credit,
	}})
	return "data: " + string(data) + "\n\n"
}

func opsTraceSend(w http.ResponseWriter, status int, streaming bool, body string) {
	contentType := "application/json"
	if streaming {
		contentType = "text/event-stream"
	}
	w.Header().Set("Content-Type", contentType)
	w.WriteHeader(status)
	_, _ = io.WriteString(w, body)
}

func opsTraceRequest(stream bool, tools bool) *http.Request {
	extra := ""
	if tools {
		extra = `,"tools":[{"type":"function","function":{"name":"lookup","parameters":{"type":"object","properties":{"x":{"type":"integer"}},"required":["x"]}}}]`
	}
	body := fmt.Sprintf(`{"model":"cn:deepseek-v4.1-flash","stream":%t,"messages":[{"role":"user","content":"PRIVATE_BODY_MARKER"}]%s}`, stream, extra)
	return httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
}

func opsTraceKnown(t *testing.T, name string, got *int64, want int64) {
	t.Helper()
	if got == nil {
		t.Errorf("%s lost its observed value; got null, want %d", name, want)
	} else if *got != want {
		t.Errorf("%s = %d, want %d", name, *got, want)
	}
}

func (f *opsTraceFixture) observe(t *testing.T, want usage.Totals) (requestlog.Record, requestlog.Summary) {
	t.Helper()
	page, err := f.details.List(requestlog.Query{Limit: 10})
	if err != nil || len(page.Items) != 1 || page.Total != 1 {
		t.Fatalf("expected exactly one persisted request detail: page=%+v err=%v", page, err)
	}
	summary := page.Items[0]
	record, err := f.details.Get(summary.RequestID)
	if err != nil {
		t.Fatal(err)
	}
	ledger := f.ledger.Snapshot().Totals
	evidence, _ := json.Marshal(map[string]any{"case": t.Name(), "actual_http_calls": f.calls.Load(), "detail": record, "summary": summary, "ledger": ledger})
	t.Logf("ops-trace-evidence: %s", evidence)
	if ledger != want {
		t.Errorf("ledger consumption changed: got=%+v want=%+v", ledger, want)
	}
	if !record.UpstreamStarted || record.AttemptCount != int(f.calls.Load()) || len(record.Attempts) != record.AttemptCount {
		t.Errorf("trace does not match actual HTTP attempts: calls=%d count=%d stored=%d", f.calls.Load(), record.AttemptCount, len(record.Attempts))
	}
	if record.Model != "cn:deepseek-v4.1-flash" || record.Protocol != requestlog.ProtocolChat || record.RequestID == "" {
		t.Errorf("request identity/protocol/model changed: %+v", record)
	}
	opsTraceKnown(t, "summary.input", summary.InputTokens, want.PromptTokens)
	if summary.OutputTokens != nil || want.CompletionTokens != 0 {
		opsTraceKnown(t, "summary.output", summary.OutputTokens, want.CompletionTokens)
	}
	opsTraceKnown(t, "summary.cached", summary.CachedTokens, want.CachedTokens)
	if summary.Credit == nil || math.Abs(*summary.Credit-want.Credit) > 1e-9 {
		t.Errorf("summary credit differs from observed ledger: got=%v want=%v", summary.Credit, want.Credit)
	}
	for i, attempt := range record.Attempts {
		if attempt.Number != i+1 || attempt.AccountID == "" || attempt.Model != record.Model || attempt.HTTPStatus == 0 {
			t.Errorf("attempt identity/order/status missing: %+v", attempt)
		}
	}
	for _, forbidden := range []string{"PRIVATE_BODY_MARKER", "UPSTREAM_BODY_MARKER", "fixture-only"} {
		if strings.Contains(string(evidence), forbidden) {
			t.Errorf("request metadata exposed synthetic body/credential marker %q", forbidden)
		}
	}
	return record, summary
}

func opsTraceWireError(raw string, stream bool) string {
	objects := []string{raw}
	if stream {
		objects = nil
		for _, line := range strings.Split(raw, "\n") {
			if strings.HasPrefix(line, "data: ") {
				objects = append(objects, strings.TrimPrefix(line, "data: "))
			}
		}
	}
	for _, object := range objects {
		var value struct {
			Error struct{ Code string } `json:"error"`
		}
		if json.Unmarshal([]byte(object), &value) == nil && value.Error.Code != "" {
			return value.Error.Code
		}
	}
	return ""
}

func TestOpsTraceUpstreamErrorsAreNotSuccessOrDeliveryFailure(t *testing.T) {
	measured := opsTraceUsage(100, 25, 40, 5, 0.25)
	for _, stream := range []bool{false, true} {
		for _, badTool := range []bool{false, true} {
			t.Run(fmt.Sprintf("stream=%t/bad_tool=%t", stream, badTool), func(t *testing.T) {
				payload := postreleaseUsageContent + measured
				if badTool {
					payload += reasoningGuardTool(0, "call_fixture", "lookup", `{"x":1}`) + reasoningGuardTool(0, "", "", "TRAILING_INVALID_SUFFIX") + postreleaseFinish("tool_calls") + "data: [DONE]\n\n"
				} else {
					payload += "data: {\"error\":{\"code\":\"ops_stream_error\",\"message\":\"UPSTREAM_BODY_MARKER\"}}\n\n"
				}
				f := newOpsTraceFixture(t, func(_ int, w http.ResponseWriter, _ *http.Request) { opsTraceSend(w, 200, true, payload) })
				wire := httptest.NewRecorder()
				f.handler.ServeHTTP(wire, opsTraceRequest(stream, badTool))
				record, summary := f.observe(t, usage.Totals{Requests: 1, FailedRequests: 1, PromptTokens: 100, CompletionTokens: 25, CachedTokens: 40, TotalTokens: 125, Credit: 0.25})
				wantHTTP := http.StatusBadGateway
				if stream {
					wantHTTP = http.StatusOK
				}
				if wire.Code != wantHTTP || record.HTTPStatus != wire.Code || record.Status != requestlog.StatusError || record.Stream != stream {
					t.Errorf("request error status differs from actual response: wire=%d record=%+v", wire.Code, record)
				}
				code := opsTraceWireError(wire.Body.String(), stream)
				if code == "" || record.ErrorCode != code {
					t.Errorf("specific client error was lost: wire_code=%q recorded_code=%q", code, record.ErrorCode)
				}
				a := record.Attempts[0]
				if a.Status != requestlog.StatusError || a.HTTPStatus != 200 || a.ErrorCode == "" || a.ErrorCode == "response_delivery_failed" {
					t.Errorf("upstream error was misattributed: %+v", a)
				}
				if !badTool && a.ErrorCode != "ops_stream_error" {
					t.Errorf("original upstream error code was overwritten: %q", a.ErrorCode)
				}
				if summary.UsageState != requestlog.UsageComplete || a.UsageState != requestlog.UsageComplete {
					t.Error("fully observed usage was downgraded despite the ledger retaining a complete report")
				}
				opsTraceKnown(t, "attempt.reasoning", a.ReasoningTokens, 5)
			})
		}
	}
}

func TestOpsTraceMalformedChoiceKeepsIndependentUsage(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%t", stream), func(t *testing.T) {
			payload := strings.Replace(opsTraceUsage(100, 25, 40, 5, 0.25), `"choices":[]`, `"choices":[{"index":0,"delta":{},"finish_reason":{"invalid":true}}]`, 1) + "data: [DONE]\n\n"
			f := newOpsTraceFixture(t, func(_ int, w http.ResponseWriter, _ *http.Request) { opsTraceSend(w, 200, true, payload) })
			wire := httptest.NewRecorder()
			f.handler.ServeHTTP(wire, opsTraceRequest(stream, false))
			record, summary := f.observe(t, usage.Totals{Requests: 1, FailedRequests: 1, PromptTokens: 100, CompletionTokens: 25, CachedTokens: 40, TotalTokens: 125, Credit: 0.25})
			if record.Status != requestlog.StatusError || record.Attempts[0].Status != requestlog.StatusError {
				t.Error("malformed choice was labeled successful")
			}
			if summary.UsageState != requestlog.UsageComplete || record.Attempts[0].UsageState != requestlog.UsageComplete {
				t.Error("valid usage was coupled to malformed choice decoding")
			}
			opsTraceKnown(t, "attempt.reasoning", record.Attempts[0].ReasoningTokens, 5)
		})
	}
}

func TestOpsTraceLoopRetryAccountsForEachHTTPAttempt(t *testing.T) {
	partial := "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":120,\"prompt_tokens_details\":{\"cached_tokens\":64},\"completion_tokens_details\":{\"reasoning_tokens\":4},\"credit\":0.5}}\n\n"
	loop := partial + reasoningGuardFrame(map[string]any{"reasoning_content": reasoningGuardLines(300)})
	success := postreleaseUsageContent + opsTraceUsage(100, 25, 40, 5, 0.25) + postreleaseFinish("stop") + "data: [DONE]\n\n"
	for _, stream := range []bool{false, true} {
		for _, ending := range []string{"success", "loop_again", "rejected_known", "rejected_missing"} {
			t.Run(fmt.Sprintf("stream=%t/%s", stream, ending), func(t *testing.T) {
				f := newOpsTraceFixture(t, func(index int, w http.ResponseWriter, _ *http.Request) {
					if index == 0 || ending == "loop_again" {
						opsTraceSend(w, 200, true, loop)
						return
					}
					if ending == "success" {
						opsTraceSend(w, 200, true, success)
						return
					}
					body := map[string]any{"error": map[string]any{"code": "invalid_request_error", "message": "UPSTREAM_BODY_MARKER"}}
					if ending == "rejected_known" {
						body["usage"] = map[string]any{"prompt_tokens": 7, "completion_tokens": 3, "prompt_tokens_details": map[string]any{"cached_tokens": 2}, "completion_tokens_details": map[string]any{"reasoning_tokens": 1}, "credit": 0.25}
					}
					raw, _ := json.Marshal(body)
					opsTraceSend(w, 400, false, string(raw))
				})
				wire := httptest.NewRecorder()
				f.handler.ServeHTTP(wire, opsTraceRequest(stream, false))
				want := usage.Totals{Requests: 1, UnreportedRequests: 1, PromptTokens: 120, CachedTokens: 64, TotalTokens: 120, Credit: 0.5}
				switch ending {
				case "success":
					want.PromptTokens, want.CompletionTokens, want.CachedTokens, want.TotalTokens, want.Credit = 220, 25, 104, 245, 0.75
				case "loop_again":
					want.FailedRequests, want.PromptTokens, want.CachedTokens, want.TotalTokens, want.Credit = 1, 240, 128, 240, 1
				case "rejected_known":
					want.FailedRequests, want.PromptTokens, want.CompletionTokens, want.CachedTokens, want.TotalTokens, want.Credit = 1, 127, 3, 66, 130, 0.75
				case "rejected_missing":
					want.FailedRequests = 1
				}
				record, summary := f.observe(t, want)
				if record.AttemptCount != 2 || len(record.Attempts) != 2 || f.calls.Load() != 2 {
					t.Fatalf("expected one same-account retry, got calls=%d trace=%+v", f.calls.Load(), record)
				}
				first, last := record.Attempts[0], record.Attempts[1]
				if first.AccountID != last.AccountID || first.Status != requestlog.StatusError || first.ErrorCode != upstream.ReasoningLoopErrorCode || first.UsageState != requestlog.UsagePartial {
					t.Errorf("first interrupted attempt or same-account retry changed: first=%+v last=%+v", first, last)
				}
				opsTraceKnown(t, "first.input", first.InputTokens, 120)
				opsTraceKnown(t, "first.cached", first.CachedTokens, 64)
				opsTraceKnown(t, "first.reasoning", first.ReasoningTokens, 4)
				if first.OutputTokens != nil {
					t.Error("first attempt's missing output was fabricated as measured zero")
				}
				if record.HTTPStatus != wire.Code || summary.UsageState != requestlog.UsagePartial {
					t.Error("request wire status or partial usage summary changed")
				}
				if ending == "success" {
					if record.Status != requestlog.StatusSuccess || record.ErrorCode != "" || last.Status != requestlog.StatusSuccess || last.ErrorCode != "" || last.FinishReason != "stop" || last.UsageState != requestlog.UsageComplete {
						t.Errorf("successful retry inherited the discarded error: record=%+v last=%+v", record, last)
					}
				} else {
					if record.Status != requestlog.StatusError || last.Status != requestlog.StatusError || record.ErrorCode == "" {
						t.Errorf("failed retry has no failure state: record=%+v last=%+v", record, last)
					}
					if ending == "loop_again" {
						if last.ErrorCode != upstream.ReasoningLoopErrorCode || record.ErrorCode != upstream.ReasoningLoopErrorCode {
							t.Error("final loop code was overwritten")
						}
					} else {
						if wireCode := opsTraceWireError(wire.Body.String(), stream); wireCode != "invalid_request_error" || record.ErrorCode != wireCode || last.ErrorCode != wireCode {
							t.Errorf("retry HTTP cause differs between wire/request/attempt: wire=%q request=%q attempt=%q", wireCode, record.ErrorCode, last.ErrorCode)
						}
						if last.HTTPStatus != 400 || last.ErrorCode == "" || last.ErrorCode == "transport_error" || last.ErrorCode == upstream.ReasoningLoopErrorCode {
							t.Errorf("retry HTTP rejection was confused with transport/first-loop failure: %+v", last)
						}
						if ending == "rejected_known" {
							opsTraceKnown(t, "retry.input", last.InputTokens, 7)
							opsTraceKnown(t, "retry.output", last.OutputTokens, 3)
							opsTraceKnown(t, "retry.cached", last.CachedTokens, 2)
							if last.UsageState != requestlog.UsageComplete {
								t.Error("retry response's complete usage was dropped")
							}
						} else if last.InputTokens != nil || last.OutputTokens != nil || last.Credit != nil || last.UsageState != requestlog.UsageMissing {
							t.Error("rejected retry invented missing consumption")
						}
					}
				}
				if ending == "loop_again" || ending == "rejected_missing" {
					if summary.OutputTokens != nil {
						t.Error("no attempt reported output; summary must keep null")
					}
				}
			})
		}
	}
}

func TestOpsTraceClientCancellationKeepsReportedUsage(t *testing.T) {
	upstreamCanceled := make(chan struct{}, 1)
	// Exceed the existing short-line guard threshold so the marker is visible
	// while the upstream remains open, rather than only after its EOF.
	payload := opsTraceUsage(100, 25, 40, 5, 0.25) + reasoningGuardFrame(map[string]any{"content": strings.Repeat("ops_cancel_marker ", 4)})
	f := newOpsTraceFixture(t, func(_ int, w http.ResponseWriter, r *http.Request) {
		opsTraceSend(w, 200, true, payload)
		_ = http.NewResponseController(w).Flush()
		select {
		case <-r.Context().Done():
			upstreamCanceled <- struct{}{}
		case <-time.After(3 * time.Second):
			t.Error("upstream request did not receive cancellation")
		}
	})
	public := httptest.NewServer(f.handler)
	defer public.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	body, _ := io.ReadAll(opsTraceRequest(true, false).Body)
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, public.URL+"/v1/chat/completions", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	client := public.Client()
	client.Timeout = 5 * time.Second
	response, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	reader := bufio.NewReader(response.Body)
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatalf("cancel marker not received: %v", err)
		}
		if strings.Contains(line, "ops_cancel_marker") {
			break
		}
	}
	cancel()
	_ = response.Body.Close()
	select {
	case <-upstreamCanceled:
	case <-time.After(2 * time.Second):
		t.Fatal("client cancellation did not reach real upstream HTTP context")
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		page, err := f.details.List(requestlog.Query{})
		if err != nil {
			t.Fatal(err)
		}
		if page.Total == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("canceled request detail was not persisted")
		}
		time.Sleep(time.Millisecond)
	}
	record, summary := f.observe(t, usage.Totals{Requests: 1, FailedRequests: 1, PromptTokens: 100, CompletionTokens: 25, CachedTokens: 40, TotalTokens: 125, Credit: 0.25})
	a := record.Attempts[0]
	if record.Status != requestlog.StatusCanceled || record.ErrorCode != "client_canceled" || record.HTTPStatus != 200 || a.Status != requestlog.StatusCanceled || a.ErrorCode != "client_canceled" {
		t.Errorf("explicit caller cancellation was misclassified: record=%+v attempt=%+v", record, a)
	}
	if summary.UsageState != requestlog.UsageComplete || a.UsageState != requestlog.UsageComplete || summary.UnknownAttempts != 0 {
		t.Error("fully reported cancellation usage disagrees with the complete-usage ledger")
	}
}

type opsTraceBrokenWriter struct {
	*httptest.ResponseRecorder
	mode       string
	armed, hit bool
}

func (w *opsTraceBrokenWriter) Write(body []byte) (int, error) {
	terminal := strings.Contains(string(body), "[DONE]")
	if w.mode == "json" || w.mode == "terminal" && terminal {
		w.hit = true
		return 0, io.ErrClosedPipe
	}
	if terminal {
		w.armed = true
	}
	return w.ResponseRecorder.Write(body)
}
func (w *opsTraceBrokenWriter) FlushError() error {
	if w.mode == "flush" && w.armed {
		w.hit = true
		return io.ErrClosedPipe
	}
	w.ResponseRecorder.Flush()
	return nil
}
func (w *opsTraceBrokenWriter) Flush() { _ = w.FlushError() }

func TestOpsTraceDownstreamFailureDoesNotRewriteSuccessfulUpstream(t *testing.T) {
	for _, mode := range []string{"json", "terminal", "flush"} {
		t.Run(mode, func(t *testing.T) {
			payload := postreleaseUsageContent + opsTraceUsage(100, 25, 40, 5, 0.25) + postreleaseFinish("stop") + "data: [DONE]\n\n"
			f := newOpsTraceFixture(t, func(_ int, w http.ResponseWriter, _ *http.Request) { opsTraceSend(w, 200, true, payload) })
			wire := &opsTraceBrokenWriter{ResponseRecorder: httptest.NewRecorder(), mode: mode}
			f.handler.ServeHTTP(wire, opsTraceRequest(mode != "json", false))
			if !wire.hit {
				t.Fatal("fixture did not reach intended downstream write failure")
			}
			record, summary := f.observe(t, usage.Totals{Requests: 1, FailedRequests: 1, PromptTokens: 100, CompletionTokens: 25, CachedTokens: 40, TotalTokens: 125, Credit: 0.25})
			a := record.Attempts[0]
			if record.Status != requestlog.StatusError || record.ErrorCode != "response_delivery_failed" || record.HTTPStatus != 200 {
				t.Errorf("local delivery failure confused with caller cancellation/upstream failure: %+v", record)
			}
			if a.Status != requestlog.StatusSuccess || a.ErrorCode != "" || a.HTTPStatus != 200 || a.FinishReason != "stop" {
				t.Errorf("fully received successful upstream was rewritten by downstream failure: %+v", a)
			}
			if summary.UsageState != requestlog.UsageComplete || a.UsageState != requestlog.UsageComplete {
				t.Error("downstream failure erased upstream completion evidence")
			}
		})
	}
}

func TestOpsTraceNonStreamSchemaRejectionAfterSuccessfulAggregation(t *testing.T) {
	payload := postreleaseUsageContent + opsTraceUsage(100, 25, 40, 5, 0.25) +
		reasoningGuardTool(0, "call_fixture", "lookup", `{"x":"wrong_type"}`) + postreleaseFinish("tool_calls") + "data: [DONE]\n\n"
	if _, err := upstream.Aggregate(strings.NewReader(payload)); err != nil {
		t.Fatalf("fixture must aggregate successfully before output contract validation: %v", err)
	}
	f := newOpsTraceFixture(t, func(_ int, w http.ResponseWriter, _ *http.Request) { opsTraceSend(w, 200, true, payload) })
	wire := httptest.NewRecorder()
	// Schema validation is an explicit strict contract. The ordinary non-strict
	// tool fixture above intentionally remains unchanged for the other cases.
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"cn:deepseek-v4.1-flash","stream":false,"messages":[{"role":"user","content":"PRIVATE_BODY_MARKER"}],"tools":[{"type":"function","function":{"name":"lookup","strict":true,"parameters":{"type":"object","properties":{"x":{"type":"integer"}},"required":["x"],"additionalProperties":false}}}]}`))
	f.handler.ServeHTTP(wire, request)
	record, summary := f.observe(t, usage.Totals{Requests: 1, FailedRequests: 1, PromptTokens: 100, CompletionTokens: 25, CachedTokens: 40, TotalTokens: 125, Credit: 0.25})
	a := record.Attempts[0]
	if wire.Code != http.StatusBadGateway || record.HTTPStatus != wire.Code || record.Status != requestlog.StatusError {
		t.Errorf("schema rejection was not a failed client response: wire=%d record=%+v", wire.Code, record)
	}
	if code := opsTraceWireError(wire.Body.String(), false); code != "response_contract_violation" || record.ErrorCode != code || a.ErrorCode != code {
		t.Errorf("post-aggregation contract failure was lost: wire=%q request=%q attempt=%q", code, record.ErrorCode, a.ErrorCode)
	}
	if a.HTTPStatus != http.StatusOK || a.Status != requestlog.StatusError || a.UsageState != requestlog.UsageComplete || summary.UsageState != requestlog.UsageComplete {
		t.Errorf("post-aggregation failure rewrote the transport status or observed usage: %+v", a)
	}
}
