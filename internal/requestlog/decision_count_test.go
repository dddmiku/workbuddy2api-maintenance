// ═══ 更新日志 ═══
// 2026-09-25：验证真实调度次数/截断状态的持久化、旧格式兼容和最后一次调度摘要。
package requestlog

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"os"
	"path/filepath"
	"testing"
)

func TestDecisionTruncationKeepsActualCountAndLastSelection(t *testing.T) {
	path := filepath.Join(t.TempDir(), "requests.jsonl")
	s := openFixture(t, path, Options{})
	r := fixtureRecord("decision-overflow")
	r.DecisionCount = 65
	for i := 1; i < MaxDecisions; i++ {
		r.Decisions = append(r.Decisions, Decision{Attempt: i, ReasonCode: "weighted_selection", AccountID: fmt.Sprintf("prefix-%d", i)})
	}
	r.Decisions = append(r.Decisions, Decision{Attempt: 65, ReasonCode: "cooldown_fallback", AccountID: "actual-last-account"})
	appendFixture(t, s, r)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened := openFixture(t, path, Options{})
	got, err := reopened.Get(r.RequestID)
	if err != nil {
		t.Fatal(err)
	}
	if got.DecisionCount != 65 || !got.DecisionsTruncated || len(got.Decisions) != MaxDecisions {
		t.Fatalf("decision count/truncation changed: %+v", got)
	}
	if got.Decisions[62].AccountID != "prefix-63" || got.Decisions[63].AccountID != "actual-last-account" {
		t.Fatal("first 63/latest retention changed")
	}
	page, err := reopened.List(Query{})
	if err != nil || len(page.Items) != 1 {
		t.Fatalf("summary unavailable: %v", err)
	}
	summary := page.Items[0]
	if summary.DecisionCount != 65 || !summary.DecisionsTruncated || summary.LastDecision == nil || summary.LastDecision.Attempt != 65 || summary.LastDecision.AccountID != "actual-last-account" || summary.LastDecision.ReasonCode != "cooldown_fallback" {
		t.Fatalf("summary misidentified final selection: %+v", summary)
	}
}

func TestLegacyDecisionCountsAreDerivedWhenReadingDisk(t *testing.T) {
	path := filepath.Join(t.TempDir(), "requests.jsonl")
	s := openFixture(t, path, Options{})
	r := fixtureRecord("legacy-decisions")
	r.Decisions = []Decision{{Attempt: 1, ReasonCode: "sticky_unavailable"}, {Attempt: 1, ReasonCode: "weighted_selection", AccountID: "selected"}}
	appendFixture(t, s, r)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := bytes.SplitAfter(raw, []byte{'\n'})
	var oldFrame frame
	if err := json.Unmarshal(lines[1], &oldFrame); err != nil {
		t.Fatal(err)
	}
	var oldRecord map[string]any
	if err := json.Unmarshal(oldFrame.Record, &oldRecord); err != nil {
		t.Fatal(err)
	}
	delete(oldRecord, "decision_count")
	delete(oldRecord, "decisions_truncated")
	oldFrame.Record, err = json.Marshal(oldRecord)
	if err != nil {
		t.Fatal(err)
	}
	oldFrame.Checksum = fmt.Sprintf("%08x", crc32.ChecksumIEEE(oldFrame.Record))
	line, err := json.Marshal(oldFrame)
	if err != nil {
		t.Fatal(err)
	}
	legacy := append(bytes.Clone(lines[0]), append(line, '\n')...)
	if err := os.WriteFile(path, legacy, 0o600); err != nil {
		t.Fatal(err)
	}
	reopened := openFixture(t, path, Options{})
	got, err := reopened.Get(r.RequestID)
	if err != nil || got.DecisionCount != 2 || got.DecisionsTruncated {
		t.Fatalf("legacy detail changed: %v %+v", err, got)
	}
	page, err := reopened.List(Query{})
	if err != nil || page.Items[0].DecisionCount != 2 || page.Items[0].DecisionsTruncated || page.Items[0].LastDecision.AccountID != "selected" {
		t.Fatalf("legacy summary disagrees with detail: %v %+v", err, page)
	}
}

func TestDecisionTotalLimitsRejectImpossibleCounts(t *testing.T) {
	path := filepath.Join(t.TempDir(), "requests.jsonl")
	s := openFixture(t, path, Options{})
	for _, total := range []int{-1, 1, MaxDecisionCount + 1} {
		r := fixtureRecord(fmt.Sprintf("invalid-decisions-%d", total))
		r.DecisionCount = total
		r.Decisions = []Decision{{ReasonCode: "sticky_unavailable"}, {ReasonCode: "weighted_selection"}}
		if err := s.Append(r); !errors.Is(err, ErrInvalidRecord) {
			t.Errorf("invalid total %d accepted: %v", total, err)
		}
	}
	r := fixtureRecord("maximum-decisions")
	r.DecisionCount = MaxDecisionCount
	r.Decisions = []Decision{{ReasonCode: "weighted_selection", AccountID: "actual-last"}}
	appendFixture(t, s, r)
	page, err := s.List(Query{})
	if err != nil || page.Total != 1 || page.Items[0].DecisionCount != MaxDecisionCount || !page.Items[0].DecisionsTruncated {
		t.Fatalf("bounded total not retained: %v %+v", err, page)
	}
}
