package trae

import (
	"fmt"
	"io"
	"strings"
	"testing"
)

func matchedCacheBill(input, output, read, write int64) SessionCredit {
	return SessionCredit{
		SessionMatched: true, CacheAttributionSafe: true,
		Credits: 0.06, ModelName: "DeepSeek-V4.1-Flash",
		InputTokens: input, InputTokensPresent: true, InputTokensValid: true,
		OutputTokens: output, OutputTokensPresent: true, OutputTokensValid: true,
		CacheReadTokens:  SessionCacheCount{Value: read, Present: true, Valid: true},
		CacheWriteTokens: SessionCacheCount{Value: write, Present: true, Valid: true},
	}
}

func TestMatchedBillRepairsReportedZeroCache(t *testing.T) {
	for _, tc := range []struct{ input, output, cached int64 }{
		{60762, 126, 2176}, {61047, 2, 60672},
	} {
		tr := newTranslator("trae:deepseek-v4.1-flash")
		tr.billingIdentity = outboundSelection{BillingModel: "deepseek-v4.1-flash"}
		tr.usage = mapUsage(map[string]any{"prompt_tokens": tc.input, "completion_tokens": tc.output,
			"cache_read_input_tokens": int64(0), "cache_creation_input_tokens": int64(0)})
		tr.withCredit("fixture", "fixture-turn", func() (SessionCredit, bool) {
			return matchedCacheBill(tc.input, tc.output, tc.cached, 0), true
		})
		tr.applyCredit()
		details, _ := tr.usage["prompt_tokens_details"].(map[string]any)
		if details["cached_tokens"] != tc.cached {
			t.Fatalf("matched billing cache was not reflected: got=%v want=%d", details["cached_tokens"], tc.cached)
		}
		if tr.usage["prompt_tokens"] != tc.input || tr.usage["completion_tokens"] != tc.output || tr.usage["total_tokens"] != tc.input+tc.output {
			t.Fatal("billing reconciliation changed token totals")
		}
	}
}

func TestBillCacheRequiresSameCallAndValidCounters(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*SessionCredit)
	}{
		{"unmatched session", func(b *SessionCredit) { b.SessionMatched = false }},
		{"ambiguous details", func(b *SessionCredit) { b.CacheAttributionSafe = false }},
		{"aggregated input", func(b *SessionCredit) { b.InputTokens *= 2 }},
		{"different output", func(b *SessionCredit) { b.OutputTokens++ }},
		{"missing input", func(b *SessionCredit) { b.InputTokensPresent = false }},
		{"invalid input", func(b *SessionCredit) { b.InputTokensValid = false }},
		{"missing output", func(b *SessionCredit) { b.OutputTokensPresent = false }},
		{"invalid output", func(b *SessionCredit) { b.OutputTokensValid = false }},
		{"wrong model", func(b *SessionCredit) { b.ModelName = "other-model" }},
		{"invalid read", func(b *SessionCredit) { b.CacheReadTokens.Valid = false }},
		{"missing read", func(b *SessionCredit) { b.CacheReadTokens = SessionCacheCount{} }},
		{"read above input", func(b *SessionCredit) { b.CacheReadTokens.Value = 101 }},
		{"read negative", func(b *SessionCredit) { b.CacheReadTokens.Value = -1 }},
		{"read plus write above input", func(b *SessionCredit) { b.CacheWriteTokens.Value = 91 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bill := matchedCacheBill(100, 2, 10, 0)
			tc.change(&bill)
			tr := newTranslator("deepseek-v4.1-flash")
			tr.billingIdentity = outboundSelection{BillingModel: "deepseek-v4.1-flash"}
			tr.usage = mapUsage(map[string]any{"prompt_tokens": int64(100), "completion_tokens": int64(2), "cache_read_input_tokens": int64(0)})
			tr.withCredit("fixture", "fixture-turn", func() (SessionCredit, bool) { return bill, true })
			tr.applyCredit()
			details, _ := tr.usage["prompt_tokens_details"].(map[string]any)
			if details["cached_tokens"] != int64(0) {
				t.Fatal("unattributable billing cache overwrote the stream")
			}
		})
	}
}

func TestBillCachePreservesPositiveStreamAndUnknownUsage(t *testing.T) {
	for _, raw := range []map[string]any{
		{"prompt_tokens": int64(100), "completion_tokens": int64(2), "cache_read_input_tokens": int64(8)},
		{"prompt_tokens": int64(100)}, nil,
	} {
		tr := newTranslator("deepseek-v4.1-flash")
		tr.billingIdentity = outboundSelection{BillingModel: "deepseek-v4.1-flash"}
		tr.usage = mapUsage(raw)
		tr.withCredit("fixture", "fixture-turn", func() (SessionCredit, bool) { return matchedCacheBill(100, 2, 10, 0), true })
		tr.applyCredit()
		details, _ := tr.usage["prompt_tokens_details"].(map[string]any)
		if raw != nil && raw["cache_read_input_tokens"] != nil {
			if details["cached_tokens"] != int64(8) {
				t.Fatal("positive stream cache was overwritten")
			}
		} else if details != nil {
			t.Fatal("missing usage denominator gained a cache observation")
		}
	}
}

func TestBillCacheInFinalStreamUsage(t *testing.T) {
	frames := "event: output\ndata: {\"response\":\"fixture\"}\n\n" +
		"event: token_usage\ndata: {\"prompt_tokens\":61047,\"completion_tokens\":2,\"cache_read_input_tokens\":0}\n\n" +
		"event: done\ndata: {}\n\n"
	calls := 0
	reader := translateStreamWithCredit(io.NopCloser(strings.NewReader(frames)), "deepseek-v4.1-flash", nil,
		&creditSpec{selection: outboundSelection{BillingModel: "deepseek-v4.1-flash"}, scope: "fixture", turnID: "fixture-turn", fetcher: func() (SessionCredit, bool) {
			calls++
			return matchedCacheBill(61047, 2, 60672, 0), true
		}})
	defer reader.Close()
	out, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), `"cached_tokens":60672`) || !strings.Contains(string(out), "data: [DONE]") || calls != 1 {
		t.Fatal("final usage did not carry matched bill cache exactly once")
	}
}

func TestBillCacheRejectsChangingUsageSnapshots(t *testing.T) {
	frames := "event: output\ndata: {\"response\":\"fixture\"}\n\n" +
		"event: token_usage\ndata: {\"prompt_tokens\":30000,\"completion_tokens\":1}\n\n" +
		"event: token_usage\ndata: {\"prompt_tokens\":61047,\"completion_tokens\":2,\"cache_read_input_tokens\":0}\n\n" +
		"event: done\ndata: {}\n\n"
	reader := translateStreamWithCredit(io.NopCloser(strings.NewReader(frames)), "deepseek-v4.1-flash", nil,
		&creditSpec{selection: outboundSelection{BillingModel: "deepseek-v4.1-flash"}, scope: "fixture", turnID: "fixture-turn", fetcher: func() (SessionCredit, bool) {
			return matchedCacheBill(61047, 2, 60672, 0), true
		}})
	defer reader.Close()
	out, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), `"cached_tokens":60672`) {
		t.Fatal("ambiguous multi-snapshot stream gained bill cache")
	}
}

func TestBillCachePreservesEarlierPositiveSnapshot(t *testing.T) {
	for _, nested := range []bool{false, true} {
		frames := "event: output\ndata: {\"response\":\"fixture\"}\n\n"
		for _, cached := range []int{8, 0} {
			usage := fmt.Sprintf(`{"prompt_tokens":100,"completion_tokens":2,"cache_read_input_tokens":%d,"cache_creation_input_tokens":%d}`, cached, cached)
			if nested {
				usage = `{"usage":` + usage + `}`
			}
			frames += "event: token_usage\ndata: " + usage + "\n\n"
		}
		frames += "event: done\ndata: {}\n\n"
		reader := translateStreamWithCredit(io.NopCloser(strings.NewReader(frames)), "deepseek-v4.1-flash", nil,
			&creditSpec{selection: outboundSelection{BillingModel: "deepseek-v4.1-flash"}, scope: "fixture", turnID: "fixture-turn", fetcher: func() (SessionCredit, bool) {
				return matchedCacheBill(100, 2, 10, 10), true
			}})
		result, err := io.ReadAll(reader)
		reader.Close()
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(result), `"cached_tokens":8`) || !strings.Contains(string(result), `"cache_creation_input_tokens":8`) {
			t.Fatalf("nested=%t: positive SSE counters lost to a later zero snapshot", nested)
		}
	}
}

func TestBillCacheRejectsFractionalSSEDenominatorBeforeRounding(t *testing.T) {
	for _, event := range []string{"token_usage", "output"} {
		for _, nested := range []bool{false, true} {
			if event == "output" && !nested {
				continue
			}
			usage := `{"prompt_tokens":9007199254740990.5,"completion_tokens":2,"cache_read_input_tokens":0}`
			if nested {
				usage = `{"usage":` + usage + `}`
			}
			frames := "event: output\ndata: {\"response\":\"fixture\"}\n\n" +
				"event: " + event + "\ndata: " + usage + "\n\n" + "event: done\ndata: {}\n\n"
			reader := translateStreamWithCredit(io.NopCloser(strings.NewReader(frames)), "deepseek-v4.1-flash", nil,
				&creditSpec{selection: outboundSelection{BillingModel: "deepseek-v4.1-flash"}, scope: "fixture", turnID: "fixture-turn", fetcher: func() (SessionCredit, bool) {
					return matchedCacheBill(9007199254740990, 2, 100, 0), true
				}})
			result, err := io.ReadAll(reader)
			reader.Close()
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(result), `"cached_tokens":100`) || strings.Contains(string(result), `"prompt_tokens":9007199254740990`) {
				t.Fatalf("event=%s nested=%t: fractional denominator rounded into valid billing attribution", event, nested)
			}
		}
	}
}
