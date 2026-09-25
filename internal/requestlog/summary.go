// ═══ 更新日志 ═══
// 2026-09-25：列表仅提供有界摘要，逐次汇总已知计量并显式标明未知或尚未调用上游。
// 2026-09-25：摘要显示真实调度次数及截断状态，末条由采集端保留为真正最后一次调度。
package requestlog

import "unicode/utf8"

func summarize(r Record) Summary {
	s := Summary{
		RequestID: r.RequestID, Protocol: r.Protocol, Model: r.Model,
		KeyID: r.KeyID, KeyName: r.KeyName, StartedAt: r.StartedAt,
		FinishedAt: r.FinishedAt, RecordedAt: r.RecordedAt, Status: r.Status,
		HTTPStatus: r.HTTPStatus, DurationMS: r.DurationMS, QueueMS: r.QueueMS,
		Stream: r.Stream, ErrorCode: r.ErrorCode, UpstreamStarted: r.UpstreamStarted,
		FinishReason: r.FinishReason,
		AttemptCount: r.AttemptCount, StoredAttempts: len(r.Attempts),
		AttemptsTruncated: r.AttemptsTruncated, DecisionCount: r.DecisionCount,
		DecisionsTruncated: r.DecisionsTruncated,
		UsageState:         UsageNotStarted,
	}
	if r.TTFBMS != nil {
		value := *r.TTFBMS
		s.TTFBMS = &value
	}
	if len(r.Decisions) > 0 {
		d := r.Decisions[len(r.Decisions)-1]
		accountID := d.AccountID
		truncated := d.AccountIDTruncated
		if len(accountID) > 64 {
			accountID, truncated = accountID[:64], true
			for !utf8.ValidString(accountID) {
				accountID = accountID[:len(accountID)-1]
			}
		}
		s.LastDecision = &DecisionSummary{Attempt: d.Attempt, ReasonCode: d.ReasonCode, SelectionMethod: d.SelectionMethod, BlockedBy: d.BlockedBy, AccountID: accountID, AccountIDTruncated: truncated, AccountRealm: d.AccountRealm, ObservedAt: d.ObservedAt}
	}
	if !r.UpstreamStarted {
		return s
	}
	missing := r.AttemptCount - len(r.Attempts)
	s.UnknownAttempts, s.MissingInputAttempts, s.MissingOutputAttempts = missing, missing, missing
	s.MissingCachedAttempts, s.MissingReasoningAttempts, s.MissingCreditAttempts = missing, missing, missing
	for _, a := range r.Attempts {
		if a.UsageState != UsageComplete {
			s.UnknownAttempts++
		}
		addKnown(&s.InputTokens, a.InputTokens, &s.MissingInputAttempts)
		addKnown(&s.OutputTokens, a.OutputTokens, &s.MissingOutputAttempts)
		addKnown(&s.CachedTokens, a.CachedTokens, &s.MissingCachedAttempts)
		addKnown(&s.ReasoningTokens, a.ReasoningTokens, &s.MissingReasoningAttempts)
		if a.Credit == nil {
			s.MissingCreditAttempts++
		} else {
			if s.Credit == nil {
				s.Credit = new(float64)
			}
			*s.Credit += *a.Credit
		}
	}
	switch {
	case s.UnknownAttempts == 0 && r.AttemptCount > 0:
		s.UsageState = UsageComplete
	case s.InputTokens != nil || s.OutputTokens != nil || s.CachedTokens != nil || s.ReasoningTokens != nil || s.Credit != nil:
		s.UsageState = UsagePartial
	default:
		s.UsageState = UsageMissing
	}
	return s
}

func addKnown(total **int64, value *int64, missing *int) {
	if value == nil {
		*missing++
		return
	}
	if *total == nil {
		*total = new(int64)
	}
	**total += *value
}
