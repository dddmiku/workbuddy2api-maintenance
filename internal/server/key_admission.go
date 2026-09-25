// ═══ 更新日志 ═══
// 2026-09-25：四协议共用鉴权后的限流与排队，保持各自错误格式并在等待期间重新核对密钥。
package server

import (
	"context"
	"errors"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"workbuddy2api/internal/apikeys"
	"workbuddy2api/internal/keylimit"
	"workbuddy2api/internal/requestlog"
)

var errQueuedKeyInactive = errors.New("queued API key is no longer active")

func (h *Handler) withGeneration(protocol requestlog.Protocol, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		trace := traceFor(r)
		info, managed := requestKeyInfo(r)
		if trace != nil {
			trace.enabled = h.cfg.Requests != nil
			trace.record.Protocol = protocol
			if managed {
				trace.record.KeyID, trace.record.KeyName = info.ID, info.Name
			}
		}
		if managed && info.Limits != nil {
			start := time.Now()
			latest := info
			secret := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
			permit, err := h.cfg.KeyLimits.Acquire(r.Context(), info.ID, func() (keylimit.Policy, error) {
				value, status := h.cfg.APIKeys.Lookup(secret)
				if status != apikeys.StatusActive || value.ID != info.ID {
					return keylimit.Policy{}, errQueuedKeyInactive
				}
				latest = value
				if value.Limits == nil {
					return keylimit.Policy{}, nil
				}
				return *value.Limits, nil
			})
			if trace != nil {
				trace.record.QueueMS = max(0, time.Since(start).Milliseconds())
			}
			if err != nil {
				var limited *keylimit.LimitError
				switch {
				case errors.As(err, &limited):
					rateHeaders(w, limited.Snapshot)
					seconds := max(1, int((limited.RetryAfter+time.Second-1)/time.Second))
					w.Header().Set("Retry-After", strconv.Itoa(seconds))
					writeOpenAIError(w, 429, limited.Code, limitMessage(limited.Code))
				case errors.Is(err, errQueuedKeyInactive):
					writeOpenAIError(w, 401, "invalid_api_key", "the API key was disabled, deleted, or expired while waiting")
				case r.Context().Err() != nil:
					return
				default:
					log.Printf("WARN: [key-limits] admission unavailable: %v", err)
					writeOpenAIError(w, 503, "key_limits_unavailable", "key admission is temporarily unavailable; retry later")
				}
				return
			}
			rateHeaders(w, permit.Snapshot)
			if trace != nil {
				trace.permit = permit
			} else {
				defer permit.Release()
			}
			r = r.WithContext(context.WithValue(r.Context(), apiKeyContextKey{}, latest))
			if trace != nil {
				trace.record.KeyName = latest.Name
			}
		}
		next(w, r)
	}
}

func rateHeaders(w http.ResponseWriter, value keylimit.Snapshot) {
	if value.Policy.RequestsPerMinute > 0 {
		w.Header().Set("X-RateLimit-Limit", strconv.Itoa(value.Policy.RequestsPerMinute))
		w.Header().Set("X-RateLimit-Remaining", strconv.Itoa(value.Remaining))
	}
	if value.Policy.MaxConcurrent > 0 {
		w.Header().Set("X-Concurrency-Limit", strconv.Itoa(value.Policy.MaxConcurrent))
	}
}

func limitMessage(code string) string {
	switch code {
	case "key_rate_limit":
		return "this API key has reached its request rate limit"
	case "key_queue_timeout":
		return "this API key's bounded concurrency queue timed out"
	case "key_queue_full":
		return "this API key's concurrency queue is full"
	default:
		return "this API key has reached its concurrency limit"
	}
}

func (h *Handler) keyLimitStatus(w http.ResponseWriter, r *http.Request) {
	policies := map[string]keylimit.Policy{}
	if h.cfg.APIKeys != nil {
		for _, info := range h.cfg.APIKeys.List() {
			if info.Limits != nil {
				policies[info.ID] = *info.Limits
			} else {
				policies[info.ID] = keylimit.Policy{}
			}
		}
	}
	states, err := h.cfg.KeyLimits.Snapshot(r.Context(), policies)
	if err != nil {
		_ = writeJSON(w, 503, map[string]any{"ok": false, "message": "限流状态暂不可读，请稍后刷新"})
		return
	}
	_ = writeJSON(w, 200, map[string]any{"ok": true, "keys": states, "max_requests_per_minute": keylimit.MaxRequestsPerMinute, "max_concurrent": keylimit.MaxConcurrent, "max_queue_seconds": keylimit.MaxQueueSeconds, "max_queued_per_key": keylimit.MaxQueuedPerKey})
}
