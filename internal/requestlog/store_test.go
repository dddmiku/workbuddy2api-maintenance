// ═══ 更新日志 ═══
// 2026-09-25：验证请求明细的真实零/缺失区分、独立尝试汇总、紧凑列表、留存与并发关闭。
package requestlog

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

func intValue(n int64) *int64       { return &n }
func floatValue(n float64) *float64 { return &n }

func fixtureRecord(id string) Record {
	start := time.Now().UTC().Add(-time.Second)
	return Record{
		RequestID: id, Protocol: ProtocolResponses, Model: "cn:deepseek-v4.1-flash",
		KeyID: "key-fixture", KeyName: "测试调用方", StartedAt: start,
		FinishedAt: start.Add(time.Second), DurationMS: 1000, QueueMS: 12,
		Status: StatusSuccess, HTTPStatus: 200, Stream: true,
	}
}

func openFixture(t *testing.T, path string, options Options) *Store {
	t.Helper()
	s, err := Open(path, options)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func appendFixture(t *testing.T, s *Store, r Record) {
	t.Helper()
	if err := s.Append(r); err != nil {
		t.Fatal(err)
	}
}

func TestSummaryPreservesMissingUsageAndActualZero(t *testing.T) {
	path := filepath.Join(t.TempDir(), "requests.jsonl")
	s := openFixture(t, path, Options{})
	r := fixtureRecord("real-request-id-01")
	r.Attempts = []Attempt{
		{Number: 1, AccountID: "first", Status: StatusError, ErrorCode: "channel_rejected", UsageReason: "upstream_not_reported"},
		{Number: 2, AccountID: "second", Status: StatusSuccess, InputTokens: intValue(113900), OutputTokens: intValue(0), CachedTokens: intValue(0)},
	}
	r.Decisions = []Decision{{Attempt: 1, ReasonCode: "weighted", StageCounts: map[string]int{"eligible": 2}}, {Attempt: 2, ReasonCode: "retry", ExcludedCounts: map[string]int{"channel_rejected": 1}}}
	appendFixture(t, s, r)
	if r.Attempts[1].UsageState != "" {
		t.Fatal("Append mutated the caller's attempt slice")
	}
	*r.Attempts[1].InputTokens = 9
	r.Decisions[0].StageCounts["eligible"] = 500
	got, err := s.Get(r.RequestID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Model != "cn:deepseek-v4.1-flash" || !got.UpstreamStarted || got.AttemptCount != 2 || got.AttemptsTruncated {
		t.Fatalf("identity/count changed: %+v", got)
	}
	if got.Attempts[0].InputTokens != nil || got.Attempts[0].Credit != nil || got.Attempts[0].UsageState != UsageMissing || got.Attempts[1].UsageState != UsageComplete {
		t.Fatalf("unknown usage changed: %+v", got.Attempts)
	}
	if *got.Attempts[1].InputTokens != 113900 || *got.Attempts[1].CachedTokens != 0 || got.Decisions[0].StageCounts["eligible"] != 2 {
		t.Fatal("stored metadata aliases the caller")
	}
	*got.Attempts[1].InputTokens = 7
	got.Decisions[0].StageCounts["eligible"] = 9
	page, err := s.List(Query{})
	if err != nil {
		t.Fatal(err)
	}
	summary := page.Items[0]
	if page.Total != 1 || summary.UsageState != UsagePartial || summary.UnknownAttempts != 1 || summary.MissingInputAttempts != 1 || summary.MissingOutputAttempts != 1 || summary.MissingCachedAttempts != 1 || summary.MissingReasoningAttempts != 2 || summary.MissingCreditAttempts != 2 {
		t.Fatalf("incomplete coverage hidden: %+v", summary)
	}
	if summary.InputTokens == nil || *summary.InputTokens != 113900 || summary.OutputTokens == nil || *summary.OutputTokens != 0 || summary.CachedTokens == nil || *summary.CachedTokens != 0 || summary.ReasoningTokens != nil || summary.Credit != nil {
		t.Fatalf("known zero or unknown changed: %+v", summary)
	}
	*page.Items[0].InputTokens = 1
	page, err = s.List(Query{})
	if err != nil || *page.Items[0].InputTokens != 113900 {
		t.Fatalf("List aliases cached values: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened := openFixture(t, path, Options{})
	got, err = reopened.Get(r.RequestID)
	if err != nil || *got.Attempts[1].InputTokens != 113900 || got.Attempts[0].InputTokens != nil {
		t.Fatalf("durable record changed: %v", err)
	}
	encoded, _ := json.Marshal(got)
	if !bytes.Contains(encoded, []byte(`"input_tokens":null`)) || !bytes.Contains(encoded, []byte(`"cached_tokens":0`)) {
		t.Fatal("JSON did not preserve null versus zero")
	}
}

func TestSummaryAddsSeparateAttemptsAndMarksTruncation(t *testing.T) {
	s := openFixture(t, filepath.Join(t.TempDir(), "requests.jsonl"), Options{})
	r := fixtureRecord("retry-three")
	r.AttemptCount = 4
	r.Attempts = []Attempt{
		{Number: 1, Status: StatusError, InputTokens: intValue(10), Credit: floatValue(0.25)},
		{Number: 2, Status: StatusSuccess, InputTokens: intValue(20), OutputTokens: intValue(3), CachedTokens: intValue(8), ReasoningTokens: intValue(2), Credit: floatValue(0)},
		{Number: 4, Status: StatusCanceled, OutputTokens: intValue(2), Credit: floatValue(0.5)},
	}
	appendFixture(t, s, r)
	page, err := s.List(Query{})
	if err != nil {
		t.Fatal(err)
	}
	got := page.Items[0]
	if *got.InputTokens != 30 || *got.OutputTokens != 5 || *got.CachedTokens != 8 || *got.ReasoningTokens != 2 || *got.Credit != 0.75 {
		t.Fatalf("attempts were not independently accumulated: %+v", got)
	}
	if !got.AttemptsTruncated || got.AttemptCount != 4 || got.StoredAttempts != 3 || got.UnknownAttempts != 3 || got.MissingInputAttempts != 2 || got.MissingOutputAttempts != 2 || got.MissingCreditAttempts != 1 {
		t.Fatalf("omitted attempt was treated as free: %+v", got)
	}
}

func TestNotStartedIsDifferentFromUnknownUpstreamUsage(t *testing.T) {
	s := openFixture(t, filepath.Join(t.TempDir(), "requests.jsonl"), Options{})
	r := fixtureRecord("preflight")
	r.Model, r.KeyID, r.KeyName = "", "", ""
	r.Status, r.HTTPStatus = StatusRejected, 400
	appendFixture(t, s, r)
	r = fixtureRecord("started-without-observation")
	r.UpstreamStarted, r.Status = true, StatusError
	appendFixture(t, s, r)
	page, err := s.List(Query{})
	if err != nil {
		t.Fatal(err)
	}
	if page.Items[0].UsageState != UsageMissing || page.Items[0].UnknownAttempts != 1 || !page.Items[0].AttemptsTruncated || page.Items[0].InputTokens != nil {
		t.Fatal("unobserved upstream attempt was hidden")
	}
	if page.Items[1].UsageState != UsageNotStarted || page.Items[1].UnknownAttempts != 0 || page.Items[1].UpstreamStarted || page.Items[1].InputTokens != nil {
		t.Fatal("preflight rejection pretends to have measured zero usage")
	}
}

func TestFinishReasonSurvivesSuccessfulHTTPAndSummary(t *testing.T) {
	s := openFixture(t, filepath.Join(t.TempDir(), "requests.jsonl"), Options{})
	r := fixtureRecord("limited-output")
	r.FinishReason = "length"
	r.Attempts = []Attempt{{Number: 1, Status: StatusSuccess, HTTPStatus: 200, FinishReason: "length", InputTokens: intValue(10), OutputTokens: intValue(100)}}
	appendFixture(t, s, r)
	page, err := s.List(Query{})
	if err != nil || page.Items[0].FinishReason != "length" || page.Items[0].Status != StatusSuccess || page.Items[0].HTTPStatus != 200 {
		t.Fatalf("finish reason hidden by HTTP status: %v %+v", err, page)
	}
	got, err := s.Get(r.RequestID)
	if err != nil || got.FinishReason != "length" || got.Attempts[0].FinishReason != "length" {
		t.Fatalf("finish reason not durable: %v", err)
	}
	for _, inAttempt := range []bool{false, true} {
		bad := fixtureRecord("bad-reason")
		if inAttempt {
			bad.Attempts = []Attempt{{Number: 1, Status: StatusError, FinishReason: "upstream body with secrets"}}
		} else {
			bad.FinishReason = "stop\nsecret"
		}
		if err := s.Append(bad); !errors.Is(err, ErrInvalidRecord) {
			t.Fatalf("unstructured finish reason accepted: %v", err)
		}
	}
}

func TestListFiltersPaginationAndCompactManagementPayload(t *testing.T) {
	s := openFixture(t, filepath.Join(t.TempDir(), "requests.jsonl"), Options{})
	for i := 0; i < MaxPageSize; i++ {
		r := fixtureRecord(fmt.Sprintf("large-%03d", i))
		r.TTFBMS = intValue(0)
		r.Model, r.KeyName = strings.Repeat("<", 256), strings.Repeat("<", 256)
		r.KeyID = strings.Repeat("k", 128)
		for j := 1; j <= MaxAttempts; j++ {
			r.Attempts = append(r.Attempts, Attempt{Number: j, Status: StatusError})
		}
		for j := 1; j <= MaxDecisions; j++ {
			r.Decisions = append(r.Decisions, Decision{Attempt: j, ReasonCode: "weighted"})
		}
		last := &r.Decisions[len(r.Decisions)-1]
		last.AccountID, last.ReasonCode, last.SelectionMethod, last.BlockedBy, last.AccountRealm = strings.Repeat("<", 256), strings.Repeat("r", 64), strings.Repeat("s", 64), strings.Repeat("b", 64), strings.Repeat("c", 64)
		appendFixture(t, s, r)
	}
	page, err := s.List(Query{Limit: math.MaxInt})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != MaxPageSize || page.Total != MaxPageSize || page.Items[0].RequestID != fmt.Sprintf("large-%03d", MaxPageSize-1) {
		t.Fatal("limit or newest-first order changed")
	}
	raw, err := json.Marshal(page)
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) >= 1<<20 || bytes.Contains(raw, []byte(`"attempts":`)) || bytes.Contains(raw, []byte(`"decisions":`)) {
		t.Fatalf("list exceeds the management contract: %d bytes", len(raw))
	}
	filtered, err := s.List(Query{KeyID: strings.Repeat("k", 128), Model: strings.Repeat("<", 256), Status: StatusSuccess, Offset: 5, Limit: 2})
	if err != nil || filtered.Total != MaxPageSize || len(filtered.Items) != 2 || filtered.Items[0].RequestID != fmt.Sprintf("large-%03d", MaxPageSize-6) {
		t.Fatalf("filters or pagination failed: %v %+v", err, filtered)
	}
	empty, err := s.List(Query{KeyID: "absent", RequestID: fmt.Sprintf("large-%03d", MaxPageSize-1)})
	if err != nil || empty.Total != 0 || len(empty.Items) != 0 {
		t.Fatal("filters were not ANDed")
	}
	detail, err := s.Get(fmt.Sprintf("large-%03d", MaxPageSize-1))
	if err != nil || len(detail.Attempts) != MaxAttempts || len(detail.Decisions) != MaxDecisions {
		t.Fatalf("detail lost its bounded arrays: %v", err)
	}
	if page.Items[0].TTFBMS == nil || *page.Items[0].TTFBMS != 0 || page.Items[0].LastDecision == nil || !page.Items[0].LastDecision.AccountIDTruncated || len(page.Items[0].LastDecision.AccountID) != 64 || len(detail.Decisions[len(detail.Decisions)-1].AccountID) != 256 {
		t.Fatal("compact decision or known zero TTFB was lost")
	}
	page.Items[0].LastDecision.ReasonCode = "changed"
	*page.Items[0].TTFBMS = 100
	page, err = s.List(Query{})
	if err != nil || page.Items[0].LastDecision.ReasonCode == "changed" || *page.Items[0].TTFBMS != 0 {
		t.Fatal("last decision/TTFB alias cached summary")
	}
}

func TestRetentionBoundsCountAgeAndBytesAcrossStores(t *testing.T) {
	t.Run("count-and-stale-cache", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "requests.jsonl")
		a, b := openFixture(t, path, Options{MaxRecords: 5}), openFixture(t, path, Options{MaxRecords: 5})
		for i := 1; i <= 8; i++ {
			appendFixture(t, a, fixtureRecord(fmt.Sprintf("a-%d", i)))
		}
		appendFixture(t, b, fixtureRecord("b-9"))
		appendFixture(t, a, fixtureRecord("a-10"))
		page, err := b.List(Query{})
		if err != nil || page.Total > 5 {
			t.Fatalf("count bound failed: %v %+v", err, page)
		}
		for _, id := range []string{"a-8", "b-9", "a-10"} {
			if _, err := a.Get(id); err != nil {
				t.Fatalf("stale store erased %s: %v", id, err)
			}
		}
		if _, err := b.Get("a-1"); !errors.Is(err, ErrNotFound) {
			t.Fatal("stale store resurrected expired record")
		}
	})
	t.Run("age-on-read", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "requests.jsonl")
		s := openFixture(t, path, Options{MaxAge: time.Second})
		now := time.Now().UTC()
		s.now = func() time.Time { return now }
		appendFixture(t, s, fixtureRecord("old"))
		before, _ := os.Stat(path)
		now = now.Add(time.Second)
		page, err := s.List(Query{})
		if err != nil || page.Total != 0 {
			t.Fatalf("read did not expire boundary record: %v", err)
		}
		after, _ := os.Stat(path)
		if after.Size() >= before.Size() {
			t.Fatal("expired data remained on disk")
		}
		appendFixture(t, s, fixtureRecord("new"))
		if _, err := s.Get("old"); !errors.Is(err, ErrNotFound) {
			t.Fatal("expired request became visible")
		}
	})
	t.Run("bytes", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "requests.jsonl")
		options := Options{MaxBytes: 4096}
		s := openFixture(t, path, options)
		for i := 0; i < 30; i++ {
			appendFixture(t, s, fixtureRecord(fmt.Sprintf("bytes-%d", i)))
			info, _ := os.Stat(path)
			if info.Size() > options.MaxBytes {
				t.Fatalf("journal grew past limit: %d", info.Size())
			}
		}
		if _, err := s.Get("bytes-29"); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Get("bytes-0"); !errors.Is(err, ErrNotFound) {
			t.Fatal("byte retention failed")
		}
	})
}

func TestConcurrentStoresAndCallerGoroutines(t *testing.T) {
	path := filepath.Join(t.TempDir(), "requests.jsonl")
	stores := []*Store{openFixture(t, path, Options{}), openFixture(t, path, Options{})}
	var wg sync.WaitGroup
	errorsSeen := make(chan error, 4)
	for worker := 0; worker < 4; worker++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			for round := 0; round < 25; round++ {
				if err := stores[worker%2].Append(fixtureRecord(fmt.Sprintf("w%d-%d", worker, round))); err != nil {
					errorsSeen <- err
					return
				}
			}
		}(worker)
	}
	wg.Wait()
	close(errorsSeen)
	for err := range errorsSeen {
		t.Error(err)
	}
	for _, s := range stores {
		page, err := s.List(Query{Limit: 200})
		if err != nil || page.Total != 100 {
			t.Fatalf("concurrent records lost: %v total=%d", err, page.Total)
		}
	}
}

func TestInputAndQueryBoundsDoNotModifyJournal(t *testing.T) {
	path := filepath.Join(t.TempDir(), "requests.jsonl")
	s := openFixture(t, path, Options{})
	appendFixture(t, s, fixtureRecord("valid"))
	original, _ := os.ReadFile(path)
	cases := map[string]func(*Record){
		"request-id":     func(r *Record) { r.RequestID = "../escape" },
		"protocol":       func(r *Record) { r.Protocol = "unsupported" },
		"name":           func(r *Record) { r.KeyName = strings.Repeat("x", 257) },
		"body-in-code":   func(r *Record) { r.ErrorCode = "upstream returned secret text" },
		"control":        func(r *Record) { r.Model = "model\nAuthorization: bearer" },
		"invalid-utf8":   func(r *Record) { r.KeyName = string([]byte{0xff}) },
		"timestamp":      func(r *Record) { r.FinishedAt = r.StartedAt.Add(-time.Second) },
		"duration":       func(r *Record) { r.QueueMS = r.DurationMS + 1 },
		"attempt-count":  func(r *Record) { r.AttemptCount = MaxAttemptCount + 1 },
		"attempts":       func(r *Record) { r.Attempts = make([]Attempt, MaxAttempts+1) },
		"decisions":      func(r *Record) { r.Decisions = make([]Decision, MaxDecisions+1) },
		"negative-usage": func(r *Record) { r.Attempts = []Attempt{{Number: 1, Status: StatusError, InputTokens: intValue(-1)}} },
		"overflow-usage": func(r *Record) {
			r.Attempts = []Attempt{{Number: 1, Status: StatusError, InputTokens: intValue(math.MaxInt64)}}
		},
		"non-finite": func(r *Record) {
			r.Attempts = []Attempt{{Number: 1, Status: StatusError, Credit: floatValue(math.NaN())}}
		},
		"false-complete": func(r *Record) { r.Attempts = []Attempt{{Number: 1, Status: StatusError, UsageState: UsageComplete}} },
		"false-missing": func(r *Record) {
			r.Attempts = []Attempt{{Number: 1, Status: StatusError, UsageState: UsageMissing, InputTokens: intValue(0)}}
		},
		"duplicate-attempt": func(r *Record) {
			r.Attempts = []Attempt{{Number: 1, Status: StatusError}, {Number: 1, Status: StatusError}}
		},
		"negative-count": func(r *Record) {
			r.Decisions = []Decision{{ReasonCode: "weighted", StageCounts: map[string]int{"eligible": -1}}}
		},
		"too-many-counts": func(r *Record) {
			counts := map[string]int{}
			for i := 0; i < 33; i++ {
				counts[fmt.Sprintf("count_%d", i)] = 1
			}
			r.Decisions = []Decision{{ReasonCode: "weighted", StageCounts: counts}}
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			r := fixtureRecord("invalid")
			mutate(&r)
			if err := s.Append(r); !errors.Is(err, ErrInvalidRecord) {
				t.Fatalf("wanted invalid record, got %v", err)
			}
		})
	}
	large := fixtureRecord("too-large")
	counts := map[string]int{}
	for i := 0; i < 32; i++ {
		counts[fmt.Sprintf("%02d%s", i, strings.Repeat("x", 62))] = 1
	}
	for i := 0; i < MaxDecisions; i++ {
		large.Decisions = append(large.Decisions, Decision{ReasonCode: "weighted", StageCounts: counts, ExcludedCounts: counts})
	}
	if err := s.Append(large); !errors.Is(err, ErrRecordTooLarge) {
		t.Fatalf("oversized frame not rejected: %v", err)
	}
	if err := s.Append(fixtureRecord("valid")); !errors.Is(err, ErrDuplicate) {
		t.Fatalf("duplicate record not rejected: %v", err)
	}
	for _, query := range []Query{{Offset: -1}, {Offset: DefaultMaxRecords + 1}, {Limit: -1}, {Status: "invalid"}, {RequestID: "a/b"}, {KeyID: "key\nsecret"}} {
		if _, err := s.List(query); !errors.Is(err, ErrInvalidQuery) {
			t.Fatalf("query not bounded: %+v %v", query, err)
		}
	}
	after, _ := os.ReadFile(path)
	if !bytes.Equal(original, after) {
		t.Fatal("rejected input modified journal")
	}
}

func TestOptionsAndCloseContract(t *testing.T) {
	for _, options := range []Options{{MaxRecords: -1}, {MaxRecords: DefaultMaxRecords + 1}, {MaxAge: -1}, {MaxAge: DefaultMaxAge + 1}, {MaxBytes: 511}, {MaxBytes: DefaultMaxBytes + 1}} {
		if _, err := Open(filepath.Join(t.TempDir(), "requests.jsonl"), options); err == nil {
			t.Fatalf("unbounded options accepted: %+v", options)
		}
	}
	s := openFixture(t, filepath.Join(t.TempDir(), "requests.jsonl"), Options{})
	if !reflect.DeepEqual(s.options, Options{MaxRecords: DefaultMaxRecords, MaxAge: DefaultMaxAge, MaxBytes: DefaultMaxBytes}) {
		t.Fatal("default retention changed")
	}
	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); _ = s.Close() }()
	}
	wg.Wait()
	if err := s.Append(fixtureRecord("closed")); !errors.Is(err, ErrClosed) {
		t.Fatal("closed store accepted data")
	}
	if _, err := s.List(Query{}); !errors.Is(err, ErrClosed) {
		t.Fatal("closed store allowed query")
	}
	if _, err := s.Get("closed"); !errors.Is(err, ErrClosed) {
		t.Fatal("closed store allowed detail")
	}
}
