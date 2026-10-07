// ═══ 更新日志 ═══
// 2026-10-07：新增。内容审核拒绝的账号维度判定回归测试：
//  1. 连续 3 次才停用，未达阈值不停；
//  2. 硬证据（同请求别的号成功）一次即停；
//  3. 成功清零计数，避免健康号的偶发命中累积成误停；
//  4. 计数跨重启保留；
//  5. 手工复活清零计数。
package pool

import (
	"path/filepath"
	"testing"

	"workbuddy2api/internal/auth"
)

// reviewFailsOf 曝露 entry.reviewFails 供测试断言（包内私有 helper）。
func (p *Pool) reviewFailsOf(uid string) (int, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	e, ok := p.byUID[uid]
	if !ok {
		return 0, false
	}
	return e.reviewFails, true
}

// TestReviewThresholdDisablesAfterStreak 连续审核拒绝达到阈值才停用，未达阈值不停。
func TestReviewThresholdDisablesAfterStreak(t *testing.T) {
	p := New(filepath.Join(t.TempDir(), "state.json"))
	p.Add(&auth.Auth{UID: "u1"})

	for i := 1; i < reviewFailThreshold; i++ {
		if p.NoteContentBlocked("u1") {
			t.Fatalf("第 %d 次拒绝就停用了，阈值应为 %d", i, reviewFailThreshold)
		}
		if st, _ := p.Status("u1"); st.Disabled {
			t.Fatalf("第 %d 次拒绝后不应 disabled", i)
		}
	}
	if !p.NoteContentBlocked("u1") {
		t.Fatalf("第 %d 次拒绝应达到阈值并停用", reviewFailThreshold)
	}
	st, _ := p.Status("u1")
	if !st.Disabled {
		t.Fatal("达到阈值后应 disabled")
	}
	if st.DisabledReason != reviewFailReason {
		t.Errorf("disabled_reason=%q want %q", st.DisabledReason, reviewFailReason)
	}
}

// TestFlagReviewAccountDisablesImmediately 硬证据（同请求别的号成功）一次即停，
// 不等连续计数。
func TestFlagReviewAccountDisablesImmediately(t *testing.T) {
	p := New(filepath.Join(t.TempDir(), "state.json"))
	p.Add(&auth.Auth{UID: "u1"})

	if !p.FlagReviewAccount("u1") {
		t.Fatal("FlagReviewAccount 应停用账号")
	}
	st, _ := p.Status("u1")
	if !st.Disabled {
		t.Fatal("硬证据后应 disabled")
	}
	if st.DisabledReason != reviewFailReason {
		t.Errorf("disabled_reason=%q want %q", st.DisabledReason, reviewFailReason)
	}
	// 已停用的账号重复 flag 不应重复计数或改变状态。
	if p.FlagReviewAccount("u1") {
		t.Error("已停用账号再次 flag 应返回 false")
	}
}

// TestReviewFailsClearedBySuccess 成功清零连续计数：健康号的偶发命中不会累积成误停。
func TestReviewFailsClearedBySuccess(t *testing.T) {
	p := New(filepath.Join(t.TempDir(), "state.json"))
	p.Add(&auth.Auth{UID: "u1"})

	p.NoteContentBlocked("u1")
	p.NoteContentBlocked("u1")
	if fails, _ := p.reviewFailsOf("u1"); fails != reviewFailThreshold-1 {
		t.Fatalf("reviewFails=%d want %d", fails, reviewFailThreshold-1)
	}

	p.NoteSuccess("u1")
	if fails, _ := p.reviewFailsOf("u1"); fails != 0 {
		t.Fatalf("成功入账后 reviewFails=%d want 0", fails)
	}

	// 清零后再连续命中：必须重新攒满阈值才停用。
	p.NoteContentBlocked("u1")
	if st, _ := p.Status("u1"); st.Disabled {
		t.Fatal("清零后单次拒绝不应停用")
	}
}

// TestReviewFailsPersistRoundTrip 连续计数跨重启保留：被标记的号重启后继续累计，
// 不用重新吃满阈值。
func TestReviewFailsPersistRoundTrip(t *testing.T) {
	fp := filepath.Join(t.TempDir(), "state.json")
	p := New(fp)
	p.Add(&auth.Auth{UID: "u1"})
	p.NoteContentBlocked("u1")
	p.NoteContentBlocked("u1")
	p.Flush()

	p2 := New(fp)
	p2.Add(&auth.Auth{UID: "u1"})
	fails, ok := p2.reviewFailsOf("u1")
	if !ok {
		t.Fatal("重启后账号缺失")
	}
	if fails != reviewFailThreshold-1 {
		t.Fatalf("重启后 reviewFails=%d want %d", fails, reviewFailThreshold-1)
	}
	// 恢复后只差一次：这次必须停用。
	if !p2.NoteContentBlocked("u1") {
		t.Fatal("恢复后达到阈值应停用")
	}
}

// TestReviveResetsReviewFails 手工复活同时清掉审核计数，避免复活后立刻又被停。
func TestReviveResetsReviewFails(t *testing.T) {
	p := New(filepath.Join(t.TempDir(), "state.json"))
	p.Add(&auth.Auth{UID: "u1"})
	p.FlagReviewAccount("u1")

	p.ReviveDisabled("u1")
	st, _ := p.Status("u1")
	if st.Disabled {
		t.Fatal("复活后不应 disabled")
	}
	if fails, _ := p.reviewFailsOf("u1"); fails != 0 {
		t.Fatalf("复活后 reviewFails=%d want 0", fails)
	}
}
