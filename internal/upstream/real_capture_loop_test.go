// ═══ 更新日志 ═══
// 2026-09-25：更正夹具来源说明：文件只含通用循环短句与固定测试标识，不代表完整真实抓包回放。
// 2026-09-20：根据线上正文循环现象增加固定夹具回归，锁定正文侧的保护与重试边界。
package upstream

import (
	"errors"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// TestOutputLoopGuardRealCaptureFixture 验证正文侧的循环保护。旧实现只看
// reasoning_content，可能遗漏正文已经循环、推理侧却没有明显重复的情况。
//
// testdata/real_output_loop.sse 是清理后的通用回归夹具，只含循环短句和固定测试标识，
// 共 1201 个 JSON 数据帧及一个结束标记。它不包含真实会话、推理或身份信息，
// 也不能替代真实客户端和上游验收。文件名保留以兼容已有回归入口。
func TestOutputLoopGuardRealCaptureFixture(t *testing.T) {
	raw, err := os.ReadFile("testdata/real_output_loop.sse")
	if err != nil {
		t.Fatalf("real capture fixture missing: %v", err)
	}
	// 闸门必须在客户端零字节时命中，才能整段丢弃并在同账号上重发。
	rec := httptest.NewRecorder()
	streamErr := Stream(rec, strings.NewReader(string(raw)), StreamOptions{
		Model: "global:deepseek-v4.1-flash", ReasoningLoopGuard: true, LoopRetryAvailable: true})
	var detailed *StreamError
	if !errors.As(streamErr, &detailed) || detailed.Code != OutputLoopErrorCode {
		t.Fatalf("real capture did not trip the content guard: %v", streamErr)
	}
	if !detailed.Retryable {
		t.Fatal("real capture was not held back for a same-account retry")
	}
	if body := rec.Body.String(); body != "" {
		t.Fatalf("client saw %d bytes before the retry decision", len(body))
	}
	// 错误信息里只能有计数，不能回显那串重复正文。
	if strings.Contains(streamErr.Error(), "我执行") {
		t.Fatal("real capture error exposed output text")
	}
}
