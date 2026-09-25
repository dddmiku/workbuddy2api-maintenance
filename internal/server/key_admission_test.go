// ═══ 更新日志 ═══
// 2026-09-25：通过四协议真实入口验证共享限额、排队撤销和权限重查，保护私有明细并核对已知零与未知。
package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"workbuddy2api/internal/apikeys"
	"workbuddy2api/internal/keylimit"
	"workbuddy2api/internal/requestlog"
)

type admissionProtocol struct{ name, path, body, header string }

var admissionProtocols = []admissionProtocol{
	{"chat", "/v1/chat/completions", `{"model":"cn:deepseek-v4.1-flash","messages":[{"role":"user","content":"private fixture prompt"}]}`, "Authorization"},
	{"responses", "/v1/responses", `{"model":"cn:deepseek-v4.1-flash","input":"private fixture prompt"}`, "Authorization"},
	{"messages", "/v1/messages", `{"model":"cn:deepseek-v4.1-flash","max_tokens":256,"messages":[{"role":"user","content":"private fixture prompt"}]}`, "X-API-Key"},
	{"gemini", "/v1beta/models/cn:deepseek-v4.1-flash:generateContent", `{"contents":[{"role":"user","parts":[{"text":"private fixture prompt"}]}]}`, "X-Goog-API-Key"},
}

func admissionFixture(t *testing.T, policy keylimit.Policy) (*Handler, apikeys.Info, string, *atomic.Int32) {
	t.Helper()
	success := postreleaseUsageContent + postreleaseFinish("stop") + postreleaseUsageOnly + "data: [DONE]\n\n"
	h, _ := postreleaseUsageHandler(t, success)
	keys, err := apikeys.Open(filepath.Join(t.TempDir(), "keys.json"), "")
	if err != nil {
		t.Fatal(err)
	}
	info, secret, err := keys.Create("fixture", "", nil, apikeys.Options{Limits: &policy, LimitsSet: true})
	if err != nil {
		t.Fatal(err)
	}
	history, err := requestlog.Open(filepath.Join(t.TempDir(), "requests.jsonl"), requestlog.Options{})
	if err != nil {
		t.Fatal(err)
	}
	h.cfg.APIKeys, h.cfg.Requests = keys, history
	t.Cleanup(func() { _ = history.Close(); _ = h.cfg.KeyLimits.Close() })
	calls := &atomic.Int32{}
	h.cfg.Upstream = newFakeUpstream(t, func(string) (int, string, bool) { calls.Add(1); return 200, success, true })
	return h, info, secret, calls
}

func admissionRequest(h *Handler, p admissionProtocol, secret string, ctx context.Context) *httptest.ResponseRecorder {
	request := httptest.NewRequest("POST", p.path, strings.NewReader(p.body)).WithContext(ctx)
	if p.header == "Authorization" {
		request.Header.Set(p.header, "Bearer "+secret)
	} else {
		request.Header.Set(p.header, secret)
	}
	response := httptest.NewRecorder()
	h.ServeHTTP(response, request)
	return response
}

func TestOpsKeyLimitsSharedByAllProtocols(t *testing.T) {
	h, info, secret, calls := admissionFixture(t, keylimit.Policy{RequestsPerMinute: 4, MaxConcurrent: 2})
	for round := 0; round < 2; round++ {
		for _, protocol := range admissionProtocols {
			response := admissionRequest(h, protocol, secret, context.Background())
			expected := 200
			if round > 0 {
				expected = 429
			}
			if response.Code != expected {
				t.Fatalf("%s round %d: status=%d body=%s", protocol.name, round, response.Code, response.Body.String())
			}
			if round == 0 {
				continue
			}
			if response.Header().Get("Retry-After") == "" || response.Header().Get("X-RateLimit-Remaining") != "0" {
				t.Fatalf("%s limit response missing retry/rate metadata: retry=%q remaining=%q", protocol.name, response.Header().Get("Retry-After"), response.Header().Get("X-RateLimit-Remaining"))
			}
			var body map[string]any
			if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			failure, ok := body["error"].(map[string]any)
			if !ok {
				t.Fatalf("%s has no protocol error: %s", protocol.name, response.Body.String())
			}
			if protocol.name == "gemini" && failure["status"] != "RESOURCE_EXHAUSTED" {
				t.Fatal("Gemini error shape lost")
			}
			if protocol.name == "messages" && (body["type"] != "error" || failure["type"] != "rate_limit_error") {
				t.Fatal("Messages error shape lost")
			}
		}
	}
	if calls.Load() != 4 {
		t.Fatalf("rejected request reached upstream: calls=%d", calls.Load())
	}
	state, err := h.cfg.KeyLimits.Snapshot(context.Background(), map[string]keylimit.Policy{info.ID: *info.Limits})
	if err != nil || state[info.ID].Requests != 4 || state[info.ID].Active != 0 || state[info.ID].Queued != 0 {
		t.Fatalf("unexpected settled limit state: %+v err=%v", state, err)
	}
	page, err := h.cfg.Requests.List(requestlog.Query{KeyID: info.ID})
	if err != nil || page.Total != 8 {
		t.Fatalf("missing accepted/rejected request details: %+v %v", page, err)
	}
	for _, item := range page.Items {
		if item.Status == requestlog.StatusRejected && (item.UpstreamStarted || item.AttemptCount != 0) {
			t.Fatal("local rejection has invented upstream attempt")
		}
	}
	raw, _ := json.Marshal(page)
	if strings.Contains(string(raw), secret) || strings.Contains(string(raw), "private fixture prompt") {
		t.Fatal("request detail leaked key or body")
	}
	_, other, err := h.cfg.APIKeys.Create("other", "", nil, apikeys.Options{Limits: &keylimit.Policy{RequestsPerMinute: 1}, LimitsSet: true})
	if err != nil {
		t.Fatal(err)
	}
	if response := admissionRequest(h, admissionProtocols[0], other, context.Background()); response.Code != 200 {
		t.Fatal("one key exhausted another key's limit")
	}
}

func TestOpsQueuedKeyRechecksAndReleases(t *testing.T) {
	for _, kind := range []string{"cancel", "disabled", "expired", "model_permission", "cleared"} {
		t.Run(kind, func(t *testing.T) {
			policy := keylimit.Policy{MaxConcurrent: 1, QueueTimeoutSeconds: 2}
			h, info, secret, calls := admissionFixture(t, policy)
			held, err := h.cfg.KeyLimits.Acquire(context.Background(), info.ID, func() (keylimit.Policy, error) { return policy, nil })
			if err != nil {
				t.Fatal(err)
			}
			defer held.Release()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan *httptest.ResponseRecorder, 1)
			go func() { done <- admissionRequest(h, admissionProtocols[0], secret, ctx) }()
			deadline := time.Now().Add(2 * time.Second)
			for {
				state, err := h.cfg.KeyLimits.Snapshot(context.Background(), map[string]keylimit.Policy{info.ID: policy})
				if err != nil {
					t.Fatal(err)
				}
				if state[info.ID].Queued == 1 {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("fixture did not enter key queue")
				}
				time.Sleep(5 * time.Millisecond)
			}
			expected, upstream := 401, int32(0)
			switch kind {
			case "cancel":
				cancel()
				expected = 200
			case "disabled":
				enabled := false
				_, err = h.cfg.APIKeys.Update(info.ID, nil, nil, &enabled, nil)
			case "expired":
				expiry := time.Now().Add(100 * time.Millisecond)
				_, err = h.cfg.APIKeys.Update(info.ID, nil, nil, nil, nil, apikeys.Options{ExpiresAt: &expiry, ExpiresAtSet: true})
			case "model_permission":
				models := []string{"cn:hy3"}
				_, err = h.cfg.APIKeys.Update(info.ID, nil, nil, nil, &models)
				expected = 403
				_ = held.Release()
			case "cleared":
				_, err = h.cfg.APIKeys.Update(info.ID, nil, nil, nil, nil, apikeys.Options{LimitsSet: true})
				expected, upstream = 200, 1
			}
			if err != nil {
				t.Fatal(err)
			}
			select {
			case response := <-done:
				if response.Code != expected {
					t.Fatalf("unexpected queued result: status=%d body=%s", response.Code, response.Body.String())
				}
			case <-time.After(3 * time.Second):
				t.Fatal("queued request did not settle")
			}
			if calls.Load() != upstream {
				t.Fatalf("queued authorization did not protect upstream: calls=%d", calls.Load())
			}
			_ = held.Release()
			state, err := h.cfg.KeyLimits.Snapshot(context.Background(), map[string]keylimit.Policy{info.ID: policy})
			if err != nil || state[info.ID].Active != 0 || state[info.ID].Queued != 0 {
				t.Fatalf("queue/lease leak: %+v %v", state, err)
			}
		})
	}
}

func TestOpsRequestDetailsStayOnPrivateTransport(t *testing.T) {
	h, _, secret, _ := admissionFixture(t, keylimit.Policy{})
	for _, path := range []string{"/key-limits", "/requests", "/requests/req_fixture"} {
		request := httptest.NewRequest(http.MethodGet, path, nil)
		request.Header.Set("Authorization", "Bearer "+secret)
		response := httptest.NewRecorder()
		h.ServeHTTP(response, request)
		if response.Code != 401 {
			t.Fatalf("ordinary key reached %s: %d", path, response.Code)
		}
	}
	for _, path := range []string{"/requests?key_id=a&key_id=b", "/requests?unexpected=x", "/requests?limit=-1", "/requests/req_fixture?x=y"} {
		response := httptest.NewRecorder()
		h.InternalHandler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
		if response.Code != 400 {
			t.Fatalf("invalid internal query %s: %d", path, response.Code)
		}
	}
	response := httptest.NewRecorder()
	h.InternalHandler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/requests", nil))
	if response.Code != 200 {
		t.Fatal("internal history unavailable")
	}
}
