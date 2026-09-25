// ═══ 更新日志 ═══
// 2026-09-25：覆盖实际成本分层、探索、LRU、冷却兜底、粘性和有界安全快照，防止解释偏离选号。
package pool

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
	"time"
	"unicode"
	"unicode/utf8"

	"workbuddy2api/internal/auth"
)

func TestDecisionExplainsFreePreferenceAndUnknownExploration(t *testing.T) {
	withNoPickGap(t)
	p := explorationPool(t)
	p.Add(&auth.Auth{UID: "paid"})
	p.NoteModelCost("paid", "m", 2, 1000)
	weightCalls, draws := 0, 0
	p.weightOfHook = func() { weightCalls++ }
	p.SetRandomSource(func(int64) int64 { draws++; return 0 })
	for i := 0; i < 8; i++ {
		a, d := p.PickExcludingForRealmWithDecision(nil, "m", "cn")
		if d.StageCounts["available"] != 5 || d.StageCounts["cost_free"] != 1 || d.StageCounts["cost_unknown"] != 3 || d.StageCounts["cost_paid"] != 1 {
			t.Fatalf("wrong cost partition: %+v", d)
		}
		if i%4 == 3 {
			if a == nil || !strings.HasPrefix(a.UID, "unknown-") || d.ReasonCode != "unknown_exploration" ||
				d.SelectionMethod != "exploration" || d.CostState != "unknown" || d.CostPer1K != nil || d.Weight != nil || d.WeightTotal != nil {
				t.Fatalf("exploration was mislabeled or given an invented weight: %+v", d)
			}
		} else if a == nil || a.UID != "free" || d.ReasonCode != "free_preferred" || d.CostPer1K == nil ||
			*d.CostPer1K != 0 || d.CostObservedAt == nil || d.CostSamples != 1 || !d.CostUsedForSelection {
			t.Fatalf("real free observation was lost: %+v", d)
		}
	}
	if weightCalls != 6 || draws != 6 {
		t.Fatalf("explanations caused extra scoring or draws: weights=%d draws=%d", weightCalls, draws)
	}
}

func TestDecisionDistinguishesExpiredUnknownAndPaidCost(t *testing.T) {
	withNoPickGap(t)
	p := New("")
	p.Add(&auth.Auth{UID: "expired"})
	p.Add(&auth.Auth{UID: "paid"})
	p.NoteModelCost("expired", "m", 0, 1000)
	p.NoteModelCost("paid", "m", 2, 1000)
	p.mu.Lock()
	old := p.byUID["expired"].modelCost["m"]
	old.LastSeen = time.Now().Add(-modelCostTTL - time.Hour)
	p.byUID["expired"].modelCost["m"] = old
	p.mu.Unlock()
	a, d := p.PickExcludingForRealmWithDecision(nil, "m", "cn")
	if a.UID != "expired" || d.ReasonCode != "unknown_preferred" || d.CostState != "unknown" || d.CostUnknownReason != "expired" || d.CostPer1K != nil {
		t.Fatalf("stale free price became measured zero: %+v", d)
	}
	_, d = p.PickExcludingForRealmWithDecision(map[string]bool{"expired": true}, "m", "cn")
	if d.CostState != "paid" || d.CostPer1K == nil || *d.CostPer1K != 2 || d.ExcludedCounts["already_tried"] != 1 {
		t.Fatalf("measured price or retry exclusion missing: %+v", d)
	}
}

func TestDecisionLRUUsesFullPreferredSetWithoutLotteryFacts(t *testing.T) {
	oldGap := minPickGap
	minPickGap = time.Hour
	t.Cleanup(func() { minPickGap = oldGap })
	p := New("")
	p.SetRandomSource(func(int64) int64 { t.Fatal("LRU must not consume a random draw"); return 0 })
	for i, uid := range []string{"a", "b", "c", "d", "e", "f", "g", "z"} {
		p.Add(&auth.Auth{UID: uid})
		p.SetCredits(uid, int64(100-i*10))
		p.byUID[uid].lastUsed = time.Now()
		p.byUID[uid].usedSeq = uint64(i + 10)
	}
	p.byUID["z"].usedSeq = 1
	a, d := p.PickExcludingForRealmWithDecision(nil, "m", "cn")
	if a.UID != "z" || d.ReasonCode != "recent_lru" || d.SelectionMethod != "lru" ||
		d.StageCounts["available"] != 8 || d.StageCounts["preferred"] != 8 || d.StageCounts["shortlist"] != 5 ||
		d.StageCounts["eligible"] != 0 || d.StageCounts["recent"] != 5 || d.Weight == nil || d.WeightUnits != nil || d.WeightTotal != nil {
		t.Fatalf("full-set LRU was presented as a weighted draw: %+v", d)
	}
}

func TestDecisionRecordsSeparateNormalAndFallbackFilters(t *testing.T) {
	auth.SetGlobalEnabled(true)
	t.Cleanup(func() { auth.SetGlobalEnabled(true) })
	p := New("")
	for _, uid := range []string{"tried", "realm", "disabled", "hard", "soft", "breaker", "model", "busy"} {
		p.Add(&auth.Auth{UID: uid})
	}
	p.byUID["realm"].a.Domain = "www.workbuddy.ai"
	p.byUID["tried"].disabled = true
	p.byUID["disabled"].disabled = true
	p.Cooldown("hard", CoolHard, time.Hour, "do-not-copy-secret-reason")
	p.Cooldown("soft", CoolSoft, time.Hour, "do-not-copy-secret-reason")
	p.byUID["breaker"].breakerUntil = time.Now().Add(10 * time.Minute)
	p.CooldownSoftForModel("model", time.Hour, time.Now().Add(time.Hour), "m", "do-not-copy-model-secret")
	p.SetMaxInFlight(1)
	if !p.Acquire("busy") {
		t.Fatal("fixture acquisition failed")
	}
	defer p.Release("busy")
	tried := map[string]bool{"tried": true}
	a, d := p.PickExcludingForRealmWithDecision(tried, "m", "cn")
	if a == nil || a.UID != "breaker" || d.ReasonCode != "cooldown_fallback" || d.FallbackKind != "breaker" ||
		d.FallbackUntil == nil || !d.FallbackUntil.Equal(p.byUID["breaker"].breakerUntil) ||
		d.StageCounts["available"] != 0 || d.StageCounts["healthy"] != 1 || d.StageCounts["fallback_eligible"] != 2 ||
		d.CostUsedForSelection || d.Weight != nil || d.WeightTotal != nil {
		t.Fatalf("wrong cooldown fallback facts: %+v", d)
	}
	for _, key := range []string{"already_tried", "realm_mismatch", "disabled", "account_limit", "account_cooldown", "circuit_breaker", "model_cooldown", "in_flight_full"} {
		if d.ExcludedCounts[key] != 1 {
			t.Fatalf("missing mutually exclusive normal exclusion %s: %+v", key, d)
		}
	}
	for _, uid := range []string{"soft", "breaker"} {
		p.CooldownSoftForModel(uid, time.Hour, time.Now().Add(time.Hour), "m", "model still unavailable")
	}
	a, d = p.PickExcludingForRealmWithDecision(tried, "m", "cn")
	if a != nil || d.ReasonCode != "no_available_account" || d.AccountID != "" || d.StageCounts["fallback_eligible"] != 0 ||
		d.ExcludedCounts["fallback_model_cooldown"] != 3 || d.StageCounts["fallback_evaluated"] != 8 {
		t.Fatalf("fallback blockers were inferred from the wrong scan: %+v", d)
	}
	raw, err := json.Marshal(d)
	if err != nil || strings.Contains(string(raw), "secret") || len(d.StageCounts) > 16 || len(d.ExcludedCounts) > 20 {
		t.Fatalf("decision leaked state text or grew unbounded: %s err=%v", raw, err)
	}
}

func TestDecisionStickyChecksRealmBeforeSelectionAndExplainsFailure(t *testing.T) {
	p := realmPool(t)
	before := p.pickSeq
	a, d := p.PickByUIDForModelRealmWithDecision("g1", "m", "cn")
	if a != nil || d.ReasonCode != "sticky_unavailable" || d.BlockedBy != "realm_mismatch" ||
		d.BoundAccountID != "g1" || d.AccountID != "" || p.pickSeq != before || !p.byUID["g1"].lastUsed.IsZero() {
		t.Fatalf("wrong-realm binding was selected before rejection: %+v", d)
	}
	p.NoteModelCost("cn1", "m", 2, 1000)
	a, d = p.PickByUIDForModelRealmWithDecision("cn1", "m", "cn")
	if a == nil || a.UID != "cn1" || d.ReasonCode != "sticky_hit" || d.SelectionMethod != "sticky" ||
		d.CostState != "paid" || d.CostUsedForSelection || d.Weight != nil || d.WeightTotal != nil || p.pickSeq != before+1 {
		t.Fatalf("sticky lookup invented a weighted/cost preference: %+v", d)
	}
	p.CooldownSoftForModel("cn1", time.Hour, time.Now().Add(time.Hour), "m", "fixture")
	a, d = p.PickByUIDForModelWithDecision("cn1", "m")
	if a != nil || d.BlockedBy != "model_cooldown" || d.ExcludedCounts["model_cooldown"] != 1 {
		t.Fatalf("model-specific sticky blocker missing: %+v", d)
	}
	_, d = p.PickByUIDForModelWithDecision("missing", "m")
	if d.BlockedBy != "missing_account" || d.StageCounts["evaluated"] != 1 {
		t.Fatalf("missing binding is not distinguishable: %+v", d)
	}
}

func TestDecisionUsesOriginalComputedFactsAndDetachesFromPoolState(t *testing.T) {
	withNoPickGap(t)
	p := New("")
	p.Add(&auth.Auth{UID: "safe-account", AccessToken: "access-secret", RefreshToken: "refresh-secret", DeviceToken: "device-secret"})
	p.SetCredits("safe-account", 100)
	p.NoteModelCost("safe-account", "m", 0, 1000)
	p.SetRandomSource(func(int64) int64 {
		if p.mu.TryLock() {
			p.mu.Unlock()
			t.Fatal("selection no longer holds the actual pool lock")
		}
		// This test-only callback runs after costs and weights were computed.
		// A post-hoc reconstruction would incorrectly report the new price.
		p.byUID["safe-account"].modelCost["m"] = modelCostEntry{CostPer1k: 999, LastSeen: time.Now(), Samples: 9}
		p.byUID["safe-account"].credits = 0
		return 0
	})
	_, d := p.PickExcludingForRealmWithDecision(nil, "m", "cn")
	if d.CostState != "free" || d.CostPer1K == nil || *d.CostPer1K != 0 || d.CostSamples != 1 || d.Weight == nil || *d.Weight < 10 {
		t.Fatalf("decision was reconstructed after the actual choice: %+v", d)
	}
	p.Disable("safe-account", "state-secret")
	raw, err := json.Marshal(d)
	if err != nil || strings.Contains(string(raw), "secret") || d.ReasonCode != "free_preferred" || d.AccountID != "safe-account" {
		t.Fatalf("snapshot retained live or secret state: %s err=%v", raw, err)
	}
}

func TestDecisionIdentifiersAndInvalidObservationsStaySerializable(t *testing.T) {
	withNoPickGap(t)
	for _, uid := range []string{strings.Repeat("界", 100), "control\nuid\x00", "invalid" + string([]byte{0xff})} {
		p := New("")
		p.Add(&auth.Auth{UID: uid})
		p.NoteModelCost(uid, "m", math.NaN(), 1000)
		a, d := p.PickExcludingForRealmWithDecision(nil, "m", "cn")
		if a == nil || a.UID != uid {
			t.Fatal("observability changed the real selected identity")
		}
		if len(d.AccountID) > 256 || !utf8.ValidString(d.AccountID) {
			t.Fatalf("identifier is not bounded UTF-8: %q", d.AccountID)
		}
		for _, r := range d.AccountID {
			if !unicode.IsPrint(r) {
				t.Fatalf("identifier leaked control characters: %q", d.AccountID)
			}
		}
		raw, err := json.Marshal(d)
		if err != nil || len(raw) > 4096 || d.CostState != "unknown" || d.CostUnknownReason != "invalid_observation" || d.CostPer1K != nil {
			t.Fatalf("unsafe or invented observation: %s err=%v", raw, err)
		}
	}
}
