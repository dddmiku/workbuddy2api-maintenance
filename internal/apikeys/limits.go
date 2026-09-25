// ═══ 更新日志 ═══
// 2026-09-25：限流策略使用独立副本，显式null或全零可恢复无限制，未知字段不静默忽略。
package apikeys

import (
	"bytes"
	"encoding/json"
	"io"

	"workbuddy2api/internal/keylimit"
)

func copyLimits(value *keylimit.Policy) *keylimit.Policy {
	if value == nil || value.Unlimited() {
		return nil
	}
	copy := *value
	return &copy
}

func parseLimits(raw json.RawMessage) (*keylimit.Policy, error) {
	if len(raw) == 0 || string(bytes.TrimSpace(raw)) == "null" {
		return nil, nil
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	start, err := decoder.Token()
	if err != nil || start != json.Delim('{') {
		return nil, ErrInvalidLimits
	}
	var policy keylimit.Policy
	seen := map[string]bool{}
	for decoder.More() {
		token, err := decoder.Token()
		name, ok := token.(string)
		if err != nil || !ok || seen[name] {
			return nil, ErrInvalidLimits
		}
		seen[name] = true
		var number *int
		if decoder.Decode(&number) != nil || number == nil {
			return nil, ErrInvalidLimits
		}
		switch name {
		case "requests_per_minute":
			policy.RequestsPerMinute = *number
		case "max_concurrent":
			policy.MaxConcurrent = *number
		case "queue_timeout_seconds":
			policy.QueueTimeoutSeconds = *number
		default:
			return nil, ErrInvalidLimits
		}
	}
	end, err := decoder.Token()
	if err != nil || end != json.Delim('}') || decoder.Decode(new(any)) != io.EOF || policy.Validate() != nil {
		return nil, ErrInvalidLimits
	}
	return copyLimits(&policy), nil
}
