// ═══ 更新日志 ═══
// 2026-10-14：初版——锁住「账号池卡片一直显示正在读取」的根因。
package panel

import "testing"

// TestPoolTotalsFromFlatStatus 网关 /status 返回的是**顶层扁平**结构，
// poolTotalsFrom 必须从顶层取值。
//
// 背景（2026-10-14 用户反馈「账号池一直正在读取」）：此前 poolTotalsFrom 取的是
// pool["pool"]（一个不存在的嵌套对象），于是 total/healthy/... 全部为 nil，
// 前端判据 `pool.total != null` 不成立，卡片永远停在「正在读取…」。
//
// 这条测试用真实 /status 的形状（扁平）作为输入，防止再次写成嵌套读取。
func TestPoolTotalsFromFlatStatus(t *testing.T) {
	// 真实网关 /status 的字段布局（2026-10-14 在行空板实测）。
	status := map[string]any{
		"accounts":        []any{},
		"total":           34,
		"healthy":         31,
		"cooling":         2,
		"disabled":        1,
		"in_flight_full":  0,
		"sticky_sessions": 3,
		"realm_totals": map[string]any{
			"cn": map[string]any{"total": 5},
		},
		"redis_mode": "noop",
	}
	got := poolTotalsFrom(status)
	if got.Total != 34 {
		t.Errorf("Total=%v want 34（必须从顶层取，不是 pool[\"pool\"]）", got.Total)
	}
	if got.Healthy != 31 {
		t.Errorf("Healthy=%v want 31", got.Healthy)
	}
	if got.Cooling != 2 {
		t.Errorf("Cooling=%v want 2", got.Cooling)
	}
	if got.Disabled != 1 {
		t.Errorf("Disabled=%v want 1", got.Disabled)
	}
	if got.StickySessions != 3 {
		t.Errorf("StickySessions=%v want 3", got.StickySessions)
	}
	if got.RealmTotals == nil {
		t.Error("RealmTotals 不该为 nil")
	}
}

// TestPoolTotalsFromNestedStatus 兼容嵌套形态（旧 Python 面板 / 未来若改成嵌套）。
// 面板与网关可以各自独立升级，两种布局都要能读。
func TestPoolTotalsFromNestedStatus(t *testing.T) {
	status := map[string]any{
		"pool": map[string]any{
			"total":   10,
			"healthy": 8,
		},
	}
	got := poolTotalsFrom(status)
	if got.Total != 10 || got.Healthy != 8 {
		t.Errorf("嵌套形态: Total=%v Healthy=%v want 10/8", got.Total, got.Healthy)
	}
}

// TestPoolStateFromCreditsExhaustedStaysHealthy 余额不足的账号仍然「可用」。
//
// 2026-10-18 用户口径：「可不可用是针对账号的，冷却和积分余额是模型使用方面的」。
// 用户截图指出 global 域 19 个号里 11 个积分不足被显示成「11 个不可用」——那些号
// 其实都在正常服务免费模型。PoolState.Healthy 只由「在池 / 停用 / 熔断」决定。
func TestPoolStateFromCreditsExhaustedStaysHealthy(t *testing.T) {
	// 网关 /status 里一个积分不足、未停用、未熔断的账号。
	st := poolStateFrom(map[string]any{
		"uid":               "u1",
		"credits_exhausted": true,
		"cooling":           false,
		"disabled":          false,
	})
	if !st.Healthy {
		t.Errorf("余额不足的账号应仍为可用（healthy=true）: %+v", st)
	}
	if !st.CreditsExhausted {
		t.Error("credits_exhausted 应原样透传，供面板显示余额不足标记")
	}

	// 对照：熔断 / 停用才是账号级不可用。
	for _, tc := range []struct {
		name  string
		entry map[string]any
	}{
		{"breaker", map[string]any{"cooling": true}},
		{"disabled", map[string]any{"disabled": true}},
	} {
		if got := poolStateFrom(tc.entry); got.Healthy {
			t.Errorf("%s 应判为不可用: %+v", tc.name, got)
		}
	}
}
