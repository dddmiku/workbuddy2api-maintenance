// ═══ 更新日志 ═══
// 2026-10-11：9074 限流改为具名错误：调用方靠 errors.As 判定，不匹配中文文案。
package trae

import (
	"errors"
	"strings"
	"testing"
)

// TestRateLimitErrorIsTyped 9074 必须是可判定的具名错误，而不是靠字符串匹配。
//
// 上游 message 是中文文案（"当前参与用户太多，请稍后再试"），会随上游改版漂移；
// 调度器据此决定「当天不再重试」，判错会导致限流窗口被反复延长。
func TestRateLimitErrorIsTyped(t *testing.T) {
	err := error(&RateLimitError{Message: "当前参与用户太多，请稍后再试"})
	if !IsRateLimited(err) {
		t.Fatal("9074 应被 IsRateLimited 识别")
	}
	// 包装后仍可识别（errors.As 语义）。
	wrapped := errors.Join(errors.New("outer"), err)
	if !IsRateLimited(wrapped) {
		t.Fatal("包装后仍应可识别")
	}
	// 其它错误不得误判。
	if IsRateLimited(errors.New("trae checkin: code 500: boom")) {
		t.Fatal("非 9074 不该被识别为限流")
	}
	if IsRateLimited(nil) {
		t.Fatal("nil 不该被识别为限流")
	}
	// 文案里带码值，便于日志直接读。
	if got := err.Error(); !strings.Contains(got, "9074") {
		t.Fatalf("错误文案应含码值，实际 %q", got)
	}
}
