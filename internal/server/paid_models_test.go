package server

// 2026-10-10：付费模型判定用例（倍率 > 0 才是付费；x0.00 是免费），
//             以及 realm 分键、别名剥离。

import "testing"

func TestCreditMultiplierPositive(t *testing.T) {
	cases := []struct {
		raw  string
		want bool
	}{
		// 线上实测形态（2026-10-10 的 6 个模型）。
		{"x0.79", true},          // cn:glm-5.3
		{"x0.06", true},          // cn:glm-5.3-flash
		{"x0.11", true},          // cn:deepseek-v4.1-flash
		{"x0.00", false},         // cn:hy3 / global:hy3 / global:deepseek — 免费
		{"x0.05 credits", true},  // 上游偶发的带后缀形态
		{"x0.00 credits", false}, // 带后缀的免费
		{"0.34", true},           // 无 x 前缀
		{"", false},              // 上游没给
		{"x", false},             // 只有前缀
		{"xabc", false},          // 解析不出数字
		{" credits", false},      // 只有后缀
		{"x-0.5", false},         // 负倍率（异常数据，不当作付费）
	}
	for _, c := range cases {
		if got := creditMultiplierPositive(c.raw); got != c.want {
			t.Errorf("creditMultiplierPositive(%q)=%v want %v", c.raw, got, c.want)
		}
	}
}

func TestStripModelAlias(t *testing.T) {
	cases := []struct{ in, want string }{
		{"deepseek-v4.1-flash", "deepseek-v4.1-flash"},
		{"deepseek-v4.1-flash[1m]", "deepseek-v4.1-flash"},
		{"deepseek-v4.1-flash[1M]", "deepseek-v4.1-flash"},
		{"[1m]", "[1m]"}, // 前缀就是别名：不剥离（避免剥成空串）
		{"", ""},
	}
	for _, c := range cases {
		if got := stripModelAlias(c.in); got != c.want {
			t.Errorf("stripModelAlias(%q)=%q want %q", c.in, got, c.want)
		}
	}
}

// TestPaidModelKeyMatchesPool 键的拼法必须与 pool 侧同构：两处不一致会让
// 付费判定永远查不到（表现为特性静默失效）。
func TestPaidModelKeyMatchesPool(t *testing.T) {
	if got := paidModelKeyForRealm("cn", "m"); got != "cn\x00m" {
		t.Errorf("paidModelKeyForRealm=%q want %q", got, "cn\x00m")
	}
	if got := paidModelKeyForRealm("", "m"); got != "m" {
		t.Errorf("空 realm 应退化为裸名，got %q", got)
	}
}
