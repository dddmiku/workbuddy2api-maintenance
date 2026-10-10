package pool

// 2026-10-10：付费模型的「快过期优先」硬过滤用例：付费模型只从快过期号里选、
//             免费模型不受影响、无快过期号时退回全体候选。

import (
	"testing"

	"workbuddy2api/internal/auth"
)

// newPaidTestPool 两个号：fast（快过期多）、slow（无快过期），两者其它条件相同。
// 余额都非 0，避免被 credits 因子干扰。
func newPaidTestPool() *Pool {
	p := New("")
	p.Add(&auth.Auth{UID: "fast"})
	p.Add(&auth.Auth{UID: "slow"})
	p.SetCreditsDetailed("fast", 1000, 900) // 90% 快过期
	p.SetCreditsDetailed("slow", 1000, 0)   // 无快过期
	return p
}

// TestPaidModelPrefersExpiringHard 付费模型上，只要 fast 有快过期积分，
// 选号就必须落在 fast 上——不是"更可能"，是"只从它里面选"（硬过滤）。
func TestPaidModelPrefersExpiringHard(t *testing.T) {
	p := newPaidTestPool()
	p.SetPaidModels(map[string]bool{"paid-model": true})
	// 抽 200 次：硬过滤下 fast 应独占。
	for i := 0; i < 200; i++ {
		got := p.PickExcludingForRealm(nil, "paid-model", "")
		if got == nil {
			t.Fatal("pick returned nil")
		}
		if got.UID != "fast" {
			t.Fatalf("第 %d 次选中 %s，付费模型应只选有快过期积分的号", i, got.UID)
		}
	}
}

// TestFreeModelIgnoresExpiring 免费模型（倍率 0 → 不在付费表里）不受该过滤影响：
// slow 仍会被选中，否则等于把免费流量也硬压在快过期号上，白白占它的并发名额。
func TestFreeModelIgnoresExpiring(t *testing.T) {
	p := newPaidTestPool()
	p.SetPaidModels(map[string]bool{"paid-model": true}) // free-model 不在表里
	seen := map[string]int{}
	for i := 0; i < 300; i++ {
		got := p.PickExcludingForRealm(nil, "free-model", "")
		if got == nil {
			t.Fatal("pick returned nil")
		}
		seen[got.UID]++
	}
	if seen["slow"] == 0 {
		t.Errorf("免费模型不该被快过期过滤：300 次里 slow 一次没被选中（seen=%v）", seen)
	}
}

// TestPaidModelNoExpiringFallsBack 付费模型但没有任何号有快过期积分时，
// 过滤不生效（不能把请求卡死），按原权重正常选。
func TestPaidModelNoExpiringFallsBack(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "a"})
	p.Add(&auth.Auth{UID: "b"})
	p.SetCreditsDetailed("a", 1000, 0)
	p.SetCreditsDetailed("b", 1000, 0)
	p.SetPaidModels(map[string]bool{"paid-model": true})
	seen := map[string]int{}
	for i := 0; i < 200; i++ {
		got := p.PickExcludingForRealm(nil, "paid-model", "")
		if got == nil {
			t.Fatal("无快过期号时不该选不出号")
		}
		seen[got.UID]++
	}
	if len(seen) != 2 {
		t.Errorf("无快过期积分时应按原权重在全体里选，seen=%v", seen)
	}
}

// TestPaidModelAllExpiringNoFilter 全体候选都有快过期积分时过滤无差别，
// 仍要能选出号（且不因过滤逻辑出错）。
func TestPaidModelAllExpiringNoFilter(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "a"})
	p.Add(&auth.Auth{UID: "b"})
	p.SetCreditsDetailed("a", 1000, 500)
	p.SetCreditsDetailed("b", 1000, 100)
	p.SetPaidModels(map[string]bool{"paid-model": true})
	seen := map[string]int{}
	for i := 0; i < 200; i++ {
		got := p.PickExcludingForRealm(nil, "paid-model", "")
		if got == nil {
			t.Fatal("pick returned nil")
		}
		seen[got.UID]++
	}
	if len(seen) != 2 {
		t.Errorf("全员快过期时不该只剩一个号，seen=%v", seen)
	}
}

// TestPaidModelFilterYieldsToTried 快过期号被 tried 排除后，过滤必须让位——
// 否则同一请求的重试轮换会选不出号（宁可花长期积分，也不能让请求失败）。
func TestPaidModelFilterYieldsToTried(t *testing.T) {
	p := newPaidTestPool()
	p.SetPaidModels(map[string]bool{"paid-model": true})
	// 第一次选中 fast（硬过滤生效）。
	first := p.PickExcludingForRealm(nil, "paid-model", "")
	if first == nil || first.UID != "fast" {
		t.Fatalf("首次应选 fast，got %+v", first)
	}
	// 把 fast 标为已试过：过滤后候选为空 → 必须退回全体（slow）。
	second := p.PickExcludingForRealm(map[string]bool{"fast": true}, "paid-model", "")
	if second == nil {
		t.Fatal("快过期号被排除后应退回其它号，而不是选不出")
	}
	if second.UID != "slow" {
		t.Errorf("got %s want slow（fast 已 tried）", second.UID)
	}
}

// TestPaidModelRealmScoped 倍率按 (realm, 模型) 判定：CN 侧付费不该让同名 global
// 模型也被当成付费（实测 cn:deepseek-v4.1-flash=x0.11 而 global 侧=x0.00）。
func TestPaidModelRealmScoped(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "cn1", Domain: "www.codebuddy.cn"})
	p.Add(&auth.Auth{UID: "g1", Domain: "www.workbuddy.ai"})
	p.SetCreditsDetailed("cn1", 1000, 900)
	p.SetCreditsDetailed("g1", 1000, 0)
	// 只登记 CN 侧付费。
	p.SetPaidModels(map[string]bool{paidModelKey("cn", "deepseek-v4.1-flash"): true})

	// CN 付费模型：只选 cn1（快过期）。
	for i := 0; i < 100; i++ {
		got := p.PickExcludingForRealm(nil, "deepseek-v4.1-flash", "cn")
		if got == nil || got.UID != "cn1" {
			t.Fatalf("CN 付费模型应只选快过期号，got %+v", got)
		}
	}
	// global 同名模型：不在付费表里 → 不受过滤，g1 也能被选中。
	seen := map[string]int{}
	for i := 0; i < 200; i++ {
		got := p.PickExcludingForRealm(nil, "deepseek-v4.1-flash", "global")
		if got == nil {
			t.Fatal("global pick nil")
		}
		seen[got.UID]++
	}
	if seen["g1"] == 0 {
		t.Errorf("global 同名模型不该被 CN 的付费判定影响，seen=%v", seen)
	}
}

// TestNoPaidTableDisablesFeature 未注入倍率表时特性整体关闭（零回归）。
func TestNoPaidTableDisablesFeature(t *testing.T) {
	p := newPaidTestPool()
	// 不调用 SetPaidModels。
	seen := map[string]int{}
	for i := 0; i < 300; i++ {
		got := p.PickExcludingForRealm(nil, "any-model", "")
		if got == nil {
			t.Fatal("pick returned nil")
		}
		seen[got.UID]++
	}
	if seen["slow"] == 0 {
		t.Errorf("未注入倍率表时不该有硬过滤，seen=%v", seen)
	}
}

// TestPaidModelFilterRecordsDecision 过滤发生时决策事实里要有痕迹，
// 否则运维无法确认该特性是否在生效。
func TestPaidModelFilterRecordsDecision(t *testing.T) {
	p := newPaidTestPool()
	p.SetPaidModels(map[string]bool{"paid-model": true})
	_, d := p.PickExcludingForRealmWithDecision(nil, "paid-model", "")
	if d.StageCounts["paid_expiring_preferred"] == 0 {
		t.Errorf("决策应记录 paid_expiring_preferred，stage=%v", d.StageCounts)
	}
	if d.ExcludedCounts["paid_expiring_deferred"] == 0 {
		t.Errorf("决策应记录被推迟的号数，excluded=%v", d.ExcludedCounts)
	}
}

// TestPaidModelFilterKeepsOtherFactors 过滤只换候选集，不改变后续权重口径：
// 两个快过期号之间仍按原三因子加权（credits 高的更可能被选中）。
//
// 关掉 minPickGap：它默认 100ms，紧循环里会把上一个号挤出候选、强制严格轮换，
// 分布断言就测不出权重了（这是防惊群在起作用，不是权重失效）。
func TestPaidModelFilterKeepsOtherFactors(t *testing.T) {
	withNoPickGap(t)
	p := New("")
	p.Add(&auth.Auth{UID: "rich"})
	p.Add(&auth.Auth{UID: "poor"})
	// 两者都有快过期积分（都过过滤），但 rich 余额高得多 → 权重应显著更高。
	p.SetCreditsDetailed("rich", 10000, 100)
	p.SetCreditsDetailed("poor", 100, 100)
	p.SetPaidModels(map[string]bool{"paid-model": true})
	rich, poor := 0, 0
	for i := 0; i < 400; i++ {
		switch got := p.PickExcludingForRealm(nil, "paid-model", ""); {
		case got == nil:
			t.Fatal("pick returned nil")
		case got.UID == "rich":
			rich++
		default:
			poor++
		}
	}
	if rich <= poor {
		t.Errorf("过滤后仍应按原权重（余额高的更多被选）：rich=%d poor=%d", rich, poor)
	}
}

// TestPaidModelKeyRoundTrip 键的拼法与 handler 侧一致（两处必须同构，改了要同步）。
func TestPaidModelKeyRoundTrip(t *testing.T) {
	if got := paidModelKey("cn", "m"); got != "cn\x00m" {
		t.Errorf("paidModelKey=%q want %q", got, "cn\x00m")
	}
	if got := paidModelKey("", "m"); got != "m" {
		t.Errorf("空 realm 应退化为裸名，got %q", got)
	}
}
