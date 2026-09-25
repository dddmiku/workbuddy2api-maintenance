// ═══ 更新日志 ═══
// 2026-09-25：固定抽签输入核对新旧选号序列，防止调度解释增加随机调用或改变实际候选。
package pool

import (
	"reflect"
	"testing"

	"workbuddy2api/internal/auth"
)

func TestDecisionKeepsWeightedDrawsAndRecordsActualStages(t *testing.T) {
	withNoPickGap(t)
	setup := func() (*Pool, *[]int64) {
		p := New("")
		p.idleWeightMax, p.idleWeightPerHour = 0, 0
		for _, uid := range []string{"a", "b", "c"} {
			p.Add(&auth.Auth{UID: uid})
		}
		p.SetCredits("a", 90)
		p.SetCredits("b", 60)
		p.SetCredits("c", 30)
		draws := []int64{}
		p.SetRandomSource(func(n int64) int64 {
			draws = append(draws, n)
			return (int64(len(draws)) * 7_919_000) % n
		})
		return p, &draws
	}
	legacy, legacyDraws := setup()
	detailed, detailedDraws := setup()
	selected := map[string]bool{}
	for i := 0; i < 64; i++ {
		want := legacy.PickExcludingForRealm(nil, "fixture", "cn")
		got, d := detailed.PickExcludingForRealmWithDecision(nil, "fixture", "cn")
		if got == nil || want.UID != got.UID {
			t.Fatalf("selection changed at %d: legacy=%v decision=%v", i, want, got)
		}
		selected[got.UID] = true
		if d.ReasonCode != "weighted_selection" || d.SelectionMethod != "weighted" || d.AccountID != got.UID ||
			d.StageCounts["available"] != 3 || d.StageCounts["shortlist"] != 3 || d.StageCounts["eligible"] != 3 ||
			d.CostState != "unknown" || d.CostPer1K != nil || d.Weight == nil || d.WeightUnits == nil || d.WeightTotal == nil {
			t.Fatalf("actual selection facts missing: %+v", d)
		}
	}
	if !reflect.DeepEqual(*legacyDraws, *detailedDraws) {
		t.Fatalf("decision changed random calls: legacy=%v detailed=%v", *legacyDraws, *detailedDraws)
	}
	if len(selected) != 3 {
		t.Fatalf("fixed draw fixture did not exercise every weighted candidate: %v", selected)
	}
}
