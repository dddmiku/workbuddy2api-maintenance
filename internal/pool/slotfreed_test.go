// ═══ 更新日志 ═══
// 2026-10-07：新增。覆盖在途名额释放广播（SlotFreed / broadcastSlotFreed）的唤醒语义、
// 幂等分支不误唤醒、以及惰性新建通道的零分配前提。
package pool

import (
	"sync"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
)

// TestSlotFreedBroadcastsOnRelease Release 真正扣减名额后必须关闭等待通道，
// 让等待者立刻返回——这是「等到名额就重选」能生效的唯一信号源。
func TestSlotFreedBroadcastsOnRelease(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.SetMaxInFlight(1)
	if !p.Acquire("u1") {
		t.Fatal("fixture acquisition failed")
	}

	freed := p.SlotFreed()
	select {
	case <-freed:
		t.Fatal("channel closed before any release")
	default:
	}
	p.Release("u1")
	select {
	case <-freed:
	case <-time.After(time.Second):
		t.Fatal("Release did not broadcast a freed slot")
	}
}

// TestSlotFreedReleaseWithoutSlotDoesNotBroadcast 幂等分支（没有名额可扣）不得广播：
// 它没有腾出任何名额，唤醒等待者只会让它们白跑一轮选号。
func TestSlotFreedReleaseWithoutSlotDoesNotBroadcast(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.SetMaxInFlight(1)

	// 从未 Acquire 过：inFlight 已是 0，Release 走 cur<=0 分支。
	freed := p.SlotFreed()
	p.Release("u1")
	select {
	case <-freed:
		t.Fatal("Release without a held slot must not broadcast")
	case <-time.After(50 * time.Millisecond):
	}

	// 未知 uid 同样不应广播。
	p.Release("no-such-uid")
	select {
	case <-freed:
		t.Fatal("Release of an unknown uid must not broadcast")
	case <-time.After(50 * time.Millisecond):
	}
}

// TestSlotFreedChannelIsPerBroadcast 每次广播后都会惰性新建通道：
// 新一轮等待拿到的必须是「下一次释放」的通道，而不是已经关闭的旧通道——
// 否则等待者会立刻返回、退化成忙等自旋。
func TestSlotFreedChannelIsPerBroadcast(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.SetMaxInFlight(1)

	first := p.SlotFreed()
	if !p.Acquire("u1") {
		t.Fatal("fixture acquisition failed")
	}
	p.Release("u1")
	select {
	case <-first:
	case <-time.After(time.Second):
		t.Fatal("first broadcast missing")
	}

	second := p.SlotFreed()
	if second == first {
		t.Fatal("SlotFreed reused a closed channel; waiters would spin instead of blocking")
	}
	select {
	case <-second:
		t.Fatal("reused channel reports a release that has not happened")
	default:
	}
}

// TestSlotFreedWakesAllWaiters 关闭而非投递值：一次释放要唤醒全部等待者，
// 由各自的 Acquire CAS 决出谁真正拿到名额，池里因此不需要维护等待队列。
func TestSlotFreedWakesAllWaiters(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.SetMaxInFlight(1)

	const waiters = 8
	ready := make(chan struct{}, waiters)
	var wg sync.WaitGroup
	got := make(chan struct{}, waiters)
	for i := 0; i < waiters; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ch := p.SlotFreed()
			ready <- struct{}{}
			select {
			case <-ch:
				got <- struct{}{}
			case <-time.After(2 * time.Second):
			}
		}()
	}
	for i := 0; i < waiters; i++ {
		<-ready
	}
	p.Acquire("u1")
	p.Release("u1")
	wg.Wait()
	if len(got) != waiters {
		t.Fatalf("woke %d/%d waiters; a close must wake everyone", len(got), waiters)
	}
}

// TestSlotFreedNoChannelNoAllocation 无等待者时 Release 不应分配通道：
// Release 是每个请求结束都走的最热路径，惰性新建是它零开销的前提。
func TestSlotFreedNoChannelNoAllocation(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.SetMaxInFlight(1)
	if !p.Acquire("u1") {
		t.Fatal("fixture acquisition failed")
	}
	p.Release("u1")
	p.slotWaitMu.Lock()
	ch := p.slotWaitCh
	p.slotWaitMu.Unlock()
	if ch != nil {
		t.Fatal("Release with no waiters must not allocate a wait channel")
	}
}

// TestSlotFreedBroadcastsOnDisable 停用账号同样会减少健康账号数：等名额的请求等的
// 是一个再也不会出现的状态（「有号可用、只是全忙」变成了「没有健康账号」），
// 必须被唤醒立即重判、fail fast，而不是干等到 deadline。
//
// 2026-10-08 修复：此前唤醒信号只有 Release 一个来源，停用路径不广播。
// 账号数少时（健康号数 ≤ 在途上限总量）才会显现——而那恰是 waitForSlot 服务的场景。
func TestSlotFreedBroadcastsOnDisable(t *testing.T) {
	cases := []struct {
		name string
		stop func(p *Pool, uid string) bool
	}{
		{"Disable", func(p *Pool, uid string) bool { p.Disable(uid, "fixture"); return true }},
		{"NoteSessionDead", func(p *Pool, uid string) bool {
			var hit bool
			for i := 0; i < 8 && !hit; i++ {
				hit = p.NoteSessionDead(uid)
			}
			return hit
		}},
		{"NoteAccountFault", func(p *Pool, uid string) bool {
			var hit bool
			for i := 0; i < 8 && !hit; i++ {
				hit = p.NoteAccountFault(uid)
			}
			return hit
		}},
		{"NoteContentBlocked", func(p *Pool, uid string) bool {
			var hit bool
			for i := 0; i < 8 && !hit; i++ {
				hit = p.NoteContentBlocked(uid)
			}
			return hit
		}},
		{"FlagReviewAccount", func(p *Pool, uid string) bool { return p.FlagReviewAccount(uid) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := New("")
			p.Add(&auth.Auth{UID: "u1"})
			p.SetMaxInFlight(1)

			freed := p.SlotFreed()
			if !tc.stop(p, "u1") {
				t.Fatal("fixture did not disable the account")
			}
			select {
			case <-freed:
			case <-time.After(time.Second):
				t.Fatal("disabling the last healthy account must wake slot waiters")
			}
		})
	}
}
