// ═══ 更新日志 ═══
// 2026-09-25：Gemini 复用 Chat 原始账本，缓存和推理不重复加入，最终写失败仍记录实际消费。
package server

import (
	"net/http/httptest"
	"testing"
)

func TestGeminiAccountingUsesOriginalMeasurementsOnSuccessAndWriteFailure(t *testing.T) {
	for _, action := range []string{"generateContent", "streamGenerateContent"} {
		for _, failure := range []bool{false, true} {
			h, ledger := postreleaseUsageHandler(t, postreleaseUsageContent+postreleaseFinish("stop")+postreleaseUsageOnly+"data: [DONE]\n\n")
			match := "not-a-real-output-marker"
			if failure {
				match = "finishReason"
			}
			w := &usageFinalWriteFailure{ResponseRecorder: httptest.NewRecorder(), match: match}
			geminiTestServe(h, w, action, `{"contents":[{"parts":[{"text":"accounting"}]}]}`)
			got := ledger.Snapshot().Totals
			if got.Requests != 1 || got.PromptTokens != 5000 || got.CompletionTokens != 120 || got.CachedTokens != 4096 || got.TotalTokens != 5120 || got.Credit != 1.25 || got.UnreportedRequests != 0 {
				t.Fatalf("Gemini changed the original ledger: action=%s failure=%v totals=%+v", action, failure, got)
			}
			if failure != w.failed || (failure && got.FailedRequests != 1) || (!failure && got.FailedRequests != 0) {
				t.Fatalf("Gemini lost terminal write failure: action=%s requested=%v observed=%v totals=%+v", action, failure, w.failed, got)
			}
		}
	}
}
