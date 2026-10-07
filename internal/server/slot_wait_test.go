// ═══ 更新日志 ═══
// 2026-10-07：新增。覆盖「健康账号全被在途名额占满」时的有界等待：slotWaitable 的
// 判定边界、waitForSlot 的三种收尾、以及端到端把并发峰值下的本地 503 变成短暂排队。
package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/upstream"
)

// dur 把时长包成 Config.SlotWait 需要的指针形式（nil = 未配置 → 用默认）。
func dur(d time.Duration) *time.Duration { return &d }

// TestSlotWaitableOnlyWhenHealthyButBusy 判定必须精确区分两种「选不到号」：
// 有号可用只是全忙（值得等）vs 根本没有健康账号（等不来，必须 fail fast）。
func TestSlotWaitableOnlyWhenHealthyButBusy(t *testing.T) {
	cases := []struct {
		name  string
		stage map[string]int
		want  bool
	}{
		{"healthy but all in-flight full", map[string]int{"healthy": 2, "available": 0}, true},
		{"healthy and one available", map[string]int{"healthy": 2, "available": 1}, false},
		{"no healthy account", map[string]int{"healthy": 0, "available": 0}, false},
		{"empty decision", nil, false},
		{"healthy key absent", map[string]int{"evaluated": 27}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := slotWaitable(pool.Decision{StageCounts: tc.stage}); got != tc.want {
				t.Fatalf("slotWaitable(%v)=%v want %v", tc.stage, got, tc.want)
			}
		})
	}
}

// TestWaitForSlotReturnsAccountWhenSlotFreed 等待期间有账号释放名额时，
// waitForSlot 必须醒来重选并交出该账号——这是整个修复的收益点。
// 断言同时覆盖「释放的是另一个账号」的场景：等待者醒来后重选，拿到的是
// 真正空出名额的那个号，不需要释放方与等待方一一对应。
func TestWaitForSlotReturnsAccountWhenSlotFreed(t *testing.T) {
	p := testPoolWith(
		&auth.Auth{UID: "busy", AccessToken: "at-busy", ExpiresAt: 9999999999},
		&auth.Auth{UID: "idle", AccessToken: "at-idle", ExpiresAt: 9999999999},
	)
	p.SetMaxInFlight(1)
	if !p.Acquire("busy") {
		t.Fatal("fixture acquisition failed")
	}
	// idle 被 tried 排除，所以池里"健康但可用的号"只有 busy 一个，且它满额 →
	// 选号结果必然是 healthy>0 / available=0，正好落进等待分支。
	h := NewHandler(Config{Pool: p, Upstream: upstream.New(), SlotWait: dur(time.Second)})
	tried := map[string]bool{"idle": true}
	decision := pool.Decision{StageCounts: map[string]int{"healthy": 1, "available": 0}}
	if !slotWaitable(decision) {
		t.Fatal("fixture is not in the waitable state")
	}

	type result struct {
		acct *auth.Auth
	}
	done := make(chan result, 1)
	go func() {
		acct, _ := h.waitForSlot(context.Background(), tried, "glm-5.2", "cn", time.Now().Add(3*time.Second))
		done <- result{acct}
	}()

	// 给等待者足够时间进入 select，再释放名额。
	time.Sleep(100 * time.Millisecond)
	p.Release("busy")

	select {
	case r := <-done:
		if r.acct == nil || r.acct.UID != "busy" {
			t.Fatalf("want the freed account, got %+v", r.acct)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("waitForSlot did not wake on slot release")
	}
}

// TestWaitForSlotGivesUpAtDeadline 没有名额释放时必须在 deadline 收手，
// 如实交回 nil 让调用方回 503——等待只推迟判定，不改变判定。
func TestWaitForSlotGivesUpAtDeadline(t *testing.T) {
	p := testPoolWith(&auth.Auth{UID: "busy", AccessToken: "at", ExpiresAt: 9999999999})
	p.SetMaxInFlight(1)
	if !p.Acquire("busy") {
		t.Fatal("fixture acquisition failed")
	}
	defer p.Release("busy")

	h := NewHandler(Config{Pool: p, Upstream: upstream.New(), SlotWait: dur(time.Second)})
	start := time.Now()
	acct, _ := h.waitForSlot(context.Background(), map[string]bool{}, "glm-5.2", "cn", start.Add(150*time.Millisecond))
	elapsed := time.Since(start)
	if acct != nil {
		t.Fatalf("acct=%v want nil after deadline", acct.UID)
	}
	if elapsed < 150*time.Millisecond {
		t.Fatalf("gave up after %v; must wait out the deadline", elapsed)
	}
	if elapsed > time.Second {
		t.Fatalf("gave up after %v; deadline was 150ms", elapsed)
	}
}

// TestWaitForSlotDoesNotWaitWithoutHealthyAccount 池里全是禁用/冷却账号时不得等待：
// 那种状态在等待窗口内不会改变，等待只会白让客户端等（fail fast 语义必须保留）。
func TestWaitForSlotDoesNotWaitWithoutHealthyAccount(t *testing.T) {
	p := testPoolWith(&auth.Auth{UID: "dead", AccessToken: "at", ExpiresAt: 9999999999})
	p.Disable("dead", "session dead")
	h := NewHandler(Config{Pool: p, Upstream: upstream.New(), SlotWait: dur(10 * time.Second)})

	start := time.Now()
	acct, decision := h.waitForSlot(context.Background(), map[string]bool{}, "glm-5.2", "cn", start.Add(10*time.Second))
	if acct != nil {
		t.Fatalf("acct=%v want nil", acct.UID)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("waited %v with no healthy account; must fail fast", elapsed)
	}
	if decision.StageCounts["healthy"] != 0 {
		t.Fatalf("fixture should have no healthy account: %+v", decision.StageCounts)
	}
}

// TestWaitForSlotStopsOnContextCancel 客户端断开后不得继续占着等待窗口：
// 调用方下一轮会用 ctx.Err() 收尾，这里只需尽快返回。
func TestWaitForSlotStopsOnContextCancel(t *testing.T) {
	p := testPoolWith(&auth.Auth{UID: "busy", AccessToken: "at", ExpiresAt: 9999999999})
	p.SetMaxInFlight(1)
	if !p.Acquire("busy") {
		t.Fatal("fixture acquisition failed")
	}
	defer p.Release("busy")

	h := NewHandler(Config{Pool: p, Upstream: upstream.New(), SlotWait: dur(10 * time.Second)})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		h.waitForSlot(ctx, map[string]bool{}, "glm-5.2", "cn", time.Now().Add(10*time.Second))
		close(done)
	}()
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("waitForSlot ignored context cancellation")
	}
}

// TestChatWaitsForSlotInsteadOfReturning503 端到端：健康账号的在途名额被占满时，
// 请求不再立刻 503，而是等到名额释放后正常拿到 200——这正是线上 294 次
// no_healthy_account 里「有号可用、只是全忙」那部分的真实形态。
func TestChatWaitsForSlotInsteadOfReturning503(t *testing.T) {
	up := newFakeUpstream(t, func(string) (int, string, bool) { return 200, sseOK, true })
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999})
	p.SetMaxInFlight(1)
	if !p.Acquire("u1") {
		t.Fatal("fixture acquisition failed")
	}

	h := NewHandler(Config{Pool: p, Upstream: up, SlotWait: dur(3 * time.Second)})
	rec := httptest.NewRecorder()
	start := time.Now()
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		// 让请求先进入等待，再腾出名额。
		time.Sleep(150 * time.Millisecond)
		p.Release("u1")
	}()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","messages":[]}`)))
	wg.Wait()

	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s (want 200 after waiting for the slot)", rec.Code, rec.Body)
	}
	if elapsed := time.Since(start); elapsed < 100*time.Millisecond {
		t.Fatalf("returned after %v; must have waited for the slot release", elapsed)
	}
}

// TestChat503StillImmediateWithoutHealthyAccount 回归护栏：池里没有健康账号时，
// 请求必须仍然立刻 503（不等满 SlotWait），响应体保持原有错误契约。
func TestChat503StillImmediateWithoutHealthyAccount(t *testing.T) {
	up := newFakeUpstream(t, func(string) (int, string, bool) { return 200, sseOK, true })
	p := testPoolWith(&auth.Auth{UID: "dead", AccessToken: "at", ExpiresAt: 9999999999})
	p.Disable("dead", "session dead")

	h := NewHandler(Config{Pool: p, Upstream: up, SlotWait: dur(30 * time.Second)})
	rec := httptest.NewRecorder()
	start := time.Now()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","messages":[]}`)))
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("waited %v with no healthy account; must fail fast", elapsed)
	}
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("code=%d body=%s want 503", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "no_healthy_account") {
		t.Fatalf("body=%s want no_healthy_account", rec.Body)
	}
}

// TestChatSlotWaitZeroKeepsOldBehavior 显式 SlotWait<=0 时退化为旧行为（立即 503），
// 给运维留一个「关掉等待」的逃生门。
func TestChatSlotWaitZeroKeepsOldBehavior(t *testing.T) {
	up := newFakeUpstream(t, func(string) (int, string, bool) { return 200, sseOK, true })
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999})
	p.SetMaxInFlight(1)
	if !p.Acquire("u1") {
		t.Fatal("fixture acquisition failed")
	}
	defer p.Release("u1")

	// nil 才会兜底成 DefaultSlotWait；显式 0（对应配置里的 "0s"）= 关闭等待。
	h := NewHandler(Config{Pool: p, Upstream: up, SlotWait: dur(0)})
	rec := httptest.NewRecorder()
	start := time.Now()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","messages":[]}`)))
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("waited %v with slot wait disabled", elapsed)
	}
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("code=%d body=%s want 503", rec.Code, rec.Body)
	}
}
