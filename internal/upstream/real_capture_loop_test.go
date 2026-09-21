// ═══ 更新日志 ═══
// 2026-09-20：用线上真实抓包的循环字节做回归，替代纯合成夹具，锁定「正文循环」形态。
package upstream

import (
	"errors"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// TestOutputLoopGuardRealCaptureFixture 用线上真实抓到的循环字节做回归。2026-09-20 的
// 抓包里，正文是「我执行。」重复 127230 行、唯一行只有 1 个（占比 100%），而推理侧
// 只有 3736 字符、重复覆盖 23%。旧实现只看 reasoning_content，因此既不中断也不重试。
//
// testdata/real_output_loop.sse 取自那次抓包的前 700 帧，保留原始 UTF-8 字节与分帧
// 边界，不重新构造文本；目的是让「真实循环能被拦住」这件事不依赖合成样本的假设。
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
