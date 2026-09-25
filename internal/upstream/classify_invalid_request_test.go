// ═══ 更新日志 ═══
// 2026-09-26：锁定明确参数错误不被 "too many" 限流词表误判，限流与内容拦截分类不变。
package upstream

import "testing"

func TestClassifyExplicitInvalidRequestBeforeRateWords(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   ErrKind
	}{
		{"11133_too_many_images", 400, `{"code":11133,"msg":"too many images in request","extError":{"code":"model_param_invalid","type":"invalid_request_error"}}`, ErrClient},
		{"11101_too_many_tools", 400, `{"code":11101,"msg":"Unmarshal chat params failed: too many tools"}`, ErrBadParams},
		{"openai_style_invalid", 400, `{"error":{"type":"invalid_request_error","message":"too many stop sequences"}}`, ErrClient},
		{"explicit_rate_limit_kept", 400, `{"code":11140,"msg":"The model provider is rate-limiting requests.","extError":{"type":"invalid_request_error"}}`, ErrSoftRate},
		{"status_429_kept", 429, `{"code":11133,"msg":"too many"}`, ErrSoftRate},
		{"plain_too_many_kept", 400, `{"code":12345,"msg":"too many concurrent sessions"}`, ErrSoftRate},
		{"content_blocked_kept", 400, `{"code":11133,"msg":"blocked by security policy","extError":{"type":"invalid_request_error"}}`, ErrContentBlocked},
		{"context_kept", 400, `{"code":11115,"msg":"prompt is too long: 2 tokens > 1 maximum","extError":{"type":"invalid_request_error"}}`, ErrContextTooLong},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Classify(tc.status, tc.body); got != tc.want {
				t.Fatalf("Classify = %s, want %s", got, tc.want)
			}
		})
	}
}
