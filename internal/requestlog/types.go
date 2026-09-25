// ═══ 更新日志 ═══
// 2026-09-25：新增有界请求消费明细接口，缺失计量保留 null，禁止记录请求正文或上游错误正文。
// 2026-09-25：单独记录真实调度总数和截断状态，避免64条保留上限掩盖完整调度历史。
package requestlog

import (
	"errors"
	"time"
)

const (
	DefaultMaxRecords = 10000
	DefaultMaxAge     = 7 * 24 * time.Hour
	DefaultMaxBytes   = int64(64 << 20)
	MaxRecordBytes    = 64 << 10
	MaxAttempts       = 64
	MaxAttemptCount   = 4096
	MaxDecisions      = 64
	MaxDecisionCount  = 4096
	MaxPageSize       = 100
)

var (
	ErrClosed         = errors.New("request log is closed")
	ErrNotFound       = errors.New("request record not found")
	ErrDuplicate      = errors.New("request record already exists")
	ErrInvalidRecord  = errors.New("invalid request record")
	ErrInvalidQuery   = errors.New("invalid request log query")
	ErrRecordTooLarge = errors.New("request record exceeds size limit")
	ErrCorrupt        = errors.New("request log is corrupt")
	ErrUnsafePath     = errors.New("request log path is not a regular private file")
)

type Protocol string

const (
	ProtocolChat      Protocol = "chat_completions"
	ProtocolResponses Protocol = "responses"
	ProtocolMessages  Protocol = "anthropic_messages"
	ProtocolGemini    Protocol = "gemini_generate_content"
)

// Status is the final request/attempt result; no in-flight snapshots are stored.
type Status string

const (
	StatusSuccess  Status = "success"
	StatusError    Status = "error"
	StatusCanceled Status = "canceled"
	StatusRejected Status = "rejected"
)

type UsageState string

const (
	// Complete means the upstream reported both input and output token counts.
	// Optional cache/reasoning/credit fields can still be unknown (null).
	UsageComplete   UsageState = "complete"
	UsagePartial    UsageState = "partial"
	UsageMissing    UsageState = "missing"
	UsageNotStarted UsageState = "not_started"
)

// Options can tighten the built-in retention ceilings. Zero selects the default.
// All processes sharing a path must use the same options.
type Options struct {
	MaxRecords int
	MaxAge     time.Duration
	MaxBytes   int64
}

// Record contains safe metadata for exactly one client request. Model must be
// the full canonical model name, and RequestID must be the real gateway ID.
// KeyID identifies a managed key; it must never contain the key's secret value.
// RecordedAt is assigned by Append and is the retention clock, independently of
// request duration. Callers must never place arbitrary error text in ErrorCode.
type Record struct {
	RequestID          string     `json:"request_id"`
	Protocol           Protocol   `json:"protocol"`
	Model              string     `json:"model"`
	KeyID              string     `json:"key_id"`
	KeyName            string     `json:"key_name"`
	StartedAt          time.Time  `json:"started_at"`
	FinishedAt         time.Time  `json:"finished_at"`
	RecordedAt         time.Time  `json:"recorded_at"`
	Status             Status     `json:"status"`
	HTTPStatus         int        `json:"http_status,omitempty"`
	DurationMS         int64      `json:"duration_ms"`
	QueueMS            int64      `json:"queue_ms"`
	TTFBMS             *int64     `json:"ttfb_ms"`
	Stream             bool       `json:"stream"`
	ErrorCode          string     `json:"error_code,omitempty"`
	FinishReason       string     `json:"finish_reason,omitempty"`
	UpstreamStarted    bool       `json:"upstream_started"`
	AttemptCount       int        `json:"attempt_count"`
	AttemptsTruncated  bool       `json:"attempts_truncated"`
	DecisionCount      int        `json:"decision_count"`
	DecisionsTruncated bool       `json:"decisions_truncated"`
	Attempts           []Attempt  `json:"attempts"`
	Decisions          []Decision `json:"decisions"`
}

// Attempt retains each upstream attempt, including failed retries whose usage
// was never reported. A nil metric is unknown, while a pointer to 0 is known zero.
// UsageReason and ErrorCode are stable machine codes, never upstream messages.
type Attempt struct {
	Number          int        `json:"number"`
	AccountID       string     `json:"account_id,omitempty"`
	AccountName     string     `json:"account_name,omitempty"`
	Model           string     `json:"model,omitempty"`
	StartedAt       time.Time  `json:"started_at,omitempty"`
	FinishedAt      time.Time  `json:"finished_at,omitempty"`
	DurationMS      int64      `json:"duration_ms"`
	QueueMS         int64      `json:"queue_ms"`
	Status          Status     `json:"status"`
	HTTPStatus      int        `json:"http_status,omitempty"`
	ErrorCode       string     `json:"error_code,omitempty"`
	FinishReason    string     `json:"finish_reason,omitempty"`
	InputTokens     *int64     `json:"input_tokens"`
	OutputTokens    *int64     `json:"output_tokens"`
	CachedTokens    *int64     `json:"cached_tokens"`
	ReasoningTokens *int64     `json:"reasoning_tokens"`
	Credit          *float64   `json:"credit"`
	UsageState      UsageState `json:"usage_state"`
	UsageReason     string     `json:"usage_reason,omitempty"`
}

// Decision is captured at the scheduler's actual selection seam, without
// reevaluating the pool. Counts and reason codes describe bounded facts only.
type Decision struct {
	Attempt              int            `json:"attempt"`
	Mode                 string         `json:"mode,omitempty"`
	ObservedAt           time.Time      `json:"observed_at,omitempty"`
	ReasonCode           string         `json:"reason_code"`
	StageCounts          map[string]int `json:"stage_counts,omitempty"`
	ExcludedCounts       map[string]int `json:"excluded_counts,omitempty"`
	AccountID            string         `json:"account_id,omitempty"`
	AccountName          string         `json:"account_name,omitempty"`
	AccountRealm         string         `json:"account_realm,omitempty"`
	AccountIDTruncated   bool           `json:"account_id_truncated,omitempty"`
	BoundAccountID       string         `json:"bound_account_id,omitempty"`
	CostState            string         `json:"cost_state,omitempty"`
	CostPer1K            *float64       `json:"cost_per_1k"`
	CostObservedAt       *time.Time     `json:"cost_observed_at,omitempty"`
	CostSamples          int            `json:"cost_samples,omitempty"`
	CostUnknownReason    string         `json:"cost_unknown_reason,omitempty"`
	CostUsedForSelection bool           `json:"cost_used_for_selection"`
	BlockedBy            string         `json:"blocked_by,omitempty"`
	SelectionMethod      string         `json:"selection_method,omitempty"`
	WeightUnits          *int64         `json:"weight_units"`
	WeightTotal          *int64         `json:"weight_total"`
	FallbackKind         string         `json:"fallback_kind,omitempty"`
	FallbackUntil        *time.Time     `json:"fallback_until,omitempty"`
	Weight               *float64       `json:"weight"`
	Cost                 *float64       `json:"cost"`
}

// Query combines exact filters with AND. Results are newest ingestion first.
// Offset pagination is a current snapshot, not a cursor over a frozen history.
type Query struct {
	KeyID     string
	Model     string
	Status    Status
	RequestID string
	Offset    int
	Limit     int
}

type Page struct {
	Items    []Summary     `json:"items"`
	Total    int           `json:"total"`
	Offset   int           `json:"offset"`
	Limit    int           `json:"limit"`
	Recovery *RecoveryInfo `json:"recovery,omitempty"`
}

// Summary is deliberately small: the management channel is bounded to 1 MiB,
// so attempt/decision arrays are available through Get only. Metrics are sums
// of observed values, never estimates for attempts that did not report usage.
type Summary struct {
	RequestID                string           `json:"request_id"`
	Protocol                 Protocol         `json:"protocol"`
	Model                    string           `json:"model"`
	KeyID                    string           `json:"key_id"`
	KeyName                  string           `json:"key_name"`
	StartedAt                time.Time        `json:"started_at"`
	FinishedAt               time.Time        `json:"finished_at"`
	RecordedAt               time.Time        `json:"recorded_at"`
	Status                   Status           `json:"status"`
	HTTPStatus               int              `json:"http_status,omitempty"`
	DurationMS               int64            `json:"duration_ms"`
	QueueMS                  int64            `json:"queue_ms"`
	TTFBMS                   *int64           `json:"ttfb_ms"`
	Stream                   bool             `json:"stream"`
	ErrorCode                string           `json:"error_code,omitempty"`
	FinishReason             string           `json:"finish_reason,omitempty"`
	UpstreamStarted          bool             `json:"upstream_started"`
	AttemptCount             int              `json:"attempt_count"`
	StoredAttempts           int              `json:"stored_attempts"`
	AttemptsTruncated        bool             `json:"attempts_truncated"`
	DecisionCount            int              `json:"decision_count"`
	DecisionsTruncated       bool             `json:"decisions_truncated"`
	LastDecision             *DecisionSummary `json:"last_decision,omitempty"`
	InputTokens              *int64           `json:"input_tokens"`
	OutputTokens             *int64           `json:"output_tokens"`
	CachedTokens             *int64           `json:"cached_tokens"`
	ReasoningTokens          *int64           `json:"reasoning_tokens"`
	Credit                   *float64         `json:"credit"`
	UsageState               UsageState       `json:"usage_state"`
	UnknownAttempts          int              `json:"unknown_attempts"`
	MissingInputAttempts     int              `json:"missing_input_attempts"`
	MissingOutputAttempts    int              `json:"missing_output_attempts"`
	MissingCachedAttempts    int              `json:"missing_cached_attempts"`
	MissingReasoningAttempts int              `json:"missing_reasoning_attempts"`
	MissingCreditAttempts    int              `json:"missing_credit_attempts"`
}

// DecisionSummary keeps the last scheduler outcome visible without carrying
// per-stage maps into the list. A shortened ID is explicitly marked; Get
// retains the full bounded ID emitted by the scheduler.
type DecisionSummary struct {
	Attempt            int       `json:"attempt"`
	ReasonCode         string    `json:"reason_code"`
	SelectionMethod    string    `json:"selection_method,omitempty"`
	BlockedBy          string    `json:"blocked_by,omitempty"`
	AccountID          string    `json:"account_id,omitempty"`
	AccountIDTruncated bool      `json:"account_id_truncated,omitempty"`
	AccountRealm       string    `json:"account_realm,omitempty"`
	ObservedAt         time.Time `json:"observed_at,omitempty"`
}

// RecoveryInfo makes automatic repair of an incomplete/corrupt final frame
// visible. It never contains bytes from that frame. Middle corruption fails
// closed and is not repaired automatically.
type RecoveryInfo struct {
	Count              int64     `json:"count"`
	DiscardedTailBytes int64     `json:"discarded_tail_bytes"`
	LastRecoveredAt    time.Time `json:"last_recovered_at"`
}
