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

// 11140 的两种含义：displayMsg 指明内容审核拒绝时按请求级处理，不判账号故障。
func TestClassifySeparatesContentReviewFromAccountBan(t *testing.T) {
	review := `{"code":11140,"msg":"request illegal","requestId":"af11e881","displayMsg":{"en":"The content did not pass the safety review. Please adjust and retry.","zh":"内容未通过安全审核，请修改后重试。"}}`
	if got := Classify(403, review); got != ErrContentBlocked {
		t.Fatalf("content review classified as %s, want content_blocked", got)
	}
	zhOnly := `{"code":11140,"msg":"request illegal","displayMsg":{"zh":"内容未通过安全审核，请修改后重试。"}}`
	if got := Classify(403, zhOnly); got != ErrContentBlocked {
		t.Fatalf("chinese content review classified as %s, want content_blocked", got)
	}
	ban := `{"code":11140,"msg":"request illegal","requestId":"x","displayMsg":{"en":"Forbidden","zh":"无权限"}}`
	if got := Classify(403, ban); got != ErrAccountFault {
		t.Fatalf("genuine authorization fault classified as %s, want account_fault", got)
	}
}
