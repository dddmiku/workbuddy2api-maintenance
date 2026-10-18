package pool

import (
	"os"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
)

// 熔断不该冒充「余额不足」（2026-10-18 prod 实测）。
//
// 现场：6 个 CN 号 credits=4178~5425（积分充足）、直连上游 glm-5.3 返回 200，
// 但面板全部显示 cool_kind=hard_credit。真因两层叠加：
//
//  1. CoolHard 是 CoolKind 的 iota 零值，与 JSON 缺字段的零值同值。persist.go 的
//     `if kind != CoolHard { 丢弃 }` 因而拦不住零值——每个账号重启后都被恢复成
//     coolKind=CoolHard，哪怕它从未冷却过（until 是零值）。
//  2. statusOf 的 Cooling 分支只要 Cooling=true 就无条件写
//     st.CoolKind = e.coolKind.String()，而 Cooling **也包含纯熔断**
//     （breakerUntil 在未来、until 是零值）。于是熔断被渲染成「余额不足」。
//
// 两条都必须挡住：只修 (2) 的话内存里的 coolKind 仍是脏的，任何别的读取方
// 还会看到 CoolHard；只修 (1) 的话，真冷却过期后残留的 kind 一样会漏出来。
func TestBreakerOnlyDoesNotLeakHardCreditKind(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})

	// 只开熔断：until 保持零值（= 从未冷却）。三次失败达到 breakerThreshold。
	p.mu.Lock()
	p.byUID["u1"].breakerUntil = time.Now().Add(30 * time.Minute)
	p.byUID["u1"].retryCount = 1
	p.mu.Unlock()

	st, ok := p.Status("u1")
	if !ok {
		t.Fatal("账号应存在")
	}
	if !st.Cooling {
		t.Fatal("熔断期应判 Cooling=true（面板据此显示「熔断中」）")
	}
	if st.CoolKind != "" {
		t.Errorf("纯熔断不该带 cool_kind，got %q（会被渲染成「余额不足」）", st.CoolKind)
	}
	if st.CreditsExhausted {
		t.Error("纯熔断不是积分耗尽")
	}
	if st.CoolRemaining <= 0 {
		t.Errorf("熔断剩余时长应 > 0，got %d", st.CoolRemaining)
	}
}

// 真冷却（until 生效 + CoolHard）仍必须显示 hard_credit——收窄不能砍掉正例。
func TestActiveHardCreditStillReportsKind(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.CooldownUntilTomorrow4AM("u1", "balance insufficient")

	st, _ := p.Status("u1")
	if !st.CreditsExhausted {
		t.Fatal("余额不足应置 CreditsExhausted")
	}
	if st.CoolKind != "hard_credit" {
		t.Errorf("生效中的余额不足应显示 hard_credit，got %q", st.CoolKind)
	}
}

// 重启恢复：state.json 里 cool_kind=0 但 until 是零值 → 不是余额不足。
//
// 这条是 bug 的入口——旧文件把「从未冷却」也写成 cool_kind:0，恢复侧必须靠
// until 判真伪，而不是靠枚举值（零值与 CoolHard 同值，判不出来）。
func TestRestoreIgnoresKindWhenUntilIsZero(t *testing.T) {
	dir := t.TempDir()
	fp := dir + "/state.json"
	// 旧文件形态：cool_kind=0、until 零值、credits 充足。这正是 prod 现状。
	old := `{"accounts":{"u1":{"credits":4178,"cool_kind":0,"until":"0001-01-01T00:00:00Z"}}}`
	if err := os.WriteFile(fp, []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}

	p := New(fp)
	p.Add(&auth.Auth{UID: "u1"})

	p.mu.RLock()
	until := p.byUID["u1"].until
	p.mu.RUnlock()
	if !until.IsZero() {
		t.Fatalf("until 应为零值，got %v", until)
	}
	// 注意：不能断言 kind != CoolHard——CoolHard 就是枚举零值，两者永远相等。
	// 真正能区分的是 until（上面已断言零值）与对外的 Status：只要 until 不生效，
	// statusOf 就不会把 kind 透出去。这正是本 bug 的修法。

	st, _ := p.Status("u1")
	if st.CreditsExhausted || st.Cooling || st.CoolKind != "" {
		t.Errorf("积分充足 + 未冷却的号不该有任何冷却状态: %+v", st)
	}
}
