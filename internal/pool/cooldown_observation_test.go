package pool

import (
	"testing"
	"time"

	"workbuddy2api/internal/auth"
)

// 限流/积分不足路径必须留下「最近活动」观测：面板此前对只被限流过、从未成功过的
// 账号显示「冷却中 + 从未活动」的自相矛盾组合（用户实测反馈）。这里锁定各入口都写
// last_err，且都不污染成功率权重（errTotal/errorEMA/fails 保持不变）。
//
// 2026-10-18 改：账号级限流冷却已删除（用户要求「不要账号冷却，只要模型冷却和积分不足」）。
// 现在只有两类状态：
//   - 积分不足（CoolHard）→ 账号级 until + CreditsExhausted，**不是冷却**；
//   - 模型限流 → RateLimitedModels 台账，账号本身照常可用（Cooling 恒 false）。
// Cooldown(CoolSoft) 已不写任何状态，因此「固定时长软冷却」用例改为直接断言这一点。

func newObservedPool(t *testing.T) *Pool {
	t.Helper()
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	return p
}

// assertObservedLastErr 断言 last_err 已写、且未计入失败统计（与是否冷却无关）。
func assertObservedLastErr(t *testing.T, p *Pool) {
	t.Helper()
	st, ok := p.Status("u1")
	if !ok {
		t.Fatal("status(u1) missing")
	}
	if st.LastErrTime.IsZero() {
		t.Error("限流/余额不足必须记录 last_err，否则面板显示「从未」")
	}
	if st.ErrTotal != 0 {
		t.Errorf("err_total=%d want 0: 限流不是账号故障", st.ErrTotal)
	}
}

// assertModelLimited 断言某模型在限流台账里且账号未被整体冷却。
func assertModelLimited(t *testing.T, p *Pool, model, wantReason string) {
	t.Helper()
	st, ok := p.Status("u1")
	if !ok {
		t.Fatal("status(u1) missing")
	}
	if st.Cooling {
		t.Errorf("模型级限流不得把整号标成冷却（其他模型应照常可用）: %+v", st)
	}
	found := false
	for _, m := range st.RateLimitedModels {
		if m.Model == model {
			found = true
			if wantReason != "" && m.Reason != wantReason {
				t.Errorf("model %s reason=%q want %q", model, m.Reason, wantReason)
			}
		}
	}
	if !found {
		t.Errorf("模型 %s 未出现在限流台账: %+v", model, st.RateLimitedModels)
	}
}

// 账号级 CoolSoft 已取消：调用它不写任何冷却状态（防止将来有人又加回账号级限流）。
func TestCooldownRecordsLastErr(t *testing.T) {
	p := newObservedPool(t)
	p.Cooldown("u1", CoolSoft, time.Minute, "account fault (14017)")
	assertObservedLastErr(t, p)
	st, _ := p.Status("u1")
	if st.Cooling || st.CreditsExhausted || !st.Until.IsZero() {
		t.Errorf("CoolSoft 账号级冷却已取消，不得写 until/cooling: %+v", st)
	}
}

// 积分不足（余额耗尽，等次日签到）不是冷却，单独由 CreditsExhausted 表达。
func TestCooldownUntilTomorrow4AMRecordsLastErr(t *testing.T) {
	p := newObservedPool(t)
	p.CooldownUntilTomorrow4AM("u1", "余额不足")
	assertObservedLastErr(t, p)
	st, _ := p.Status("u1")
	if st.Cooling {
		t.Errorf("余额不足不是冷却，Cooling 应 false: %+v", st)
	}
	if !st.CreditsExhausted {
		t.Errorf("余额不足应置 CreditsExhausted: %+v", st)
	}
	if st.CoolKind != "hard_credit" {
		t.Errorf("cool_kind=%q want hard_credit", st.CoolKind)
	}
	if st.Reason != "余额不足" {
		t.Errorf("reason=%q want 余额不足", st.Reason)
	}
}

// 带上游重置墙钟的限流：记在模型上，账号不冷却。
func TestCooldownSoftRateRecordsLastErr(t *testing.T) {
	p := newObservedPool(t)
	reset := time.Now().Add(30 * time.Minute)
	p.CooldownSoftRateForModel("u1", time.Minute, reset, CooldownTestModel, "429 rate limit")
	assertObservedLastErr(t, p)
	assertModelLimited(t, p, CooldownTestModel, "429 rate limit")
	if d := p.byUID["u1"].modelCooldowns[CooldownTestModel].Until.Sub(reset); d < -time.Second || d > time.Second {
		t.Errorf("until 应对齐上游重置时间，实际偏差 %v", d)
	}
}

// 无重置时间：按该模型做有界退避，同样不碰账号级 until。
func TestCooldownSoftRateBackoffRecordsLastErr(t *testing.T) {
	p := newObservedPool(t)
	p.CooldownSoftRateForModel("u1", time.Minute, time.Time{}, CooldownTestModel, "429 rate limit")
	assertObservedLastErr(t, p)
	assertModelLimited(t, p, CooldownTestModel, "429 rate limit")
}

// 模型级 6004 冷却：账号级 until 不写（切模型仍可用），但观测时间要写，
// 否则被限流的号在面板里同样显示「从未」。
func TestCooldownSoftForModelRecordsLastErr(t *testing.T) {
	p := newObservedPool(t)
	p.CooldownSoftForModel("u1", time.Minute, time.Now().Add(time.Hour), "glm-5.3", "6004 model rate limit")
	assertObservedLastErr(t, p)
	assertModelLimited(t, p, "glm-5.3", "6004 model rate limit")
}

// 11102 负缓存（该账号无此模型）同样留观测。
func TestBlockModelBackoffRecordsLastErr(t *testing.T) {
	p := newObservedPool(t)
	p.BlockModelBackoff("u1", "hy3-x", "11102 model blocked")
	assertObservedLastErr(t, p)
	assertModelLimited(t, p, "hy3-x", "11102 model blocked")
}

// 观测要落盘：重启后「最近活动」不能回到「从未」，积分不足状态也要保留。
func TestCooldownObservationPersists(t *testing.T) {
	fp := t.TempDir() + "/state.json"
	p := New(fp)
	p.Add(&auth.Auth{UID: "u1"})
	p.CooldownUntilTomorrow4AM("u1", "余额不足")
	p.Flush()
	p.Close()

	reloaded := New(fp)
	defer reloaded.Close()
	reloaded.Add(&auth.Auth{UID: "u1"})
	st, _ := reloaded.Status("u1")
	if st.LastErrTime.IsZero() {
		t.Error("last_err lost after reload")
	}
	if !st.CreditsExhausted {
		t.Errorf("积分不足状态 lost after reload: %+v", st)
	}
}
