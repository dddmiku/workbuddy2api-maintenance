// ═══ 更新日志 ═══
// 2026-09-25：为账号池提供有界安全的调度事实 DTO，区分真实权重、成本观测和未执行的阶段。
package pool

import (
	"math"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"workbuddy2api/internal/auth"
)

// Decision contains only facts about one actual selection, never credentials.
// Counts are stage-local; fallback_* exclusions form a separate scan. Account
// identifiers are printable UTF-8 display copies limited to 256 bytes.
// WeightUnits/WeightTotal describe the actual fixed-point lottery, while Weight
// can also be the ranking score of an LRU choice. CostPer1K is a fresh cached
// per-model observation (EMA), not this request's charge or a guaranteed price.
type Decision struct {
	Mode                 string         `json:"mode"`
	ReasonCode           string         `json:"reason_code"`
	SelectionMethod      string         `json:"selection_method"`
	BlockedBy            string         `json:"blocked_by,omitempty"`
	ObservedAt           time.Time      `json:"observed_at"`
	StageCounts          map[string]int `json:"stage_counts"`
	ExcludedCounts       map[string]int `json:"excluded_counts"`
	AccountID            string         `json:"account_id,omitempty"`
	AccountRealm         string         `json:"account_realm,omitempty"`
	AccountIDTruncated   bool           `json:"account_id_truncated,omitempty"`
	BoundAccountID       string         `json:"bound_account_id,omitempty"`
	Weight               *float64       `json:"weight,omitempty"`
	WeightUnits          *int64         `json:"weight_units,omitempty"`
	WeightTotal          *int64         `json:"weight_total,omitempty"`
	CostState            string         `json:"cost_state,omitempty"`
	CostPer1K            *float64       `json:"cost_per_1k,omitempty"`
	CostObservedAt       *time.Time     `json:"cost_observed_at,omitempty"`
	CostSamples          int            `json:"cost_samples,omitempty"`
	CostUnknownReason    string         `json:"cost_unknown_reason,omitempty"`
	CostUsedForSelection bool           `json:"cost_used_for_selection"`
	FallbackKind         string         `json:"fallback_kind,omitempty"`
	FallbackUntil        *time.Time     `json:"fallback_until,omitempty"`
}

// PickExcludingForRealmWithDecision selects exactly as PickExcludingForRealm,
// recording facts under the same lock without an additional draw or re-score.
// Selection does not reserve a concurrency slot; the caller must still Acquire.
func (p *Pool) PickExcludingForRealmWithDecision(tried map[string]bool, model, realm string) (*auth.Auth, Decision) {
	var decision Decision
	account := p.pickWithDecision(tried, model, realm, &decision)
	return account, decision
}

// PickByUIDForModelWithDecision is the observable form of the legacy sticky
// lookup, retaining its no-realm-filter semantics.
func (p *Pool) PickByUIDForModelWithDecision(uid, model string) (*auth.Auth, Decision) {
	return p.PickByUIDForModelRealmWithDecision(uid, model, "")
}

// PickByUIDForModelRealmWithDecision checks a sticky target's realm before
// selecting or marking it used; an empty realm retains the legacy behavior.
func (p *Pool) PickByUIDForModelRealmWithDecision(uid, model, realm string) (*auth.Auth, Decision) {
	var decision Decision
	account := p.pickByUIDForModelRealm(uid, model, realm, &decision)
	return account, decision
}

func (p *Pool) pickByUIDForModelRealm(uid, model, realm string, d *Decision) *auth.Auth {
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.byUID[uid]
	if !ok {
		if d != nil {
			d.begin("sticky", len(p.byUID), time.Now())
			d.BoundAccountID, _ = boundedDecisionID(uid)
			d.stage("evaluated", 1)
			d.unavailableSticky("missing_account")
		}
		return nil
	}
	now := time.Now()
	d.begin("sticky", len(p.byUID), now)
	if d != nil {
		d.BoundAccountID, _ = boundedDecisionID(uid)
		d.stage("evaluated", 1)
	}
	if realm != "" && e.a.Realm() != realm {
		d.unavailableSticky("realm_mismatch")
		return nil
	}
	if reason := e.unavailableReason(now, model); reason != "" {
		d.unavailableSticky(reason)
		return nil
	}
	d.stage("healthy", 1)
	if p.inFlightFull(e) {
		d.unavailableSticky("in_flight_full")
		return nil
	}
	d.stage("available", 1)
	if d != nil {
		d.ReasonCode, d.SelectionMethod = "sticky_hit", "sticky"
		mc, known := e.modelCostOf(model, now)
		d.selected(e, decisionCostOf(e, model, now, mc, known), false)
	}
	e.lastUsed = now
	p.pickSeq++
	e.usedSeq = p.pickSeq
	return e.a
}

func (d *Decision) begin(mode string, total int, now time.Time) {
	if d == nil {
		return
	}
	*d = Decision{
		Mode: mode, ObservedAt: now.UTC(),
		StageCounts:    map[string]int{"pool_total": total, "evaluated": 0, "healthy": 0, "available": 0},
		ExcludedCounts: map[string]int{},
	}
}

func (d *Decision) stage(key string, count int) {
	if d != nil {
		d.StageCounts[key] = count
	}
}

func (d *Decision) increment(key string) {
	if d != nil {
		d.StageCounts[key]++
	}
}

func (d *Decision) exclude(reason string) {
	if d != nil {
		d.ExcludedCounts[reason]++
	}
}

func (d *Decision) unavailableSticky(reason string) {
	if d != nil {
		d.ReasonCode, d.SelectionMethod, d.BlockedBy = "sticky_unavailable", "none", reason
		d.exclude(reason)
	}
}

// This is the shared first-failure predicate for health and selection.
// It preserves the existing priority: disabled, account cooldown, breaker,
// model cooldown. No raw state reason (which could contain secrets) is copied.
func (e *entry) unavailableReason(now time.Time, model string) string {
	if e.disabled {
		return "disabled"
	}
	if !e.until.IsZero() && now.Before(e.until) {
		if e.coolKind == CoolHard {
			return "account_limit"
		}
		return "account_cooldown"
	}
	if !e.breakerUntil.IsZero() && now.Before(e.breakerUntil) {
		return "circuit_breaker"
	}
	if e.modelCooled(now, model) {
		return "model_cooldown"
	}
	return ""
}

type decisionCost struct {
	state, unknownReason string
	per1k                *float64
	observedAt           *time.Time
	samples              int
}

// Normal selection passes the very observation used to build the cost tier.
// Sticky/fallback paths may inspect a cost for context, explicitly marked as
// not used for selection; they do not calculate a weight or consume a draw.
func decisionCostOf(e *entry, model string, now time.Time, mc modelCostEntry, known bool) decisionCost {
	result := decisionCost{state: "unknown"}
	if !known {
		switch {
		case model == "":
			result.unknownReason = "model_unspecified"
		case e.modelCost[model].LastSeen.IsZero():
			result.unknownReason = "unobserved"
		default:
			result.unknownReason = "expired"
		}
		return result
	}
	if math.IsNaN(mc.CostPer1k) || math.IsInf(mc.CostPer1k, 0) {
		result.unknownReason = "invalid_observation"
		return result
	}
	price := mc.CostPer1k
	if price <= 0 {
		price, result.state = 0, "free"
	} else {
		result.state = "paid"
	}
	observed := mc.LastSeen.UTC()
	result.per1k, result.observedAt, result.samples = &price, &observed, mc.Samples
	return result
}

func (d *Decision) selected(e *entry, cost decisionCost, usedCost bool) {
	if d == nil {
		return
	}
	d.AccountID, d.AccountIDTruncated = boundedDecisionID(e.a.UID)
	d.AccountRealm = e.a.Realm()
	d.CostState, d.CostPer1K, d.CostObservedAt = cost.state, cost.per1k, cost.observedAt
	d.CostSamples, d.CostUnknownReason, d.CostUsedForSelection = cost.samples, cost.unknownReason, usedCost
}

func (d *Decision) weight(value float64) {
	if d != nil && !math.IsNaN(value) && !math.IsInf(value, 0) {
		d.Weight = &value
	}
}

func boundedDecisionID(value string) (string, bool) {
	const maximum = 256
	var result strings.Builder
	result.Grow(min(len(value), maximum))
	changed := false
	for value != "" {
		r, size := utf8.DecodeRuneInString(value)
		value = value[size:]
		if (r == utf8.RuneError && size == 1) || !unicode.IsPrint(r) {
			r, changed = '?', true
		}
		if result.Len()+utf8.RuneLen(r) > maximum {
			changed = true
			break
		}
		result.WriteRune(r)
	}
	return result.String(), changed
}
