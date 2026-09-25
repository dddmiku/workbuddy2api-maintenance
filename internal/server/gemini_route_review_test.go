// ═══ 更新日志 ═══
// 2026-09-25：复核 Gemini alt 运输选择和模型详情权限，拒绝冲突查询而不静默改变请求格式。
package server

import (
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestGeminiAltTransportOptionsAreExplicit(t *testing.T) {
	for _, tc := range []struct {
		action, query string
		status        int
		contentType   string
	}{
		{"streamGenerateContent", "alt=sse", 200, "text/event-stream"},
		{"streamGenerateContent", "alt=json", 400, "application/json"},
		{"streamGenerateContent", "alt=sse&alt=json", 400, "application/json"},
		{"streamGenerateContent", "alt=sse%ZZ", 400, "application/json"},
		{"generateContent", "alt=json", 200, "application/json"},
		{"generateContent", "alt=sse", 400, "application/json"},
		{"generateContent", "alt=media", 400, "application/json"},
	} {
		t.Run(tc.action+"/"+tc.query, func(t *testing.T) {
			h, _, calls, _ := messagesFixture(t, sseOK)
			r := httptest.NewRequest("POST", "/v1beta/models/cn:fixture:"+tc.action+"?"+tc.query, strings.NewReader(`{"contents":[{"parts":[{"text":"x"}]}]}`))
			r.Header.Set("X-Goog-Api-Key", "fixture-key")
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, r)
			if rec.Code != tc.status || !strings.HasPrefix(rec.Header().Get("Content-Type"), tc.contentType) || (tc.status >= 400 && *calls != 0) {
				t.Fatalf("alt semantics changed or reached upstream: status=%d calls=%d headers=%v body=%s", rec.Code, *calls, rec.Header(), rec.Body.String())
			}
		})
	}
}

func TestGeminiModelDetailDoesNotBypassCallingKeyScope(t *testing.T) {
	seedDiscoveryModels(t)
	h, key, calls := boundKeyHandler(t, []string{"cn:known/model"})
	h.cfg.GlobalEnabled = false
	for _, tc := range []struct {
		model, supplied, bearer string
		status                  int
	}{
		{"cn:known/model", key, "", 200},
		{"cn:unknown-limits", key, "", 404},
		{"cn:missing", key, "", 404},
		{"cn:known/model", "wrong", "", 401},
		{"cn:known/model", "", "", 401},
		{"cn:known/model", key, "Bearer wrong", 401},
	} {
		for _, base := range []string{"/v1beta", "/v1"} {
			r := httptest.NewRequest("GET", base+"/models/"+url.PathEscape(tc.model), nil)
			if tc.supplied != "" || base == "/v1" {
				r.Header.Set("X-Goog-Api-Key", tc.supplied)
			}
			if tc.bearer != "" {
				r.Header.Set("Authorization", tc.bearer)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, r)
			if rec.Code != tc.status {
				t.Fatalf("model detail auth differs: %s %s got=%d want=%d body=%s", base, tc.model, rec.Code, tc.status, rec.Body.String())
			}
			if tc.status >= 400 && (strings.Contains(rec.Body.String(), "inputTokenLimit") || strings.Contains(rec.Body.String(), "displayName")) {
				t.Fatalf("hidden model metadata leaked: %s", rec.Body.String())
			}
		}
	}
	if *calls != 0 {
		t.Fatalf("cached detail checks triggered %d upstream requests", *calls)
	}
}
