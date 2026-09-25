// ═══ 更新日志 ═══
// 2026-09-25：复现粘性账号跨域后继续使用，以及注册地修复重发遗漏首轮真实或未知用量。
package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/session"
	"workbuddy2api/internal/upstream"
)

func TestOpsStickyRealmMismatchDoesNotUseStaleAccount(t *testing.T) {
	auth.SetGlobalEnabled(true)
	t.Cleanup(func() { auth.SetGlobalEnabled(true) })
	h, _ := postreleaseUsageHandler(t, sseOK)
	h.cfg.GlobalEnabled = true
	h.cfg.Pool = testPoolWith(
		&auth.Auth{UID: "preferred", AccessToken: "ops-original-cn-token", Domain: "www.codebuddy.cn", ExpiresAt: 9999999999},
		&auth.Auth{UID: "fallback", AccessToken: "ops-cn-token", Domain: "www.codebuddy.cn", ExpiresAt: 9999999999},
	)
	bindings := newBindStore()
	switched := false
	h.cfg.Session = session.New(session.Config{
		TTL: time.Hour, Store: bindings, Available: h.cfg.Pool.AvailableUIDs,
		AvailableForModel: func(model string) []string {
			realm, bare := resolveModel(model)
			available := h.cfg.Pool.AvailableUIDsForModelRealm(bare, realm)
			if !switched {
				// Model availability was valid when read. Simulate an auth-file
				// reload changing that account's realm before the actual pick.
				h.cfg.Pool.Add(&auth.Auth{UID: "preferred", AccessToken: "ops-foreign-token", Domain: "www.workbuddy.ai", ExpiresAt: 9999999999})
				switched = true
			}
			return available
		},
	})
	const key = "ops-sticky-realm"
	h.cfg.Session.Bind(key, "preferred")
	var used []string
	h.cfg.Upstream = newFakeUpstream(t, func(authorization string) (int, string, bool) {
		used = append(used, authorization)
		return http.StatusOK, sseOK, true
	})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"cn:deepseek-v4.1-flash","metadata":{"conversation_id":"ops-sticky-realm"},"messages":[{"role":"user","content":"fixture request"}]}`)))
	if rec.Code != http.StatusOK || !switched {
		t.Fatalf("fixture did not reach the successful changed-realm path: status=%d switched=%v body=%s", rec.Code, switched, rec.Body.String())
	}
	if len(used) != 1 || used[0] != "Bearer ops-cn-token" {
		t.Fatalf("wrong-realm sticky account survived unbinding: synthetic_authorization=%v", used)
	}
	bindings.mu.Lock()
	bound := bindings.binds[key]
	bindings.mu.Unlock()
	if bound != "fallback" {
		t.Fatalf("session did not converge to the correct realm: bound=%q", bound)
	}
}

func TestOpsRegionRepairRetryPreservesFirstAttemptAccounting(t *testing.T) {
	auth.SetGlobalEnabled(true)
	t.Cleanup(func() { auth.SetGlobalEnabled(true) })
	for _, stream := range []bool{false, true} {
		for _, firstReported := range []bool{false, true} {
			name := "json/first-unknown"
			if stream {
				name = "stream/first-unknown"
			}
			if firstReported {
				name = strings.Replace(name, "unknown", "reported", 1)
			}
			t.Run(name, func(t *testing.T) {
				success := postreleaseUsageContent + postreleaseFinish("stop") + postreleaseUsageOnly + "data: [DONE]\n\n"
				h, ledger := postreleaseUsageHandler(t, success)
				h.cfg.GlobalEnabled = true
				h.cfg.Pool = testPoolWith(&auth.Auth{UID: "region-fixture", AccessToken: "ops-region-token", Domain: "www.workbuddy.ai", ExpiresAt: 9999999999})
				denial := map[string]any{"error": map[string]any{"data": map[string]any{"code": 14017, "msg": "The trial version is not yet activated."}}}
				if firstReported {
					denial["usage"] = map[string]any{"prompt_tokens": 7, "completion_tokens": 3, "total_tokens": 10, "prompt_cache_hit_tokens": 2, "credit": 0.5}
				}
				first, _ := json.Marshal(denial)
				calls, repairs := 0, 0
				h.cfg.Upstream = newFakeUpstream(t, func(authorization string) (int, string, bool) {
					calls++
					if authorization != "Bearer ops-region-token" {
						t.Error("repair retried a different synthetic account")
					}
					if calls == 1 {
						return http.StatusTooManyRequests, string(first), false
					}
					return http.StatusOK, success, true
				})
				h.cfg.RegionRepair = func(a *auth.Auth) (upstream.UserArea, error) {
					repairs++
					if a.UID != "region-fixture" {
						t.Errorf("unexpected synthetic repair target %q", a.UID)
					}
					return upstream.UserArea{IOS2: "SG", IOS3: "SGP", EnName: "Singapore", Code: "65"}, nil
				}
				body, _ := json.Marshal(map[string]any{"model": "global:deepseek-v4.1-flash", "stream": stream, "messages": []any{map[string]any{"role": "user", "content": "fixture request"}}})
				rec := httptest.NewRecorder()
				h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(string(body))))
				if rec.Code != http.StatusOK || calls != 2 || repairs != 1 {
					t.Fatalf("fixture did not execute one repair and same-account retry: status=%d calls=%d repairs=%d body=%s", rec.Code, calls, repairs, rec.Body.String())
				}
				got := ledger.Snapshot().Totals
				if got.Requests != 1 || got.FailedRequests != 0 {
					t.Fatalf("successful repair must remain one successful client request: %+v", got)
				}
				if firstReported {
					if got.PromptTokens != 5007 || got.CompletionTokens != 123 || got.CachedTokens != 4098 || got.TotalTokens != 5130 || got.Credit != 1.75 || got.UnreportedRequests != 0 {
						t.Fatalf("repair retry discarded first reported consumption: %+v", got)
					}
				} else if got.PromptTokens != 5000 || got.CompletionTokens != 120 || got.CachedTokens != 4096 || got.TotalTokens != 5120 || got.Credit != 1.25 || got.UnreportedRequests != 1 {
					t.Fatalf("repair retry treated the first unreported attempt as known/free: %+v", got)
				}
			})
		}
	}
}
