// ═══ 更新日志 ═══
// 2026-09-21：保护退回「命中即停止」，夹具改为断言错误码与计数，不再断言可重发标记。
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
	// 真实循环必须在读到正文循环时命中，并按正文侧错误码中止这次请求。
	rec := httptest.NewRecorder()
	streamErr := Stream(rec, strings.NewReader(string(raw)), StreamOptions{
		Model: "global:deepseek-v4.1-flash", ReasoningLoopGuard: true})
	var detailed *StreamError
	if !errors.As(streamErr, &detailed) || detailed.Code != OutputLoopErrorCode {
		t.Fatalf("real capture did not trip the content guard: %v", streamErr)
	}
	// 错误信息里只能有计数，不能回显那串重复正文。
	if strings.Contains(streamErr.Error(), "我执行") {
		t.Fatal("real capture error exposed output text")
	}
	// 命中即停止：客户端只收到一次明确的循环错误，不会拿到成功终态。
	if body := rec.Body.String(); !strings.Contains(body, OutputLoopErrorCode) {
		t.Fatalf("client was not told about the content loop: %d bytes", len(body))
	}
}
