// ═══ 更新日志 ═══
// 2026-10-18：锁住「6004 冷却只锁被限流的那个模型」这条语义。
//
// 用户看到的现场：deepseek-v4.1-flash 提示「19 小时后重置」，但同一个账号上
// step-5-preview / glm-5.3 / kimi-k3 照常返回 200。这是**正确行为**——6004 是
// 模型级限流，不是账号封禁。但「19 小时」的数字容易让人误以为是账号级的，
// 所以这里把两层维度的隔离钉死：改错任何一处，这些测试就会红。
package pool

import (
	"testing"
	"time"

	"workbuddy2api/internal/auth"
)

// TestModelCooldownIsolatesToThatModel 一个模型冷却，不影响同账号的其他模型。
func TestModelCooldownIsolatesToThatModel(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})

	p.CooldownSoftForModel("u1", time.Minute, time.Now().Add(19*time.Hour),
		"deepseek-v4.1-flash", "6004 model rate limit")

	now := time.Now()
	e := p.byUID["u1"]
	if got := e.unavailableReason(now, "deepseek-v4.1-flash"); got != "model_cooldown" {
		t.Errorf("被限流的模型应返回 model_cooldown，实际 %q", got)
	}
	for _, other := range []string{"step-5-preview", "glm-5.3", "kimi-k3", "glm-5.2"} {
		if got := e.unavailableReason(now, other); got != "" {
			t.Errorf("同账号的其他模型 %s 应可用，实际 %q —— 模型级冷却被扩大成账号级了", other, got)
		}
	}
	// 账号级 until 必须保持零值：6004 绝不写账号级冷却。
	if !e.until.IsZero() {
		t.Errorf("6004 不该写账号级 until，实际 %v", e.until)
	}
}

// TestPickSkipsOnlyCooledModel 选号侧同样只跳过被限流的模型。
func TestPickSkipsOnlyCooledModel(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "t"})
	p.CooldownSoftForModel("u1", time.Minute, time.Now().Add(19*time.Hour),
		"deepseek-v4.1-flash", "6004 model rate limit")

	if got := p.PickExcludingForRealm(nil, "deepseek-v4.1-flash", ""); got != nil {
		t.Errorf("被限流的模型不该选出账号，实际 %v", got.UID)
	}
	if got := p.PickExcludingForRealm(nil, "step-5-preview", ""); got == nil {
		t.Error("其他模型应能选出同一个账号")
	}
}

// TestHardCreditFreeModelStillBlockedByModelCooldown 余额不足 + 模型冷却叠加时，
// 免费模型也要被模型冷却拦住（余额放行不能越过模型级限流）。
func TestHardCreditFreeModelStillBlockedByModelCooldown(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	// 倍率表里只登记一个付费模型，其余（含下面用到的两个）都按免费处理。
	// 注意不能传空表：SetPaidModels 收到空表会把 paidModels 设为 nil，而
	// hardCreditBlocksModel 对 nil 一律拦截（「宁可少放行」的保守设计），
	// 那会让本测试失去意义。
	p.SetPaidModels(map[string]bool{"cn\x00paid-only": true})

	// 余额不足 → 账号级硬冷却
	p.Cooldown("u1", CoolHard, 19*time.Hour, "余额不足")
	// 同一账号上某个免费模型又被 6004 限流
	p.CooldownSoftForModel("u1", time.Minute, time.Now().Add(19*time.Hour),
		"deepseek-v4.1-flash", "6004 model rate limit")

	now := time.Now()
	e := p.byUID["u1"]
	// 免费模型：余额不足放行，但仍要看模型冷却 → 被限流的那个模型必须拦住。
	if got := e.unavailableReason(now, "deepseek-v4.1-flash"); got != "model_cooldown" {
		t.Errorf("余额不足的号在免费但被限流的模型上应返回 model_cooldown，实际 %q", got)
	}
	// 其他免费模型：余额放行 + 无模型冷却 → 可用。
	if got := e.unavailableReason(now, "step-5-preview"); got != "" {
		t.Errorf("余额不足的号在未限流的免费模型上应可用，实际 %q", got)
	}
}
