package pool

import (
	"testing"
	"time"

	"workbuddy2api/internal/auth"
)

// 模型级限流台账的展示口径（2026-10-18 用户要求）：
//   「只有被使用的模型，被明确告知冷却，才在账号里显示模型级的冷却」
//
// 具体到 modelCooldowns 这张表：它同时承载「上游明确限制」与「本地节流」两类条目，
// 台账只该展示前者。11102「该后端无此模型」是模型本身不存在（下线/改名），与账号
// 无关、上游也没给恢复时刻，列进台账会让每个账号挂一排「模型不可用」，把真正在
// 倒计时的 6004 淹没（用户截图反馈的正是这个）。
func TestRateLimitedModelsExcludesRetiredModel(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})

	// 11102：模型不存在 → 不进台账。
	p.BlockModelBackoff("u1", "deepseek-r1-0528", "11102 model not available")

	st, _ := p.Status("u1")
	if len(st.RateLimitedModels) != 0 {
		t.Fatalf("11102 不应出现在限流台账，got %+v", st.RateLimitedModels)
	}
	// 但退避本身仍在（选号侧要避开，避免每轮都撞一次 11102）。
	p.mu.Lock()
	_, tracked := p.byUID["u1"].modelCooldowns["deepseek-r1-0528"]
	p.mu.Unlock()
	if !tracked {
		t.Fatal("11102 的本地退避应仍然记录（只影响展示，不影响选号）")
	}
}

// 6004 带上游重置墙钟 → 必须显示，且带解除时刻（截图 39 那种形态）。
func TestRateLimitedModelsKeeps6004WithReset(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})

	resetAt := time.Now().Add(18 * time.Hour)
	p.CooldownSoftForModel("u1", time.Minute, resetAt, "deepseek-v4.1-flash", "6004 model rate limit")

	st, _ := p.Status("u1")
	if len(st.RateLimitedModels) != 1 {
		t.Fatalf("6004 应出现在限流台账，got %+v", st.RateLimitedModels)
	}
	row := st.RateLimitedModels[0]
	if row.Model != "deepseek-v4.1-flash" {
		t.Fatalf("模型名不符：%q", row.Model)
	}
	if row.ResetAt.IsZero() {
		t.Fatal("6004 应带上游重置墙钟（面板据此显示「解除 <时刻>」）")
	}
}

// 14017（trial 未激活）也是上游明确对「该账号 + 该模型」给出的限制，属台账该展示的
// 内容——排除 11102 时不能把它一起砍掉（那是过度收窄）。
func TestRateLimitedModelsKeepsAccountFaultEntries(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})

	p.CooldownSoftRateForModel("u1", time.Minute, time.Time{}, "glm-5.2", "account fault (14017)")

	st, _ := p.Status("u1")
	if len(st.RateLimitedModels) != 1 {
		t.Fatalf("14017 应出现在限流台账，got %+v", st.RateLimitedModels)
	}
}

// 两类同时存在时，台账只留真限流——这正是修复前的病态：一堆「模型不可用」把
// 唯一一行真限流挤得看不见。
func TestRateLimitedModelsMixesOnlyRealLimits(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})

	p.BlockModelBackoff("u1", "default-1.1", "11102 model not available")
	p.BlockModelBackoff("u1", "kimi-k2-instruct-taiji", "11102 model not available")
	p.CooldownSoftForModel("u1", time.Minute, time.Now().Add(10*time.Hour), "deepseek-v4.1-flash", "6004 model rate limit")

	st, _ := p.Status("u1")
	if len(st.RateLimitedModels) != 1 {
		t.Fatalf("台账应只剩 1 行真限流，got %+v", st.RateLimitedModels)
	}
	if st.RateLimitedModels[0].Model != "deepseek-v4.1-flash" {
		t.Fatalf("留下的应是 6004 那行，got %q", st.RateLimitedModels[0].Model)
	}
}
