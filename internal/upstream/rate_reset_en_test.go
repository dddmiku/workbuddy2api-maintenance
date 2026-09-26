// ═══ 更新日志 ═══
// 2026-09-26：锁定中英文重置时间解析，避免英文 6004 文案被当成「无重置时间的 429」。
package upstream

import "testing"

func TestParseRateResetAcceptsEnglishUsageLimit(t *testing.T) {
	cases := []struct {
		name string
		body string
		want bool
	}{
		{"english_usage_limit", `{"code":6004,"msg":"usage exceeds frequency limit, but don't worry, your usage will reset at 2026-09-27 05:03:51 UTC+8, alternatively, you can switch to the other models to continue using it."}`, true},
		{"english_without_zone", `{"code":6004,"msg":"your usage will reset at 2026-09-27 05:03:51"}`, true},
		{"chinese_still_works", `{"code":6004,"msg":"用量将在 2026-09-27 05:03:51 重置"}`, true},
		{"no_reset", `{"code":14003,"msg":"too many requests"}`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reset, ok := ParseRateReset(tc.body)
			if ok != tc.want {
				t.Fatalf("ok=%v want=%v body=%s", ok, tc.want, tc.body)
			}
			if ok && (reset.Year() != 2026 || reset.Month() != 9 || reset.Day() != 27) {
				t.Fatalf("parsed reset = %s", reset)
			}
		})
	}
}
