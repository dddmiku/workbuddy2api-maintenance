// ═══ 更新日志 ═══
// 2026-09-25：限定请求明细各字段、尝试数量与查询边界，避免日志膨胀或把缺失值写成零。
// 2026-09-25：校验真实调度总数，兼容旧记录缺省值并以保留条数推导截断状态。
package requestlog

import (
	"fmt"
	"math"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

func normalizeOptions(o Options) (Options, error) {
	if o.MaxRecords == 0 {
		o.MaxRecords = DefaultMaxRecords
	}
	if o.MaxAge == 0 {
		o.MaxAge = DefaultMaxAge
	}
	if o.MaxBytes == 0 {
		o.MaxBytes = DefaultMaxBytes
	}
	if o.MaxRecords < 1 || o.MaxRecords > DefaultMaxRecords || o.MaxAge < time.Second || o.MaxAge > DefaultMaxAge || o.MaxBytes < 512 || o.MaxBytes > DefaultMaxBytes {
		return Options{}, fmt.Errorf("request log options exceed retention boundaries")
	}
	return o, nil
}

func validText(value string, max int, required bool) bool {
	if len(value) > max || !utf8.ValidString(value) || (required && strings.TrimSpace(value) == "") {
		return false
	}
	for _, r := range value {
		if unicode.IsControl(r) || r == '\u2028' || r == '\u2029' {
			return false
		}
	}
	return true
}

func validCode(value string, max int, required bool) bool {
	if len(value) > max || (required && value == "") {
		return false
	}
	for _, r := range value {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' || r == '-' || r == '.' || r == ':' {
			continue
		}
		return false
	}
	return true
}

func validStatus(status Status, optional bool) bool {
	return (optional && status == "") || status == StatusSuccess || status == StatusError || status == StatusCanceled || status == StatusRejected
}

func validProtocol(protocol Protocol) bool {
	return protocol == ProtocolChat || protocol == ProtocolResponses || protocol == ProtocolMessages || protocol == ProtocolGemini
}

func validHTTPStatus(value int) bool { return value == 0 || (value >= 100 && value <= 599) }

func validTime(t time.Time) bool { return !t.IsZero() && t.Year() >= 1970 && t.Year() <= 9999 }

func validToken(value *int64) bool { return value == nil || (*value >= 0 && *value <= 1<<40) }

func validMetric(value *float64) bool {
	return value == nil || (!math.IsNaN(*value) && !math.IsInf(*value, 0) && *value >= 0 && *value <= 1e12)
}

func invalid(field string) error { return fmt.Errorf("%w: %s", ErrInvalidRecord, field) }

// normalizeRecord copies any fields it changes, so Append does not mutate a
// caller's attempt slice or times. Encoding then owns the immutable snapshot.
func normalizeRecord(r Record) (Record, error) {
	if !validCode(r.RequestID, 128, true) {
		return r, invalid("request_id")
	}
	if !validProtocol(r.Protocol) {
		return r, invalid("protocol")
	}
	if !validText(r.Model, 256, false) {
		return r, invalid("model")
	}
	if !validCode(r.KeyID, 128, false) || !validText(r.KeyName, 256, false) {
		return r, invalid("key identity")
	}
	if !validStatus(r.Status, false) || !validHTTPStatus(r.HTTPStatus) || !validCode(r.ErrorCode, 64, false) || !validCode(r.FinishReason, 64, false) {
		return r, invalid("status")
	}
	if !validTime(r.StartedAt) || !validTime(r.FinishedAt) || r.FinishedAt.Before(r.StartedAt) || !validTime(r.RecordedAt) {
		return r, invalid("timestamps")
	}
	if r.DurationMS < 0 || r.QueueMS < 0 || r.QueueMS > r.DurationMS {
		return r, invalid("durations")
	}
	if !validToken(r.TTFBMS) || (r.TTFBMS != nil && *r.TTFBMS > r.DurationMS) {
		return r, invalid("ttfb_ms")
	}
	if len(r.Attempts) > MaxAttempts || len(r.Decisions) > MaxDecisions {
		return r, invalid("attempt or decision count")
	}
	if r.DecisionCount == 0 {
		r.DecisionCount = len(r.Decisions)
	}
	if r.DecisionCount < len(r.Decisions) || r.DecisionCount < 0 || r.DecisionCount > MaxDecisionCount {
		return r, invalid("decision_count")
	}
	r.DecisionsTruncated = r.DecisionCount > len(r.Decisions)
	if r.AttemptCount == 0 && len(r.Attempts) > 0 {
		r.AttemptCount = r.Attempts[len(r.Attempts)-1].Number
	}
	if r.UpstreamStarted && r.AttemptCount == 0 {
		r.AttemptCount = 1
	}
	if r.AttemptCount < len(r.Attempts) || r.AttemptCount < 0 || r.AttemptCount > MaxAttemptCount {
		return r, invalid("attempt_count")
	}
	r.UpstreamStarted = r.UpstreamStarted || r.AttemptCount > 0
	r.AttemptsTruncated = r.AttemptCount > len(r.Attempts)
	r.StartedAt, r.FinishedAt, r.RecordedAt = r.StartedAt.UTC(), r.FinishedAt.UTC(), r.RecordedAt.UTC()
	r.Attempts = append([]Attempt{}, r.Attempts...)
	if r.Decisions == nil {
		r.Decisions = []Decision{}
	}
	previous := 0
	for i := range r.Attempts {
		a := &r.Attempts[i]
		if a.Number <= previous || a.Number > r.AttemptCount {
			return r, invalid("attempt number")
		}
		previous = a.Number
		if !validText(a.AccountID, 256, false) || !validText(a.AccountName, 256, false) || !validText(a.Model, 256, false) {
			return r, invalid("attempt identity")
		}
		if !validStatus(a.Status, false) || !validHTTPStatus(a.HTTPStatus) || !validCode(a.ErrorCode, 64, false) || !validCode(a.UsageReason, 64, false) || !validCode(a.FinishReason, 64, false) {
			return r, invalid("attempt status")
		}
		if a.DurationMS < 0 || a.QueueMS < 0 || a.QueueMS > a.DurationMS {
			return r, invalid("attempt durations")
		}
		if (!a.StartedAt.IsZero() && !validTime(a.StartedAt)) || (!a.FinishedAt.IsZero() && !validTime(a.FinishedAt)) || (!a.StartedAt.IsZero() && !a.FinishedAt.IsZero() && a.FinishedAt.Before(a.StartedAt)) {
			return r, invalid("attempt timestamps")
		}
		if !a.StartedAt.IsZero() {
			a.StartedAt = a.StartedAt.UTC()
		}
		if !a.FinishedAt.IsZero() {
			a.FinishedAt = a.FinishedAt.UTC()
		}
		if !validToken(a.InputTokens) || !validToken(a.OutputTokens) || !validToken(a.CachedTokens) || !validToken(a.ReasoningTokens) || !validMetric(a.Credit) {
			return r, invalid("attempt usage")
		}
		anyUsage := a.InputTokens != nil || a.OutputTokens != nil || a.CachedTokens != nil || a.ReasoningTokens != nil || a.Credit != nil
		if a.UsageState == "" {
			switch {
			case a.InputTokens != nil && a.OutputTokens != nil:
				a.UsageState = UsageComplete
			case anyUsage:
				a.UsageState = UsagePartial
			default:
				a.UsageState = UsageMissing
			}
		}
		switch a.UsageState {
		case UsageComplete:
			if a.InputTokens == nil || a.OutputTokens == nil {
				return r, invalid("complete usage lacks input/output")
			}
		case UsagePartial:
			if !anyUsage {
				return r, invalid("partial usage lacks observations")
			}
		case UsageMissing:
			if anyUsage {
				return r, invalid("missing usage includes observations")
			}
		default:
			return r, invalid("usage_state")
		}
	}
	for _, d := range r.Decisions {
		if d.Attempt < 0 || d.Attempt > MaxAttemptCount || !validCode(d.ReasonCode, 64, true) || !validText(d.AccountID, 256, false) || !validText(d.BoundAccountID, 256, false) || !validText(d.AccountName, 256, false) {
			return r, invalid("decision identity")
		}
		for _, code := range []string{d.Mode, d.AccountRealm, d.CostState, d.BlockedBy, d.SelectionMethod, d.FallbackKind, d.CostUnknownReason} {
			if !validCode(code, 64, false) {
				return r, invalid("decision code")
			}
		}
		if !validMetric(d.Weight) || !validMetric(d.Cost) || !validMetric(d.CostPer1K) || !validToken(d.WeightUnits) || !validToken(d.WeightTotal) {
			return r, invalid("decision metrics")
		}
		if d.WeightUnits != nil && d.WeightTotal != nil && *d.WeightUnits > *d.WeightTotal {
			return r, invalid("decision weight total")
		}
		if d.FallbackUntil != nil && !validTime(*d.FallbackUntil) {
			return r, invalid("decision fallback time")
		}
		if (!d.ObservedAt.IsZero() && !validTime(d.ObservedAt)) || (d.CostObservedAt != nil && !validTime(*d.CostObservedAt)) || d.CostSamples < 0 || d.CostSamples > 1<<30 {
			return r, invalid("decision cost observation")
		}
		for _, counts := range []map[string]int{d.StageCounts, d.ExcludedCounts} {
			if len(counts) > 32 {
				return r, invalid("decision counts")
			}
			for code, count := range counts {
				if !validCode(code, 64, true) || count < 0 {
					return r, invalid("decision count")
				}
			}
		}
	}
	return r, nil
}

func normalizeQuery(q Query) (Query, error) {
	if !validCode(q.KeyID, 128, false) || !validText(q.Model, 256, false) || !validCode(q.RequestID, 128, false) || !validStatus(q.Status, true) || q.Offset < 0 || q.Offset > DefaultMaxRecords || q.Limit < 0 {
		return q, ErrInvalidQuery
	}
	if q.Limit == 0 {
		q.Limit = 50
	}
	if q.Limit > MaxPageSize {
		q.Limit = MaxPageSize
	}
	return q, nil
}
