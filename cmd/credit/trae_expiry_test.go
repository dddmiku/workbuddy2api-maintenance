// Trae 有效期口径（2026-11-09 生产 bug）。
//
// ═══ 现象 ═══
//
// 面板上某个 Trae 账号显示「余额 2,670，其中 5,000 即将过期」——过期量比余额还大，
// 自相矛盾。
//
// ═══ 根因 ═══
//
// traeExpiryView 里 amount 用 `pack.Limit` 兜底：
//
//	amount := int64(pack.Limit)
//	if pack.Remaining > 0 { amount = int64(pack.Remaining) }
//
// 已经用完的包（Remaining == 0）于是被记成「总额度」，把**已消耗**的部分也算进
// 到期档位。实测 uid=20818427：remain=2670、size=6800，各档位合计却是 6800，
// 前端按 7 天窗口取 within_1d+within_3d+within_7d 得 5000。
//
// ═══ 契约 ═══
//
// 档位里只能放「当前还剩余」的额度，且合计不得超过 remain。用完的包不产生
// 任何到期压力；上游没下发 remaining 的包无法判断剩余，同样跳过（宁可少显示，
// 不可显示错的数）。
package main

import (
	"testing"
	"time"

	"workbuddy2api/internal/upstream/trae"
)

// 用完的包（Remaining=0、Limit 有值）绝不能贡献任何档位。
func TestTraeExpirySkipsFullyConsumedPack(t *testing.T) {
	now := time.Now().Unix()
	credits := trae.Credits{
		Packs: []trae.CreditPack{
			// 已用完：Limit=5000、Remaining=0、3 天后到期。修复前这里会加 5000。
			{Limit: 5000, Remaining: 0, EndTime: now + 3*86400},
			// 还有剩余：600，7 天后到期。
			{Limit: 1000, Remaining: 600, EndTime: now + 7*86400},
		},
	}
	v := traeExpiryView(credits)
	total := v.Within1d + v.Within3d + v.Within7d + v.Within30d + v.Later + v.Unlimited
	if total != 600 {
		t.Fatalf("档位合计 = %d want 600（只算剩余额度，用完的包不该计入）: %+v", total, v)
	}
	if v.Within3d != 0 {
		t.Errorf("Within3d = %d want 0（那个包已用完）", v.Within3d)
	}
	if v.Within7d != 600 {
		t.Errorf("Within7d = %d want 600", v.Within7d)
	}
}

// 没有 Remaining 字段的包（上游未下发）不猜，直接跳过。
func TestTraeExpirySkipsUnknownRemaining(t *testing.T) {
	now := time.Now().Unix()
	credits := trae.Credits{
		Packs: []trae.CreditPack{
			{Limit: 3000, Remaining: 0, EndTime: now + 2*86400}, // 未知剩余
			{Limit: 500, Remaining: 200, EndTime: now + 1*86400},
		},
	}
	v := traeExpiryView(credits)
	if v.Within1d != 200 || v.Within3d != 0 {
		t.Fatalf("want within_1d=200 within_3d=0, got %+v", v)
	}
}

// 档位合计必须有界：不超过所有包剩余额之和（也就是 remain 的上限）。
func TestTraeExpiryNeverExceedsRemaining(t *testing.T) {
	now := time.Now().Unix()
	credits := trae.Credits{
		Total:     6800,
		Consumed:  4130,
		Remaining: 2670,
		Packs: []trae.CreditPack{
			{Limit: 4000, Remaining: 0, EndTime: now + 2*86400},
			{Limit: 1800, Remaining: 0, EndTime: now + 5*86400},
			{Limit: 1000, Remaining: 2670, EndTime: now + 20*86400},
		},
	}
	v := traeExpiryView(credits)
	total := v.Within1d + v.Within3d + v.Within7d + v.Within30d + v.Later + v.Unlimited
	if total > int64(credits.Remaining) {
		t.Fatalf("档位合计 %d 超过账号余额 %d（修复前的核心 bug）: %+v", total, int64(credits.Remaining), v)
	}
}

// 无到期时间的剩余额度归 Unlimited（不会作废），不进任何倒计时档位。
func TestTraeExpiryNoEndTimeIsUnlimited(t *testing.T) {
	credits := trae.Credits{
		Packs: []trae.CreditPack{{Limit: 300, Remaining: 300, EndTime: 0}},
	}
	v := traeExpiryView(credits)
	if v.Unlimited != 300 {
		t.Fatalf("Unlimited = %d want 300", v.Unlimited)
	}
	if v.Within1d+v.Within3d+v.Within7d+v.Within30d != 0 {
		t.Fatalf("无到期时间不该进倒计时档位: %+v", v)
	}
}
