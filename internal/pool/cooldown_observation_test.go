package pool

import (
	"testing"
	"time"

	"workbuddy2api/internal/auth"
)

// 冷却路径必须留下「最近活动」观测：面板此前对只被限流过、从未成功过的账号显示
// 「冷却中 + 从未活动」的自相矛盾组合（用户实测反馈）。这里锁定四条冷却入口都写
// last_err，且都不污染成功率权重（errTotal/errorEMA/fails 保持不变）。

func newObservedPool(t *testing.T) *Pool {
	t.Helper()
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	return p
}

// assertObservedCooling 断言账号处于冷却、last_err 已写、且未计入失败统计。
func assertObservedCooling(t *testing.T, p *Pool, wantReason string) {
	t.Helper()
	st, ok := p.Status("u1")
	if !ok {
		t.Fatal("status(u1) missing")
	}
	if !st.Cooling {
		t.Fatalf("expected cooling: %+v", st)
	}
	if st.LastErrTime.IsZero() {
		t.Error("cooldown must record last_err so the panel stops showing 从未")
	}
	if wantReason != "" && st.Reason != wantReason {
		t.Errorf("reason=%q want %q", st.Reason, wantReason)
	}
	if st.ErrTotal != 0 {
		t.Errorf("err_total=%d want 0: rate limit is not an account failure", st.ErrTotal)
	}
}

// 固定时长软冷却（429 无重置时间 / 404 / 14017 共用 Cooldown 入口）。
func TestCooldownRecordsLastErr(t *testing.T) {
	p := newObservedPool(t)
	p.Cooldown("u1", CoolSoft, time.Minute, "account fault (14017)")
	assertObservedCooling(t, p, "account fault (14017)")
}

// 硬冷却（余额耗尽，等次日签到）同样要留观测。
func TestCooldownUntilTomorrow4AMRecordsLastErr(t *testing.T) {
	p := newObservedPool(t)
	p.CooldownUntilTomorrow4AM("u1", "余额不足")
	assertObservedCooling(t, p, "余额不足")
}

// 账号级软限流（带上游重置墙钟）。
func TestCooldownSoftRateRecordsLastErr(t *testing.T) {
	p := newObservedPool(t)
	p.CooldownSoftRate("u1", time.Minute, time.Now().Add(30*time.Minute), "429 rate limit")
	assertObservedCooling(t, p, "429 rate limit")
}

// 无重置时间的有界退避分支。
func TestCooldownSoftRateBackoffRecordsLastErr(t *testing.T) {
	p := newObservedPool(t)
	p.CooldownSoftRate("u1", time.Minute, time.Time{}, "429 rate limit")
	assertObservedCooling(t, p, "429 rate limit")
}

// 模型级 6004 冷却：账号级 until 不写（切模型仍可用），但观测时间要写，
// 否则被限流的号在面板里同样显示「从未」。
func TestCooldownSoftForModelRecordsLastErr(t *testing.T) {
	p := newObservedPool(t)
	p.CooldownSoftForModel("u1", time.Minute, time.Now().Add(time.Hour), "glm-5.3", "6004 model rate limit")
	st, _ := p.Status("u1")
	if st.LastErrTime.IsZero() {
		t.Error("model-level cooldown must record last_err")
	}
	if st.Cooling {
		t.Error("6004 must not set account-level cooling: other models stay usable")
	}
	if st.ErrTotal != 0 {
		t.Errorf("err_total=%d want 0: 6004 is not an account failure", st.ErrTotal)
	}
}

// 11102 负缓存（该账号无此模型）同样留观测。
func TestBlockModelBackoffRecordsLastErr(t *testing.T) {
	p := newObservedPool(t)
	p.BlockModelBackoff("u1", "hy3-x", "11102 model blocked")
	st, _ := p.Status("u1")
	if st.LastErrTime.IsZero() {
		t.Error("11102 negative cache must record last_err")
	}
	if st.ErrTotal != 0 {
		t.Errorf("err_total=%d want 0: model unavailability is not an account failure", st.ErrTotal)
	}
}

// 观测要落盘：重启后「最近活动」不能回到「从未」。
func TestCooldownObservationPersists(t *testing.T) {
	fp := t.TempDir() + "/state.json"
	p := New(fp)
	p.Add(&auth.Auth{UID: "u1"})
	p.Cooldown("u1", CoolSoft, time.Hour, "429 rate limit")
	p.Flush()
	p.Close()

	reloaded := New(fp)
	defer reloaded.Close()
	reloaded.Add(&auth.Auth{UID: "u1"})
	st, _ := reloaded.Status("u1")
	if st.LastErrTime.IsZero() {
		t.Error("last_err lost after reload")
	}
	if !st.Cooling {
		t.Errorf("cooling window lost after reload: %+v", st)
	}
}
