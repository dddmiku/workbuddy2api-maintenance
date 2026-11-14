package trae

import (
	"bytes"
	"encoding/json"
	"math"
)

// Decode only usage with exact numbers. The other event fields retain their
// existing types (in particular float64 tool indexes).
func (t *translator) recordEventUsage(data []byte, flat bool) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var obj map[string]any
	if decoder.Decode(&obj) != nil {
		return
	}
	if usage, ok := obj["usage"].(map[string]any); ok {
		t.recordUsage(usage)
	} else if flat {
		t.recordUsage(obj)
	}
}

// recordUsage merges partial counters, but cache read/write remain one source
// observation tied to its denominators. Conflicting positive observations use
// the latest complete pair rather than manufacturing a per-field combination.
func (t *translator) recordUsage(raw map[string]any) {
	mapped := mapUsage(raw)
	if t.usage == nil {
		t.usage = make(map[string]any)
	}
	denominatorChanged := false
	for _, field := range []struct {
		key     string
		aliases []string
	}{
		{"prompt_tokens", []string{"prompt_tokens", "input_token", "input_tokens", "promptTokenCount"}},
		{"completion_tokens", []string{"completion_tokens", "output_token", "output_tokens", "candidatesTokenCount"}},
	} {
		value, provided, valid := rawUsageCount(raw, field.aliases...)
		if !provided {
			continue
		}
		if !valid {
			t.usageCountsChanged = true
			denominatorChanged = true
			delete(t.usage, field.key)
			delete(mapped, field.key)
			continue
		}
		if t.usageCounts == nil {
			t.usageCounts = make(map[string]int64)
		}
		if previous, seen := t.usageCounts[field.key]; seen && previous != value {
			t.usageCountsChanged = true
			denominatorChanged = true
		}
		t.usageCounts[field.key] = value
		t.usage[field.key] = value
	}
	if denominatorChanged {
		delete(t.usage, "prompt_tokens_details")
		delete(t.usage, "cache_creation_input_tokens")
	}
	// Partial reasoning updates must not erase independent earlier observations.
	for key, value := range mapped {
		if key == "prompt_tokens_details" || key == "cache_creation_input_tokens" ||
			key == "prompt_tokens" || key == "completion_tokens" || key == "total_tokens" {
			continue
		}
		t.usage[key] = value
	}
	input, inputOK := usageCount(t.usage["prompt_tokens"])
	output, outputOK := usageCount(t.usage["completion_tokens"])
	if inputOK && outputOK && input <= math.MaxInt64-output {
		t.usage["total_tokens"] = input + output
	} else {
		delete(t.usage, "total_tokens")
	}
	if total, provided, valid := rawUsageCount(raw, "total_tokens", "totalTokenCount"); provided && valid {
		t.usage["total_tokens"] = total
	}
	// A cache pair may arrive before its input denominator. Once a denominator
	// appears or changes, reject any stored pair that cannot belong to it.
	if inputOK {
		oldDetails, _ := t.usage["prompt_tokens_details"].(map[string]any)
		oldRead, _ := usageCount(oldDetails["cached_tokens"])
		oldWrite, _ := usageCount(t.usage["cache_creation_input_tokens"])
		if oldRead > input || oldWrite > input-oldRead {
			t.usageCountsChanged = true
			delete(t.usage, "prompt_tokens_details")
			delete(t.usage, "cache_creation_input_tokens")
		}
	}

	read, readPresent, readValid := rawUsageCount(raw, "cached_tokens", "cache_read_input_tokens", "prompt_cache_hit_tokens")
	if details, ok := raw["prompt_tokens_details"].(map[string]any); ok {
		if value, present, valid := rawUsageCount(details, "cached_tokens"); present {
			read, readPresent, readValid = value, true, valid
		}
	}
	write, writePresent, writeValid := rawUsageCount(raw, "cache_creation_input_tokens")
	if !readPresent && !writePresent {
		return
	}
	if (readPresent && !readValid) || (writePresent && !writeValid) ||
		(inputOK && (read > input || write > input-read)) {
		t.usageCountsChanged = true
		delete(t.usage, "prompt_tokens_details")
		delete(t.usage, "cache_creation_input_tokens")
		return
	}
	oldDetails, _ := t.usage["prompt_tokens_details"].(map[string]any)
	oldRead, _ := usageCount(oldDetails["cached_tokens"])
	oldWrite, _ := usageCount(t.usage["cache_creation_input_tokens"])
	oldPositive := oldRead > 0 || oldWrite > 0
	newPositive := read > 0 || write > 0
	conflict := oldPositive && newPositive &&
		((read > 0 && read != oldRead) || (write > 0 && write != oldWrite))
	if conflict {
		t.usageCountsChanged = true
	}
	if oldPositive && !conflict {
		// Missing/zero re-reports cannot erase earlier positive evidence. Keep
		// the pair atomically, including zeros/missing fields from that source.
		return
	}
	delete(t.usage, "prompt_tokens_details")
	delete(t.usage, "cache_creation_input_tokens")
	if readPresent {
		t.usage["prompt_tokens_details"] = map[string]any{"cached_tokens": read}
	}
	if writePresent {
		t.usage["cache_creation_input_tokens"] = write
	}
}

// Presence is checked before normalization so explicit invalid denominators
// cannot borrow a previous valid value as if the current event were partial.
func rawUsageCount(raw map[string]any, keys ...string) (int64, bool, bool) {
	for _, key := range keys {
		if value, present := raw[key]; present {
			n, valid := usageCount(value)
			return n, true, valid
		}
	}
	return 0, false, false
}

// reconcileBilledCache repairs absent/zero SSE cache fields only when the bill
// belongs to this request and both token denominators match exactly. It never
// turns an aggregate bill into a per-call estimate or changes token totals.
func (t *translator) reconcileBilledCache(bill SessionCredit) {
	if t.usageCountsChanged || !bill.SessionMatched || !bill.CacheAttributionSafe ||
		!bill.InputTokensPresent || !bill.InputTokensValid ||
		!bill.OutputTokensPresent || !bill.OutputTokensValid ||
		!t.billingIdentity.matchesBill(bill.ModelName) {
		return
	}
	input, inputOK := usageCount(t.usage["prompt_tokens"])
	output, outputOK := usageCount(t.usage["completion_tokens"])
	if !inputOK || !outputOK || input != bill.InputTokens || output != bill.OutputTokens {
		return
	}
	details, _ := t.usage["prompt_tokens_details"].(map[string]any)
	read, readPresent := usageCount(details["cached_tokens"])
	write, writePresent := usageCount(t.usage["cache_creation_input_tokens"])
	if read > input || write > input {
		return
	}
	setRead, setWrite := false, false
	if (!readPresent || read == 0) && bill.CacheReadTokens.Present && bill.CacheReadTokens.Valid {
		read = bill.CacheReadTokens.Value
		setRead = true
	}
	if (!writePresent || write == 0) && bill.CacheWriteTokens.Present && bill.CacheWriteTokens.Valid {
		write = bill.CacheWriteTokens.Value
		setWrite = true
	}
	if read < 0 || write < 0 || read > input || write > input-read {
		return
	}
	if setRead {
		if details == nil {
			details = make(map[string]any)
			t.usage["prompt_tokens_details"] = details
		}
		details["cached_tokens"] = read
	}
	if setWrite {
		t.usage["cache_creation_input_tokens"] = write
	}
}
