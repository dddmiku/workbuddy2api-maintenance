package trae

import (
	"encoding/json"
	"io"
	"strings"
	"testing"
)

func reviewFinalUsage(t *testing.T, model, usageEvents string, bill *SessionCredit) map[string]any {
	t.Helper()
	frames := "event: output\ndata: {\"response\":\"fixture-response\"}\n\n" + usageEvents + "event: done\ndata: {}\n\n"
	var spec *creditSpec
	if bill != nil {
		spec = &creditSpec{selection: outboundSelection{BillingModel: "deepseek-v4.1-flash"}, scope: "review-fixture", turnID: "review-turn", fetcher: func() (SessionCredit, bool) { return *bill, true }}
	}
	r := translateStreamWithCredit(io.NopCloser(strings.NewReader(frames)), model, nil, spec)
	defer r.Close()
	data, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	var usage map[string]any
	for _, line := range strings.Split(string(data), "\n") {
		if !strings.HasPrefix(line, "data: {") {
			continue
		}
		var obj map[string]any
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &obj); err != nil {
			t.Fatal(err)
		}
		if got, ok := obj["usage"].(map[string]any); ok {
			usage = got
		}
	}
	return usage
}

func reviewUsageEvent(raw string) string { return "event: token_usage\ndata: " + raw + "\n\n" }

func reviewCacheRead(usage map[string]any) float64 {
	details, _ := usage["prompt_tokens_details"].(map[string]any)
	v, _ := details["cached_tokens"].(float64)
	return v
}

func TestRegressionK4LegacyIntegerValuedNumbers(t *testing.T) {
	for _, tc := range []struct{ name, raw string }{
		{"plain_integer_control", `{"prompt_tokens":100,"completion_tokens":2,"cache_read_input_tokens":80}`},
		{"integer_valued_decimal_exponent", `{"prompt_tokens":100.0,"completion_tokens":2e0,"cache_read_input_tokens":8e1}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			u := reviewFinalUsage(t, "deepseek-v4.1-flash", reviewUsageEvent(tc.raw), nil)
			if u["prompt_tokens"] != float64(100) || u["completion_tokens"] != float64(2) || reviewCacheRead(u) != 80 {
				t.Fatalf("integer-valued JSON counters were not preserved: got=%v; want input=100 output=2 cache_read=80", u)
			}
		})
	}
}

func TestRegressionK5CrossSnapshotCacheConservation(t *testing.T) {
	events := reviewUsageEvent(`{"prompt_tokens":100,"completion_tokens":2,"cache_read_input_tokens":80,"cache_creation_input_tokens":20}`) +
		reviewUsageEvent(`{"prompt_tokens":100,"completion_tokens":2,"cache_read_input_tokens":0,"cache_creation_input_tokens":100}`)
	bill := matchedCacheBill(100, 2, 0, 100)
	u := reviewFinalUsage(t, "deepseek-v4.1-flash", events, &bill)
	input, _ := u["prompt_tokens"].(float64)
	write, _ := u["cache_creation_input_tokens"].(float64)
	read := reviewCacheRead(u)
	if read+write > input {
		t.Fatalf("merged cache counters exceed input: input=%.0f read=%.0f write=%.0f usage=%v", input, read, write, u)
	}
	if read != 0 || write != 100 || input != 100 {
		t.Fatal("conflicting snapshots must select the complete latest observed pair")
	}
}

func TestCacheSnapshotPairAndPartialBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name, first, middle, last string
		read, write               float64
		blocked                   bool
	}{
		{"reverse-conflict", `{"prompt_tokens":100,"completion_tokens":2,"cache_read_input_tokens":20,"cache_creation_input_tokens":80}`, `{}`, `{"prompt_tokens":100,"completion_tokens":2,"cache_read_input_tokens":100,"cache_creation_input_tokens":0}`, 100, 0, true},
		{"zero-pair", `{"prompt_tokens":100,"completion_tokens":2,"cache_read_input_tokens":8,"cache_creation_input_tokens":8}`, `{"reasoning_tokens":1}`, `{"prompt_tokens":100,"completion_tokens":2,"cache_read_input_tokens":0,"cache_creation_input_tokens":0}`, 8, 8, false},
		{"changed-denominator", `{"prompt_tokens":100,"completion_tokens":2,"cache_read_input_tokens":0}`, `{"prompt_tokens":101}`, `{"prompt_tokens":100,"completion_tokens":2,"cache_read_input_tokens":0}`, 0, 0, true},
		{"invalid-denominator", `{"prompt_tokens":100,"completion_tokens":2,"cache_read_input_tokens":0}`, `{"prompt_tokens":100.000000000000001}`, `{"prompt_tokens":100,"completion_tokens":2,"cache_read_input_tokens":0}`, 0, 0, true},
		{"oversized-pair", `{"prompt_tokens":100,"completion_tokens":2}`, `{}`, `{"prompt_tokens":100,"completion_tokens":2,"cache_read_input_tokens":80,"cache_creation_input_tokens":80}`, 0, 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tr := newTranslator("deepseek-v4.1-flash")
			tr.billingIdentity = outboundSelection{BillingModel: "deepseek-v4.1-flash"}
			for _, raw := range []string{tc.first, tc.middle, tc.last} {
				tr.recordEventUsage([]byte(raw), true)
			}
			tr.reconcileBilledCache(matchedCacheBill(100, 2, 10, 0))
			details, _ := tr.usage["prompt_tokens_details"].(map[string]any)
			read, _ := usageCount(details["cached_tokens"])
			write, _ := usageCount(tr.usage["cache_creation_input_tokens"])
			if float64(read) != tc.read || float64(write) != tc.write || tr.usageCountsChanged != tc.blocked {
				t.Fatalf("pair=%d/%d blocked=%t", read, write, tr.usageCountsChanged)
			}
			if tc.name == "zero-pair" {
				details, _ := tr.usage["completion_tokens_details"].(map[string]any)
				if details["reasoning_tokens"] != int64(1) {
					t.Fatal("partial reasoning observation lost")
				}
			}
		})
	}
}

func TestCachePairBeforeDenominatorCannotExceedLaterInput(t *testing.T) {
	tr := newTranslator("deepseek-v4.1-flash")
	tr.billingIdentity = outboundSelection{BillingModel: "deepseek-v4.1-flash"}
	tr.recordEventUsage([]byte(`{"cache_read_input_tokens":80,"cache_creation_input_tokens":20}`), true)
	tr.recordEventUsage([]byte(`{"prompt_tokens":50,"completion_tokens":2}`), true)
	details, _ := tr.usage["prompt_tokens_details"].(map[string]any)
	if _, ok := details["cached_tokens"]; ok || tr.usage["cache_creation_input_tokens"] != nil || !tr.usageCountsChanged {
		t.Fatalf("pre-denominator cache pair survived incompatible input: usage=%v", tr.usage)
	}
}

func TestRegressionK8RemotePreservesInvalidFractionalDenominator(t *testing.T) {
	raw := `{"prompt_tokens":100.000000000000001,"completion_tokens":2,"cache_read_input_tokens":0}`
	converted, ok := (&remoteEventConverter{}).convert("token_usage", raw)
	if !ok {
		t.Fatal("converter unexpectedly rejected the whole fixture")
	}
	for _, tc := range []struct{ name, events string }{{"legacy_control", reviewUsageEvent(raw)}, {"remote", converted}} {
		t.Run(tc.name, func(t *testing.T) {
			bill := matchedCacheBill(100, 2, 80, 0)
			u := reviewFinalUsage(t, "deepseek-v4.1-flash", tc.events, &bill)
			if u["prompt_tokens"] == float64(100) || reviewCacheRead(u) == 80 {
				t.Fatalf("fractional source denominator rounded into matched bill attribution: usage=%v", u)
			}
		})
	}
}
