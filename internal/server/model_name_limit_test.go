// ═══ 更新日志 ═══
// 2026-09-26：超长模型名在选号前拒绝，不调用上游、不记账。
package server

import (
	"net/http/httptest"
	"strings"
	"testing"

	"workbuddy2api/internal/auth"
)

func TestOversizedModelNameRejectedBeforeUpstream(t *testing.T) {
	calls := 0
	up := newFakeUpstream(t, func(string) (int, string, bool) {
		calls++
		return 200, string(sseStream(`{"choices":[{"index":0,"delta":{"content":"x"},"finish_reason":"stop"}]}`)), true
	})
	p := testPoolWith(&auth.Auth{UID: "model-limit", AccessToken: "at", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up})
	for _, path := range []string{"/v1/chat/completions", "/v1/responses"} {
		rec := httptest.NewRecorder()
		model := "cn:" + strings.Repeat("m", 300)
		var body string
		if path == "/v1/chat/completions" {
			body = `{"model":"` + model + `","stream":false,"messages":[{"role":"user","content":"hi"}]}`
		} else {
			body = `{"model":"` + model + `","stream":false,"input":"hi"}`
		}
		h.ServeHTTP(rec, httptest.NewRequest("POST", path, strings.NewReader(body)))
		if rec.Code != 400 || !strings.Contains(rec.Body.String(), "model name exceeds") {
			t.Fatalf("%s: status=%d body=%s", path, rec.Code, rec.Body)
		}
	}
	if calls != 0 {
		t.Fatalf("oversized model reached upstream %d times", calls)
	}
}
