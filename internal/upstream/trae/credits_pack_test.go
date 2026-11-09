// Trae 积分包剩余额度的解析口径（2026-11-09 实测上游真实结构）。
//
// ═══ 之前的错误假设 ═══
//
// parseCreditPack 读 pack["remaining_amount"] / pack["available_amount"]，
// 但上游**这两个字段从来不存在**，于是 CreditPack.Remaining 恒为 0。
// 后果：traeExpiryView 的 amount 退回用 pack.Limit（包总额度），把**已消耗**的
// 部分也算进「即将过期」，面板出现「余额 2,670，其中 5,000 即将过期」。
//
// ═══ 真实结构（curl 实测）═══
//
//	{
//	  "display_desc": "老用户福利",
//	  "expire_time": 1794480993,
//	  "entitlement_base_info": {
//	    "end_time": 1794480993,
//	    "product_extra": {"package_extra": {
//	      "package_name": "福利积分",
//	      "quota": {"credits_limit": 4000}
//	    }}
//	  },
//	  "usage": {"credits_amount": 4000}   ← 该包已用多少；空 {} = 未使用
//	}
//
// 剩余 = credits_limit − usage.credits_amount
package trae

import "testing"

// 上游真实形态：额度 4000，已用 4000 → 剩余 0。
func TestParseCreditPackFullyUsed(t *testing.T) {
	pack := map[string]any{
		"display_desc": "老用户福利",
		"expire_time":  float64(1794480993),
		"entitlement_base_info": map[string]any{
			"end_time": float64(1794480993),
			"product_extra": map[string]any{
				"package_extra": map[string]any{
					"package_name": "福利积分",
					"quota":        map[string]any{"credits_limit": float64(4000)},
				},
			},
		},
		"usage": map[string]any{"credits_amount": float64(4000)},
	}
	got := parseCreditPack(pack)
	if got.Limit != 4000 {
		t.Fatalf("Limit = %v want 4000", got.Limit)
	}
	if got.Remaining != 0 {
		t.Fatalf("Remaining = %v want 0（全部用完）", got.Remaining)
	}
	if got.EndTime != 1794480993 {
		t.Fatalf("EndTime = %v want 1794480993", got.EndTime)
	}
}

// usage 为空对象 = 未使用，剩余应等于额度（不是 0）。
func TestParseCreditPackUnused(t *testing.T) {
	pack := map[string]any{
		"display_desc": "每月登录赠送",
		"entitlement_base_info": map[string]any{
			"end_time": float64(1796054399),
			"product_extra": map[string]any{
				"package_extra": map[string]any{
					"quota": map[string]any{"credits_limit": float64(300)},
				},
			},
		},
		"usage": map[string]any{},
	}
	got := parseCreditPack(pack)
	if got.Remaining != 300 {
		t.Fatalf("Remaining = %v want 300（usage 空对象代表未使用）", got.Remaining)
	}
}

// 部分使用：额度 1000，已用 400 → 剩余 600。
func TestParseCreditPackPartiallyUsed(t *testing.T) {
	pack := map[string]any{
		"entitlement_base_info": map[string]any{
			"end_time": float64(1794480993),
			"product_extra": map[string]any{
				"package_extra": map[string]any{
					"quota": map[string]any{"credits_limit": float64(1000)},
				},
			},
		},
		"usage": map[string]any{"credits_amount": float64(400)},
	}
	if got := parseCreditPack(pack); got.Remaining != 600 {
		t.Fatalf("Remaining = %v want 600", got.Remaining)
	}
}

// 没有 usage 键（上游格式变更）：无法判断已用多少，剩余保持 0 而不是猜成全额度。
func TestParseCreditPackMissingUsageIsZero(t *testing.T) {
	pack := map[string]any{
		"entitlement_base_info": map[string]any{
			"end_time": float64(1794480993),
			"product_extra": map[string]any{
				"package_extra": map[string]any{
					"quota": map[string]any{"credits_limit": float64(500)},
				},
			},
		},
	}
	if got := parseCreditPack(pack); got.Remaining != 0 {
		t.Fatalf("Remaining = %v want 0（缺 usage 不猜）", got.Remaining)
	}
}

// 超额使用（上游给负数剩余）钳到 0，不能出现负余额。
func TestParseCreditPackClampsNegative(t *testing.T) {
	pack := map[string]any{
		"entitlement_base_info": map[string]any{
			"product_extra": map[string]any{
				"package_extra": map[string]any{
					"quota": map[string]any{"credits_limit": float64(100)},
				},
			},
		},
		"usage": map[string]any{"credits_amount": float64(150)},
	}
	if got := parseCreditPack(pack); got.Remaining != 0 {
		t.Fatalf("Remaining = %v want 0（负数钳零）", got.Remaining)
	}
}

// end_time 缺失时回落顶层 expire_time。
func TestParseCreditPackFallsBackToExpireTime(t *testing.T) {
	pack := map[string]any{
		"expire_time": float64(1796054399),
		"entitlement_base_info": map[string]any{
			"product_extra": map[string]any{
				"package_extra": map[string]any{
					"quota": map[string]any{"credits_limit": float64(100)},
				},
			},
		},
		"usage": map[string]any{},
	}
	if got := parseCreditPack(pack); got.EndTime != 1796054399 {
		t.Fatalf("EndTime = %v want 1796054399（回落 expire_time）", got.EndTime)
	}
}
