// ═══ 更新日志 ═══
// 2026-10-10：新增。锁定 Status 透出快过期积分子集（面板据此提示「其中 N 即将过期」）。
package pool

import (
	"testing"

	"workbuddy2api/internal/auth"
)

// TestStatusExposesExpiringCredits 面板要显示「其中 N 即将过期」，数据来自 Status。
// creditsExpiring 是 credits 的子集，由签到/积分任务按 expiring_soon 窗口写入。
func TestStatusExposesExpiringCredits(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.SetCreditsDetailed("u1", 1000, 250)

	st, ok := p.Status("u1")
	if !ok {
		t.Fatal("account missing")
	}
	if st.Credits != 1000 || st.CreditsExpiring != 250 {
		t.Fatalf("credits=%d expiring=%d want 1000/250", st.Credits, st.CreditsExpiring)
	}
}

// TestStatusExpiringNeverExceedsTotal 快过架子集必须钳到 [0, credits]：
// 持久化恢复或扣减竞态都可能让它在某一刻超过总量，透出超过总量的值会让面板
// 显示出「其中 1200 即将过期 / 共 1000」这种自相矛盾的数字。
func TestStatusExpiringNeverExceedsTotal(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	// 直接写 entry 模拟不一致状态（正常写入路径 SetCreditsDetailed 会自行钳制）。
	p.SetCreditsDetailed("u1", 100, 500)

	st, _ := p.Status("u1")
	if st.CreditsExpiring > st.Credits {
		t.Fatalf("expiring=%d exceeds credits=%d", st.CreditsExpiring, st.Credits)
	}
	if st.CreditsExpiring != 100 {
		t.Fatalf("expiring=%d want clamped to 100", st.CreditsExpiring)
	}
}

// TestStatusExpiringZeroWhenUnset 没有快过期积分时不透出字段（omitempty）——
// 面板据此整行不显示，而不是显示「其中 0 即将过期」。
func TestStatusExpiringZeroWhenUnset(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.SetCreditsDetailed("u1", 500, 0)

	st, _ := p.Status("u1")
	if st.CreditsExpiring != 0 {
		t.Fatalf("expiring=%d want 0", st.CreditsExpiring)
	}
}
