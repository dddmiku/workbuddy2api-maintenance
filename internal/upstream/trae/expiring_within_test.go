// Trae 快过期额度的窗口计算（2026-11-09）。
//
// 用户要求「按 cn 一样，优先使用过期积分」：Trae 账号此前只写余额总量、
// 从不写 creditsExpiring，选号侧整套「快过期优先」机制对该域完全失效。
//
// 本文件锁住数据的正确性——口径必须与面板 traeExpiryView、以及 WorkBuddy 侧的
// CreditBuckets.Expiring 三者一致：只算当前剩余、且到期时间落在窗口内。
package trae

import (
	"testing"
	"time"
)

func TestExpiringWithinBasic(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	credits := Credits{
		Total: 6800, Consumed: 4129, Remaining: 2671,
		Packs: []CreditPack{
			{Limit: 4000, Remaining: 0, EndTime: now.Add(24 * time.Hour).Unix()},   // 用完
			{Limit: 1000, Remaining: 400, EndTime: now.Add(48 * time.Hour).Unix()}, // 窗口内
			{Limit: 2000, Remaining: 300, EndTime: now.Add(5 * 24 * time.Hour).Unix()},
			{Limit: 1000, Remaining: 900, EndTime: now.Add(60 * 24 * time.Hour).Unix()}, // 窗口外
		},
	}
	got := ExpiringWithin(credits, 7*24*time.Hour, now)
	if got != 700 {
		t.Fatalf("ExpiringWithin = %d want 700（400+300；用完的包与窗口外的包都不算）", got)
	}
}

// window<=0 时不判定（与 WorkBuddy 侧 soon<=0 不分桶同口径）。
func TestExpiringWithinZeroWindow(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	credits := Credits{Packs: []CreditPack{
		{Limit: 100, Remaining: 100, EndTime: now.Add(time.Hour).Unix()},
	}}
	if got := ExpiringWithin(credits, 0, now); got != 0 {
		t.Fatalf("window=0 应返回 0，got %d", got)
	}
}

// 无到期时间的包永不「快过期」（它们是 Unlimited）。
func TestExpiringWithinNoEndTime(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	credits := Credits{Packs: []CreditPack{
		{Limit: 500, Remaining: 500, EndTime: 0},
	}}
	if got := ExpiringWithin(credits, 30*24*time.Hour, now); got != 0 {
		t.Fatalf("无到期时间不该算快过期，got %d", got)
	}
}

// 边界：到期时刻正好等于 deadline 时算在窗口内（与 WorkBuddy 的
// `!end.After(now.Add(soon))` 逐字一致）。
func TestExpiringWithinBoundaryInclusive(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	window := 7 * 24 * time.Hour
	credits := Credits{Packs: []CreditPack{
		{Limit: 250, Remaining: 250, EndTime: now.Add(window).Unix()},
	}}
	if got := ExpiringWithin(credits, window, now); got != 250 {
		t.Fatalf("临界时刻应在窗口内，got %d", got)
	}
}

// 结果永远不超过 remain（它是 remain 的子集，这是选号侧信任它的前提）。
func TestExpiringWithinNeverExceedsRemaining(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	credits := Credits{
		Total: 6800, Consumed: 4129.12, Remaining: 2670.88,
		Packs: []CreditPack{
			{Limit: 4530, Remaining: 2000, EndTime: now.Add(time.Hour).Unix()},
			{Limit: 2270, Remaining: 670, EndTime: now.Add(2 * 24 * time.Hour).Unix()},
		},
	}
	got := ExpiringWithin(credits, 7*24*time.Hour, now)
	if got > int64(credits.Remaining) {
		t.Fatalf("快过期 %d 超过余额 %d —— 会让选号为了不存在的紧迫度牺牲分层", got, int64(credits.Remaining))
	}
	if got != 2670 {
		t.Fatalf("ExpiringWithin = %d want 2670（2000+670）", got)
	}
}
