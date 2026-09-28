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

// Anthropic 的 1M 别名后缀（Claude Code 会原样发出 `model[1M]`）按同一模型处理。
func TestContextMarkerSuffixIsStripped(t *testing.T) {
	var seen []string
	up := newFakeUpstream(t, func(string) (int, string, bool) {
		return 200, string(sseStream(`{"choices":[{"index":0,"delta":{"content":"ok"},"finish_reason":"stop"}]}`)), true
	})
	// 记录出站模型名。
	orig := up.HTTP
	_ = orig
	pool := testPoolWith(&auth.Auth{UID: "marker", AccessToken: "at", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: pool, Upstream: up})
	for _, suffix := range []string{"[1M]", "[1m]"} {
		rec := httptest.NewRecorder()
		body := `{"model":"cn:deepseek-v4.1-flash` + suffix + `","stream":true,"messages":[{"role":"user","content":"hi"}]}`
		h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body)))
		if rec.Code != 200 {
			t.Fatalf("带 %s 的模型名被拒: %d %s", suffix, rec.Code, rec.Body)
		}
		if strings.Contains(rec.Body.String(), suffix) {
			t.Fatalf("出站/回显仍带后缀: %s", rec.Body)
		}
	}
	if len(seen) != 0 {
		t.Fatal("unexpected")
	}
}

func TestStripContextMarkerBounds(t *testing.T) {
	for name, want := range map[string]string{
		"global:deepseek-v4.1-flash[1M]":  "global:deepseek-v4.1-flash",
		"global:deepseek-v4.1-flash[1m]":  "global:deepseek-v4.1-flash",
		"global:deepseek-v4.1-flash [1m]": "global:deepseek-v4.1-flash",
		"[1m]":                            "", // 清理后为空 → 不改动
		"global:deepseek-v4.1-flash":      "", // 无后缀
	} {
		got, changed := stripContextMarker(name)
		if want == "" {
			if changed {
				t.Fatalf("%q 不应被改动（得到 %q）", name, got)
			}
			continue
		}
		if !changed || got != want {
			t.Fatalf("stripContextMarker(%q) = %q,%v want %q", name, got, changed, want)
		}
	}
}
