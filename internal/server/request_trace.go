// ═══ 更新日志 ═══
// 2026-09-25：调度明细超过64条保留前63条及真实最后选择，并记录实际总数与截断标记。
// 2026-09-25：取消不抹掉已经完整上报的用量，计量完整度与请求成功状态分开记录。
// 2026-09-25：区分实际下行写失败和调用方取消，已完成的上游尝试不会被交付错误改写。
// 2026-09-25：保留记录上限外的末次结果，安全记录数值错误码，避免历史尝试被收尾改写。
// 2026-09-25：请求收尾继承真实尝试错误，协议收尾不再覆盖已知上游原因。
// 2026-09-25：把每次真实上游尝试、调度和消费关联到请求ID，明细不保存正文或凭据。
package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/keylimit"
	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/requestlog"
	"workbuddy2api/internal/upstream"
)

type requestTraceKey struct{}

type requestTrace struct {
	record                      requestlog.Record
	enabled                     bool
	stat                        *chatStat
	permit                      *keylimit.Permit
	pending                     *requestlog.Attempt
	last                        *requestlog.Attempt
	deliveryErr                 error
	account, accountName, model string
}

func traceFor(r *http.Request) *requestTrace {
	trace, _ := r.Context().Value(requestTraceKey{}).(*requestTrace)
	return trace
}

type traceResponseWriter struct {
	http.ResponseWriter
	trace    *requestTrace
	status   int
	writeErr error
}

func (w *traceResponseWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
func (w *traceResponseWriter) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
	w.ResponseWriter.WriteHeader(status)
}
func (w *traceResponseWriter) Write(data []byte) (int, error) {
	if w.status == 0 {
		w.status = 200
	}
	n, err := w.ResponseWriter.Write(data)
	if err == nil && n != len(data) {
		err = io.ErrShortWrite
	}
	w.observeDeliveryError(err)
	return n, err
}
func (w *traceResponseWriter) Flush() { _ = w.FlushError() }
func (w *traceResponseWriter) FlushError() error {
	if w.writeErr != nil {
		return w.writeErr
	}
	err := flushHTTPResponse(w.ResponseWriter)
	w.observeDeliveryError(err)
	return err
}
func (w *traceResponseWriter) observeDeliveryError(err error) {
	if err != nil && w.writeErr == nil {
		w.writeErr = err
		w.trace.noteDeliveryError(err)
	}
}
func (w *traceResponseWriter) ObserveRequestError(code string) {
	w.trace.record.ErrorCode = traceCode(code)
}

func observeResponseError(w http.ResponseWriter, code string) {
	for depth := 0; w != nil && depth < 32; depth++ {
		if target, ok := w.(interface{ ObserveRequestError(string) }); ok {
			target.ObserveRequestError(code)
		}
		next, ok := w.(interface{ Unwrap() http.ResponseWriter })
		if !ok {
			break
		}
		w = next.Unwrap()
	}
}

func traceCode(code string) string {
	if len(code) > 64 {
		return "upstream_error"
	}
	for _, ch := range code {
		if !((ch >= 'a' && ch <= 'z') || (ch >= 'A' && ch <= 'Z') || (ch >= '0' && ch <= '9') || ch == '_' || ch == '-' || ch == '.' || ch == ':') {
			return "upstream_error"
		}
	}
	return code
}

func traceText(value string) string {
	var out strings.Builder
	for _, ch := range value {
		if unicode.IsControl(ch) || ch == '\u2028' || ch == '\u2029' {
			continue
		}
		n := len(string(ch))
		if out.Len()+n > 256 {
			break
		}
		out.WriteRune(ch)
	}
	return out.String()
}

func (t *requestTrace) selected(a *auth.Auth, bare string) {
	if t == nil || !t.enabled || a == nil {
		return
	}
	copy := a.Snapshot()
	t.account, t.accountName = traceText(copy.UID), traceText(copy.Nickname)
	t.model = traceText(copy.Realm() + ":" + bare)
}

func (t *requestTrace) decision(value pool.Decision) {
	if t == nil || !t.enabled {
		return
	}
	t.record.DecisionCount++
	raw, err := json.Marshal(value)
	if err != nil {
		t.record.DecisionsTruncated = true
		return
	}
	var item requestlog.Decision
	if json.Unmarshal(raw, &item) != nil {
		t.record.DecisionsTruncated = true
		return
	}
	item.Attempt = t.record.AttemptCount + 1
	if len(t.record.Decisions) < requestlog.MaxDecisions {
		t.record.Decisions = append(t.record.Decisions, item)
	} else {
		// Keep the initial history and the real final outcome. The list view
		// derives LastDecision from this last slot, even after history is capped.
		t.record.Decisions[requestlog.MaxDecisions-1] = item
	}
	t.record.DecisionsTruncated = t.record.DecisionCount > len(t.record.Decisions)
}

func (t *requestTrace) event(event upstream.ChatAttemptEvent) {
	if t == nil || !t.enabled {
		return
	}
	if event.Stage == "start" {
		if t.pending != nil {
			t.observe(nil, "unobserved_attempt")
		}
		t.record.UpstreamStarted = true
		t.record.AttemptCount++
		t.pending = &requestlog.Attempt{Number: t.record.AttemptCount, AccountID: t.account, AccountName: t.accountName, Model: t.model, StartedAt: event.At, Status: requestlog.StatusError}
	} else if event.Stage == "headers" && t.pending != nil {
		t.pending.HTTPStatus = event.Status
	}
}

func traceCount(value int) *int64 {
	if value < 0 {
		return nil
	}
	n := int64(value)
	return &n
}

func (t *requestTrace) observe(stats *chatStatsReader, code string) {
	if t == nil || !t.enabled {
		return
	}
	// Calls rejected before HTTP.Do are local failures, not billable attempts.
	if t.pending == nil {
		return
	}
	a := *t.pending
	t.pending = nil
	a.FinishedAt = time.Now()
	a.DurationMS = max(0, a.FinishedAt.Sub(a.StartedAt).Milliseconds())
	a.ErrorCode = traceCode(code)
	if stats != nil {
		a.InputTokens = traceCount(stats.PromptTokens())
		if value, ok := stats.Tokens(); ok {
			a.OutputTokens = traceCount(value)
		}
		a.CachedTokens = traceCount(stats.CachedTokens())
		if stats.reasoning != nil {
			n := *stats.reasoning
			a.ReasoningTokens = &n
		}
		a.FinishReason = traceCode(stats.finishReason)
		if value, ok := stats.Credit(); ok {
			a.Credit = &value
		}
	}
	if a.HTTPStatus >= 200 && a.HTTPStatus < 300 && code == "" {
		a.Status = requestlog.StatusSuccess
	} else if code == "client_canceled" || code == "response_delivery_failed" {
		a.Status = requestlog.StatusCanceled
	} else {
		a.Status = requestlog.StatusError
	}
	switch {
	case a.InputTokens != nil && a.OutputTokens != nil:
		a.UsageState = requestlog.UsageComplete
	case a.InputTokens != nil || a.OutputTokens != nil || a.CachedTokens != nil || a.ReasoningTokens != nil || a.Credit != nil:
		a.UsageState = requestlog.UsagePartial
		a.UsageReason = "partial_usage"
	default:
		a.UsageState = requestlog.UsageMissing
		a.UsageReason = "usage_not_reported"
	}
	if code == "reasoning_loop" || traceLoopCode(code) || code == "stream_incomplete" {
		if a.UsageState == requestlog.UsageComplete {
			a.UsageState = requestlog.UsagePartial
		}
		a.UsageReason = "response_interrupted"
	}
	if (code == "client_canceled" || code == "response_delivery_failed") && a.UsageState != requestlog.UsageComplete {
		a.UsageReason = "response_interrupted"
	}
	if a.HTTPStatus >= 400 && a.UsageState != requestlog.UsageComplete {
		a.UsageReason = "upstream_rejected_without_complete_usage"
	}
	if len(t.record.Attempts) < requestlog.MaxAttempts {
		t.record.Attempts = append(t.record.Attempts, a)
		t.last = &t.record.Attempts[len(t.record.Attempts)-1]
	} else {
		t.record.AttemptsTruncated = true
		// Keep one bounded terminal snapshot even when the persisted attempt
		// array is capped. Finalization must never mutate an earlier attempt.
		t.last = &a
	}
}

func (t *requestTrace) lastAttempt() *requestlog.Attempt {
	if t == nil {
		return nil
	}
	if t.last != nil && t.last.Number == t.record.AttemptCount {
		return t.last
	}
	if n := len(t.record.Attempts); n > 0 && t.record.Attempts[n-1].Number == t.record.AttemptCount {
		return &t.record.Attempts[n-1]
	}
	return nil
}

func (t *requestTrace) markLastFailure(code string, canceled bool) {
	a := t.lastAttempt()
	if a == nil || a.ErrorCode != "" {
		return
	}
	a.ErrorCode = traceCode(code)
	a.Status = requestlog.StatusError
	if canceled {
		a.Status = requestlog.StatusCanceled
	}
}

func (t *requestTrace) noteDeliveryError(err error) {
	if t != nil && err != nil && t.deliveryErr == nil {
		t.deliveryErr = err
	}
}

// CompletionError on a protocol adapter can mean invalid model output. Only
// the writer at the actual network boundary proves a downstream delivery fault.
func traceDeliveryError(w http.ResponseWriter) error {
	for depth := 0; w != nil && depth < 32; depth++ {
		switch writer := w.(type) {
		case *boundedResponseWriter:
			if writer.err != nil {
				return writer.err
			}
		case *traceResponseWriter:
			if writer.writeErr != nil {
				return writer.writeErr
			}
		}
		wrapper, ok := w.(interface{ Unwrap() http.ResponseWriter })
		if !ok {
			break
		}
		w = wrapper.Unwrap()
	}
	return nil
}

func traceCallerCanceled(ctx context.Context, deliveryErr error) bool {
	return ctx.Err() != nil && !(deliveryErr != nil && errors.Is(context.Cause(ctx), deliveryErr))
}

func (t *requestTrace) finishResponseError(w http.ResponseWriter, err error, ctx context.Context) {
	if t == nil || err == nil {
		return
	}
	if deliveryErr := traceDeliveryError(w); deliveryErr != nil {
		t.noteDeliveryError(deliveryErr)
		return
	}
	t.markLastFailure(traceStreamCode(err, ctx), ctx.Err() != nil)
}

func (s *chatStat) streamAttemptCode(err error, stats *chatStatsReader, w http.ResponseWriter, ctx context.Context) string {
	deliveryErr := traceDeliveryError(w)
	s.trace.noteDeliveryError(deliveryErr)
	var streamErr *upstream.StreamError
	if errors.As(err, &streamErr) && !traceCanceledRead(streamErr, ctx) {
		return traceStreamCode(err, ctx)
	}
	if deliveryErr != nil && !traceCallerCanceled(ctx, deliveryErr) {
		if stats != nil && stats.responseEnded() {
			return ""
		}
		return "response_delivery_failed"
	}
	return traceStreamCode(err, ctx)
}

func (h *Handler) finishTrace(t *requestTrace, wire *traceResponseWriter, ctx context.Context) {
	defer func() {
		if t.permit != nil {
			if err := t.permit.Release(); err != nil && !errors.Is(err, keylimit.ErrClosed) {
				log.Printf("WARN: [key-limits] request lease release failed: %v", err)
			}
		}
	}()
	if !t.enabled {
		return
	}
	t.record.FinishedAt = time.Now()
	if t.record.FinishedAt.Before(t.record.StartedAt) {
		t.record.FinishedAt = t.record.StartedAt
	}
	t.record.DurationMS = max(0, t.record.FinishedAt.Sub(t.record.StartedAt).Milliseconds())
	t.record.QueueMS = min(t.record.QueueMS, t.record.DurationMS)
	t.record.HTTPStatus = wire.status
	t.record.Status = requestlog.StatusSuccess
	if t.stat != nil {
		if t.stat.ttfb > 0 {
			n := min(t.record.DurationMS, t.stat.ttfb.Milliseconds())
			t.record.TTFBMS = &n
		}
		if t.stat.failed {
			t.record.Status = requestlog.StatusError
		}
	}
	if wire.status >= 400 {
		t.record.Status = requestlog.StatusError
		if !t.record.UpstreamStarted {
			t.record.Status = requestlog.StatusRejected
		}
	}
	if t.deliveryErr != nil {
		t.record.Status = requestlog.StatusError
		if t.record.ErrorCode == "" {
			t.record.ErrorCode = "response_delivery_failed"
		}
	}
	if traceCallerCanceled(ctx, t.deliveryErr) {
		t.record.Status = requestlog.StatusCanceled
		t.record.ErrorCode = "client_canceled"
	}
	if t.pending != nil {
		code := "transport_error"
		if traceCallerCanceled(ctx, t.deliveryErr) {
			code = "client_canceled"
		} else if t.deliveryErr != nil {
			code = "response_delivery_failed"
		}
		t.observe(nil, code)
	}
	if last := t.lastAttempt(); last != nil {
		t.record.FinishReason = last.FinishReason
		if t.record.Status != requestlog.StatusSuccess && t.record.ErrorCode == "" {
			t.record.ErrorCode = last.ErrorCode
		}
	}
	if t.record.Status != requestlog.StatusSuccess && t.record.ErrorCode == "" {
		t.record.ErrorCode = "request_failed"
	}
	if h.cfg.Requests != nil {
		if err := h.cfg.Requests.Append(t.record); err != nil {
			h.requestLogErrors.Add(1)
			log.Printf("WARN: [requests] could not persist request detail rid=%s: %v", t.record.RequestID, err)
		}
	}
}

func (s *chatStat) attemptCode() string {
	if s.attemptError != "" {
		return s.attemptError
	}
	if s.trace != nil && s.trace.pending != nil && s.trace.pending.HTTPStatus >= 400 {
		return "upstream_error"
	}
	return ""
}

func traceStreamCode(err error, ctx context.Context) string {
	if err == nil {
		return ""
	}
	var stream *upstream.StreamError
	if errors.As(err, &stream) && !traceCanceledRead(stream, ctx) {
		// StreamError.Code may classify an error envelope as upstream_error;
		// the safe, concrete code sent to the client lives in that envelope.
		if code := traceErrorValue(stream.Upstream["code"]); code != "" {
			return code
		}
		return traceCode(stream.Code)
	}
	if ctx.Err() != nil {
		return "client_canceled"
	}
	return "response_contract_violation"
}

func traceCanceledRead(err *upstream.StreamError, ctx context.Context) bool {
	return ctx.Err() != nil && err.Code == "upstream_read_error" && errors.Is(err, ctx.Err())
}

func traceLoopCode(code string) bool {
	return code == upstream.ReasoningLoopErrorCode || code == upstream.OutputLoopErrorCode
}

func traceErrorValue(value any) string {
	switch code := value.(type) {
	case string:
		if code != "" {
			return traceCode(code)
		}
	case json.Number:
		if value, err := code.Float64(); err == nil && !math.IsNaN(value) && !math.IsInf(value, 0) {
			return traceCode(code.String())
		}
	case float64:
		if !math.IsNaN(code) && !math.IsInf(code, 0) {
			return traceCode(strconv.FormatFloat(code, 'f', -1, 64))
		}
	}
	return ""
}

func traceHTTPCode(status int, body []byte) string {
	var response struct {
		Code  any `json:"code"`
		Error struct {
			Code any `json:"code"`
		} `json:"error"`
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	if decoder.Decode(&response) == nil && decoder.Decode(new(any)) == io.EOF {
		if code := traceErrorValue(response.Error.Code); code != "" {
			return code
		}
		if code := traceErrorValue(response.Code); code != "" {
			return code
		}
	}
	return upstream.Classify(status, string(body)).String()
}

func traceTransportCode(ctx context.Context) string {
	if ctx.Err() != nil {
		return "client_canceled"
	}
	return "transport_error"
}

// absorbRetryFailure finalizes only the current failed HTTP attempt. Earlier
// loop attempts and client-internal retries have already been observed.
func (s *chatStat) absorbRetryFailure(ctx context.Context, status int, body []byte, err error) *upstream.StreamError {
	if err != nil {
		code := traceTransportCode(ctx)
		s.attemptError = code
		s.absorbUsage(nil)
		return &upstream.StreamError{Code: code, Message: "upstream retry could not be completed due to a transport error", Cause: err}
	}
	code := traceHTTPCode(status, body)
	s.attemptError = code
	s.absorbJSONUsage(body)
	return &upstream.StreamError{Code: code, Message: fmt.Sprintf("upstream retry failed with HTTP status %d", status)}
}
