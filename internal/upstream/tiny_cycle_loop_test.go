// ═══ 更新日志 ═══
// 2026-09-23：新增「极短行严格周期循环」回归：线上截图里的「好。」/「执行。」两行交替
// 必须在客户端零字节时命中并可同账号重发；表格类交替重复必须继续放行。
package upstream

import (
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestOutputLoopGuardCatchesTinyAlternatingContent 复现线上漏检：正文是「好。」与
// 「执行。」严格交替。旧规则下窗口里只有 2 种短行、覆盖率 100%，但两行各占 50%，
// 过不了「一行占多数」这条兜底，保护完全不触发，网页被顶死。
func TestOutputLoopGuardCatchesTinyAlternatingContent(t *testing.T) {
	var text strings.Builder
	for index := 0; index < 400; index++ {
		if index%2 == 0 {
			text.WriteString("好。\n")
		} else {
			text.WriteString("执行。\n")
		}
	}
	raw := outputGuardTestFrame(text.String()) + "data: [DONE]\n\n"

	rec := httptest.NewRecorder()
	err := Stream(rec, strings.NewReader(raw), StreamOptions{
		Model: "global:deepseek-v4.1-flash", ReasoningLoopGuard: true, LoopRetryAvailable: true})
	var streamErr *StreamError
	if !errors.As(err, &streamErr) || streamErr.Code != OutputLoopErrorCode {
		t.Fatalf("tiny alternating content was not detected: %v", err)
	}
	if !streamErr.Retryable {
		t.Fatal("tiny alternating content was not held back for a same-account retry")
	}
	if body := rec.Body.String(); body != "" {
		t.Fatalf("client saw %d bytes before the retry decision", len(body))
	}
	if strings.Contains(err.Error(), "好。") || strings.Contains(err.Error(), "执行。") {
		t.Fatal("loop error exposed output text")
	}
}

// TestReasoningLoopGuardCatchesTinyAlternatingReasoning 推理侧同形态也必须命中：
// 叙述循环在两行严格交替时同样没有主导行。
func TestReasoningLoopGuardCatchesTinyAlternatingReasoning(t *testing.T) {
	var text strings.Builder
	for index := 0; index < 400; index++ {
		if index%2 == 0 {
			text.WriteString("好。\n")
		} else {
			text.WriteString("执行。\n")
		}
	}
	guard := &reasoningLoopGuard{}
	observed := guard.add(text.String())
	if observed == nil {
		observed = guard.finish()
	}
	if !IsReasoningLoopError(observed) {
		t.Fatalf("tiny alternating reasoning was not detected: unique=%d err=%v",
			len(guard.counts), observed)
	}
}

// TestTinyCycleDoesNotFireOnLongAlternatingRows 锁定判据边界：行长超过 loopCycleMaxRunes
// 的交替重复（表格、状态行）不能被当成循环。
func TestTinyCycleDoesNotFireOnLongAlternatingRows(t *testing.T) {
	cases := map[string]string{
		"table_border":  "| --- | --- |",
		"table_row":     "| a | b |",
		"status_zh":     "检查数据一致性",
		"status_longer": "等待上游返回结果",
	}
	for name, line := range cases {
		t.Run(name, func(t *testing.T) {
			var text strings.Builder
			for index := 0; index < 400; index++ {
				if index%2 == 0 {
					text.WriteString(line + "\n")
				} else {
					text.WriteString("另一行也要足够长\n")
				}
			}
			guard := &reasoningLoopGuard{}
			observed := guard.add(text.String())
			if observed == nil {
				observed = guard.finish()
			}
			if IsReasoningLoopError(observed) {
				t.Fatalf("long alternating rows were cut off: unique=%d", len(guard.counts))
			}
		})
	}
}
