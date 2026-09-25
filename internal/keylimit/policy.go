// ═══ 更新日志 ═══
// 2026-09-25：密钥频率、并发和有界排队共用校验契约，缺省保持无限制。
package keylimit

import (
	"errors"
	"time"
)

const (
	MaxRequestsPerMinute = 60000
	MaxConcurrent        = 256
	MaxQueueSeconds      = 30
	MaxQueuedPerKey      = 32
	MaxQueuedTotal       = 512
	MaxRunningTotal      = 4096
)

var (
	ErrPolicy   = errors.New("invalid key limit policy")
	ErrClosed   = errors.New("key limit manager is closed")
	ErrCapacity = errors.New("key limit state capacity exceeded")
)

type Policy struct {
	RequestsPerMinute   int `json:"requests_per_minute"`
	MaxConcurrent       int `json:"max_concurrent"`
	QueueTimeoutSeconds int `json:"queue_timeout_seconds"`
}

func (p Policy) Validate() error {
	if p.RequestsPerMinute < 0 || p.RequestsPerMinute > MaxRequestsPerMinute ||
		p.MaxConcurrent < 0 || p.MaxConcurrent > MaxConcurrent ||
		p.QueueTimeoutSeconds < 0 || p.QueueTimeoutSeconds > MaxQueueSeconds ||
		(p.QueueTimeoutSeconds > 0 && p.MaxConcurrent == 0) {
		return ErrPolicy
	}
	return nil
}

func (p Policy) Unlimited() bool { return p.RequestsPerMinute == 0 && p.MaxConcurrent == 0 }

// Conservative one-second buckets retain admission for 60–61 seconds. This
// bounds storage without exceeding the configured rolling 60-second count.
type Snapshot struct {
	Limited           bool   `json:"limited"`
	Requests          int    `json:"requests"`
	Remaining         int    `json:"remaining"`
	Active            int    `json:"active"`
	Queued            int    `json:"queued"`
	RetryAfterSeconds int    `json:"retry_after_seconds"`
	Policy            Policy `json:"policy"`
}

type LimitError struct {
	Code       string
	RetryAfter time.Duration
	Snapshot   Snapshot
}

func (e *LimitError) Error() string { return e.Code }

// Reevaluate live policy while queued. Revocation and tighter limits must not
// be bypassed by a waiter that captured an older key record.
type Resolver func() (Policy, error)
