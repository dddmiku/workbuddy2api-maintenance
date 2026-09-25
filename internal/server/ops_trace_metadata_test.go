// ═══ 更新日志 ═══
// 2026-09-25：超过调度记录上限时保留真实最后选择，并明确报告总次数与省略历史。
// 2026-09-25：聚合成功后的工具契约拒绝仍须记尝试失败，同时保留全部已知消费。
// 2026-09-25：请求在实际 HTTP 发送前失败时，账本与请求明细都不伪造上游消费。
// 2026-09-25：锁定有界尝试记录的真实末次结果，以及安全的数值/字符串上游错误码。
package server

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/requestlog"
	"workbuddy2api/internal/upstream"
	"workbuddy2api/internal/usage"
)

func TestOpsTraceTruncationKeepsActualLastOutcome(t *testing.T) {
	for _, count := range []int{requestlog.MaxAttempts, requestlog.MaxAttempts + 1} {
		t.Run(fmt.Sprintf("attempts=%d", count), func(t *testing.T) {
			start := time.Now()
			trace := &requestTrace{enabled: true, record: requestlog.Record{StartedAt: start}, stat: &chatStat{failed: true}}
			for i := 1; i <= count; i++ {
				trace.event(upstream.ChatAttemptEvent{Stage: "start", At: time.Now()})
				trace.event(upstream.ChatAttemptEvent{Stage: "headers", Status: 200})
				stats := newChatStatsReaderSince(strings.NewReader(""), start)
				stats.observeJSON(`{"choices":[{"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":2}}`)
				code := ""
				if i == count {
					stats.finishReason, code = "tool_calls", "ops_terminal_error"
				}
				trace.observe(stats, code)
			}
			// A final protocol check observes the same terminal failure. It must
			// not rewrite an older retained attempt when the current one was capped.
			trace.markLastFailure("response_contract_violation", false)
			h := &Handler{}
			h.finishTrace(trace, &traceResponseWriter{ResponseWriter: httptest.NewRecorder(), trace: trace, status: 200}, context.Background())
			if trace.record.ErrorCode != "ops_terminal_error" || trace.record.FinishReason != "tool_calls" || trace.record.Status != requestlog.StatusError {
				t.Errorf("request took its terminal state from an older retained attempt: status=%s error=%q finish=%q", trace.record.Status, trace.record.ErrorCode, trace.record.FinishReason)
			}
			if trace.record.AttemptCount != count || len(trace.record.Attempts) != requestlog.MaxAttempts || trace.record.AttemptsTruncated != (count > requestlog.MaxAttempts) {
				t.Errorf("bounded attempt count changed: count=%d stored=%d truncated=%v", trace.record.AttemptCount, len(trace.record.Attempts), trace.record.AttemptsTruncated)
			}
			if count > requestlog.MaxAttempts {
				stored := trace.record.Attempts[len(trace.record.Attempts)-1]
				if stored.Status != requestlog.StatusSuccess || stored.ErrorCode != "" || stored.FinishReason != "stop" {
					t.Errorf("late failure changed an earlier successful attempt: number=%d status=%s error=%q finish=%q", stored.Number, stored.Status, stored.ErrorCode, stored.FinishReason)
				}
			}
		})
	}
}

func TestOpsTraceErrorMetadataKeepsSafeConcreteCodes(t *testing.T) {
	for _, tc := range []struct {
		name string
		code any
		want string
	}{
		{"string", "fixture_code", "fixture_code"},
		{"numeric", float64(6004), "6004"},
		{"json-number", json.Number("11115"), "11115"},
		{"control-text", "fixture\nPRIVATE_TEXT", "upstream_error"},
		{"oversized", strings.Repeat("x", 65), "upstream_error"},
		{"structured", map[string]any{"message": "PRIVATE_TEXT"}, "upstream_error"},
		{"nonfinite", math.Inf(1), "upstream_error"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := &upstream.StreamError{Code: "upstream_error", Upstream: map[string]any{"code": tc.code, "message": "PRIVATE_TEXT"}}
			if got := traceStreamCode(err, context.Background()); got != tc.want {
				t.Errorf("safe upstream code=%q want=%q", got, tc.want)
			}
		})
	}
}

func TestOpsTracePreflightFailureDoesNotChargeAttempt(t *testing.T) {
	h, ledger := postreleaseUsageHandler(t, "")
	details, err := requestlog.Open(filepath.Join(t.TempDir(), "requests.jsonl"), requestlog.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = details.Close() })
	h.cfg.Requests = details
	h.cfg.MaxRotate = 1
	h.cfg.Upstream.ChatBaseCN = "://invalid"
	wire := httptest.NewRecorder()
	h.ServeHTTP(wire, opsTraceRequest(false, false))
	page, err := details.List(requestlog.Query{Limit: 1})
	if err != nil || len(page.Items) != 1 {
		t.Fatalf("local rejection detail missing: page=%+v err=%v", page, err)
	}
	got := page.Items[0]
	if got.Status != requestlog.StatusRejected || got.UpstreamStarted || got.AttemptCount != 0 || got.UsageState != requestlog.UsageNotStarted {
		t.Errorf("local preflight failure created an upstream attempt: %+v", got)
	}
	if totals := ledger.Snapshot().Totals; totals != (usage.Totals{}) {
		t.Errorf("ledger invented usage for a request rejected before HTTP.Do: %+v", totals)
	}
}

func TestOpsTraceNonStreamingToolContractFailureMarksAttempt(t *testing.T) {
	payload := postreleaseUsageContent + opsTraceUsage(100, 25, 40, 5, 0.25) + postreleaseFinish("stop") + "data: [DONE]\n\n"
	f := newOpsTraceFixture(t, func(_ int, w http.ResponseWriter, _ *http.Request) { opsTraceSend(w, 200, true, payload) })
	extra := `,"tools":[{"type":"function","function":{"name":"lookup","parameters":{"type":"object","properties":{}}}}],"tool_choice":"required"`
	wire := httptest.NewRecorder()
	f.handler.ServeHTTP(wire, postreleaseUsageRequest("/v1/chat/completions", false, extra))
	record, summary := f.observe(t, usage.Totals{Requests: 1, FailedRequests: 1, PromptTokens: 100, CompletionTokens: 25, CachedTokens: 40, TotalTokens: 125, Credit: 0.25})
	if wire.Code != http.StatusBadGateway || record.Status != requestlog.StatusError || opsTraceWireError(wire.Body.String(), false) != "response_contract_violation" {
		t.Fatalf("fixture did not reject missing required tool: status=%d body=%s record=%+v", wire.Code, wire.Body.String(), record)
	}
	if attempt := record.Attempts[0]; attempt.Status != requestlog.StatusError || attempt.ErrorCode != "response_contract_violation" {
		t.Errorf("successful parsing hid the model's tool contract failure: status=%s error=%q", attempt.Status, attempt.ErrorCode)
	}
	if summary.UsageState != requestlog.UsageComplete {
		t.Error("tool contract rejection discarded measured usage")
	}
}

func TestOpsTraceDecisionCapKeepsActualLastSelection(t *testing.T) {
	for _, count := range []int{requestlog.MaxDecisions, requestlog.MaxDecisions + 1, requestlog.MaxDecisions + 6} {
		t.Run(fmt.Sprintf("decisions=%d", count), func(t *testing.T) {
			trace := &requestTrace{enabled: true}
			for i := 1; i <= count; i++ {
				trace.decision(pool.Decision{ReasonCode: "recent_lru", AccountID: fmt.Sprintf("account-%03d", i), SelectionMethod: "lru", ObservedAt: time.Now()})
			}
			if trace.record.DecisionCount != count || trace.record.DecisionsTruncated != (count > requestlog.MaxDecisions) {
				t.Errorf("actual scheduler count/truncation lost: count=%d truncated=%v want=%d", trace.record.DecisionCount, trace.record.DecisionsTruncated, count)
			}
			if len(trace.record.Decisions) != requestlog.MaxDecisions {
				t.Fatalf("scheduler details exceeded or lost their bound: stored=%d", len(trace.record.Decisions))
			}
			for i := 0; i < requestlog.MaxDecisions-1; i++ {
				if got, want := trace.record.Decisions[i].AccountID, fmt.Sprintf("account-%03d", i+1); got != want {
					t.Errorf("retained initial decision changed: index=%d got=%s want=%s", i, got, want)
				}
			}
			last := trace.record.Decisions[len(trace.record.Decisions)-1]
			if want := fmt.Sprintf("account-%03d", count); last.AccountID != want {
				t.Errorf("an old account was presented as the final choice: got=%s want=%s", last.AccountID, want)
			}
		})
	}
}
