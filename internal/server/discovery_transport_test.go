// ═══ 更新日志 ═══
// 2026-09-25：覆盖模型发现权限、未知能力、内置工具告知及下行阻塞保护，防止兼容修复静默降级。
package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/upstream"
)

func seedDiscoveryModels(t *testing.T) {
	t.Helper()
	resetModelsCache()
	t.Cleanup(resetModelsCache)
	dynamicModelsCache.Lock()
	dynamicModelsCache.ids = []upstream.ModelInfo{
		{ID: "known/model", Name: "Known model", ContextWindow: 65536, MaxTokens: 4096, SupportsToolCall: true},
		{ID: "unknown-limits"},
	}
	dynamicModelsCache.fetched = time.Now()
	dynamicModelsCache.Unlock()
}

func TestModelRetrievalRespectsBoundKey(t *testing.T) {
	seedDiscoveryModels(t)
	h, key, calls := boundKeyHandler(t, []string{"cn:known/model"})
	h.cfg.GlobalEnabled = false
	for _, tc := range []struct {
		model string
		code  int
	}{
		{"cn:known/model", 200},
		{"cn:unknown-limits", 404},
		{"global:known/model", 404},
		{"known/model", 404},
		{"cn:not-real", 404},
	} {
		t.Run(tc.model, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/v1/models/"+url.PathEscape(tc.model), nil)
			req.Header.Set("Authorization", "Bearer "+key)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != tc.code {
				t.Fatalf("status=%d want %d: %s", rec.Code, tc.code, rec.Body)
			}
			if tc.code == 200 {
				var model map[string]any
				if err := json.Unmarshal(rec.Body.Bytes(), &model); err != nil || model["id"] != tc.model || model["context_length"] != float64(65536) {
					t.Fatalf("retrieval differs from visible model: %s (%v)", rec.Body, err)
				}
			} else if !assertJSONErrorCode(t, rec.Body.String(), "model_not_found") {
				t.Fatalf("hidden/unknown models must use the same error: %s", rec.Body)
			}
		})
	}
	if *calls != 0 {
		t.Fatalf("cached discovery made %d upstream calls", *calls)
	}
}

func TestModelDiscoveryAcceptsAnthropicAPIKey(t *testing.T) {
	seedDiscoveryModels(t)
	h, key, _ := boundKeyHandler(t, []string{"cn:known/model"})
	h.cfg.GlobalEnabled = false
	for _, tc := range []struct {
		name, bearer, xkey string
		status             int
	}{
		{"anthropic", "", key, 200},
		{"invalid", "", "wrong", 401},
		{"missing", "", "", 401},
		{"bearer_has_priority", "Bearer wrong", key, 401},
		{"valid_bearer_has_priority", "Bearer " + key, "wrong", 200},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
			req.Header.Set("Authorization", tc.bearer)
			req.Header.Set("X-API-Key", tc.xkey)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != tc.status {
				t.Fatalf("status=%d want %d: %s", rec.Code, tc.status, rec.Body)
			}
			if rec.Code == 200 && (strings.Contains(rec.Body.String(), "unknown-limits") || !strings.Contains(rec.Body.String(), "cn:known/model")) {
				t.Fatalf("API key header bypassed model permissions: %s", rec.Body)
			}
		})
	}
}

func TestDiscoveryDoesNotInventLimits(t *testing.T) {
	seedDiscoveryModels(t)
	h := NewHandler(Config{Pool: testPoolWith(), Upstream: upstream.New()})
	for _, model := range h.modelList() {
		if model["id"] != "cn:unknown-limits" {
			continue
		}
		for _, key := range []string{"context_length", "max_output_tokens"} {
			if _, exists := model[key]; exists {
				t.Errorf("unknown model must omit %s instead of claiming %v", key, model[key])
			}
		}
	}
}

func TestCapabilityEndpointDescribesUnavailableBuiltins(t *testing.T) {
	h := NewHandler(Config{APIKey: "test-key"})
	for _, key := range []string{"", "test-key"} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/v1/capabilities", nil)
		req.Header.Set("Authorization", "Bearer "+key)
		h.ServeHTTP(rec, req)
		if key == "" {
			if rec.Code != 401 {
				t.Fatalf("capabilities bypassed authentication: %d", rec.Code)
			}
			continue
		}
		var result map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil || rec.Code != 200 {
			t.Fatalf("capability discovery failed: %d %s (%v)", rec.Code, rec.Body, err)
		}
		for _, key := range []string{"native_compaction", "response_storage", "exact_token_counting"} {
			if result[key] != false {
				t.Errorf("unimplemented %s must explicitly be false: %v", key, result[key])
			}
		}
		if !strings.Contains(rec.Body.String(), "web_search") || !strings.Contains(rec.Body.String(), "tool_search") {
			t.Errorf("unsupported default tools are not disclosed: %s", rec.Body)
		}
	}
}

func TestBuiltinFilteringIsDisclosed(t *testing.T) {
	for _, path := range []string{"/v1/responses", "/v1/chat/completions"} {
		for _, stream := range []bool{false, true} {
			t.Run(path+"/"+map[bool]string{false: "json", true: "stream"}[stream], func(t *testing.T) {
				up := newFakeUpstream(t, func(string) (int, string, bool) { return 200, sseOK, true })
				h := NewHandler(Config{Pool: testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}), Upstream: up})
				body := map[string]any{"model": "cn:glm-5.2", "stream": stream, "tools": []any{map[string]any{"type": "web_search"}, map[string]any{"type": "tool_search", "execution": "client"}}}
				if path == "/v1/responses" {
					body["input"] = "hi"
				} else {
					body["messages"] = []any{map[string]any{"role": "user", "content": "hi"}}
				}
				raw, _ := json.Marshal(body)
				rec := httptest.NewRecorder()
				h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, path, strings.NewReader(string(raw))))
				if rec.Code != 200 {
					t.Fatalf("compatible default tools rejected: %d %s", rec.Code, rec.Body)
				}
				if got := rec.Header().Get("X-WB2API-Ignored-Tools"); got != "tool_search, web_search" {
					t.Errorf("silent capability loss: header=%q", got)
				}
				if rec.Header().Get("Warning") == "" {
					t.Error("human-readable compatibility warning is missing")
				}
			})
		}
	}
}

type deadlineObservationWriter struct {
	*httptest.ResponseRecorder
	deadline        time.Time
	missingDeadline bool
	flushErr        error
}

func (w *deadlineObservationWriter) SetWriteDeadline(deadline time.Time) error {
	w.deadline = deadline
	return nil
}
func (w *deadlineObservationWriter) Write(p []byte) (int, error) {
	w.missingDeadline = w.missingDeadline || w.deadline.IsZero()
	return w.ResponseRecorder.Write(p)
}
func (w *deadlineObservationWriter) FlushError() error {
	w.missingDeadline = w.missingDeadline || w.deadline.IsZero()
	return w.flushErr
}

func TestNetworkWriteDeadlineArmedOnlyForOutput(t *testing.T) {
	w := &deadlineObservationWriter{ResponseRecorder: httptest.NewRecorder()}
	NewHandler(Config{Pool: testPoolWith()}).ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if w.missingDeadline {
		t.Fatal("HTTP response wrote without a bounded write deadline")
	}
	if !w.deadline.IsZero() {
		t.Fatal("write deadline leaked into upstream idle time or a reused connection")
	}
}

func TestFlushFailureCancelsUpstreamWork(t *testing.T) {
	for _, path := range []string{"/v1/chat/completions", "/v1/responses"} {
		t.Run(path, func(t *testing.T) {
			var active context.Context
			up := &upstream.Client{HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				active = r.Context()
				return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(sseOK))}, nil
			})}, ChatBaseCN: "https://fake.example"}
			p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
			p.SetMaxInFlight(1)
			h := NewHandler(Config{Pool: p, Upstream: up})
			body := `{"model":"cn:glm-5.2","stream":true,"messages":[{"role":"user","content":"hi"}]}`
			if path == "/v1/responses" {
				body = `{"model":"cn:glm-5.2","stream":true,"input":"hi"}`
			}
			want := errors.New("client flush failed")
			w := &deadlineObservationWriter{ResponseRecorder: httptest.NewRecorder(), flushErr: want}
			h.ServeHTTP(w, httptest.NewRequest(http.MethodPost, path, strings.NewReader(body)))
			if active == nil || !errors.Is(context.Cause(active), want) {
				t.Fatal("client output failure did not cancel upstream context with its cause")
			}
			if !p.Acquire("u1") {
				t.Fatal("client failure retained account lease")
			}
			p.Release("u1")
		})
	}
}

func TestProtocolWritersRetainFlushFailure(t *testing.T) {
	want := errors.New("failed to flush final response")
	for _, protocol := range []string{"chat", "responses"} {
		t.Run(protocol, func(t *testing.T) {
			inner := &deadlineObservationWriter{ResponseRecorder: httptest.NewRecorder(), flushErr: want}
			if protocol == "chat" {
				w := &chatContractWriter{inner: inner}
				w.Flush()
				if !errors.Is(w.CompletionError(), want) {
					t.Fatal("chat ignored flush failure")
				}
			} else {
				w := newResponsesWriter(inner, &responsesRequest{Stream: true})
				w.Flush()
				if !errors.Is(w.CompletionError(), want) {
					t.Fatal("responses ignored flush failure")
				}
			}
		})
	}
}

type pipeResponseWriter struct {
	conn   net.Conn
	header http.Header
}

func (w *pipeResponseWriter) Header() http.Header         { return w.header }
func (w *pipeResponseWriter) WriteHeader(int)             {}
func (w *pipeResponseWriter) Write(p []byte) (int, error) { return w.conn.Write(p) }
func (w *pipeResponseWriter) SetWriteDeadline(deadline time.Time) error {
	return w.conn.SetWriteDeadline(deadline)
}

func TestDownstreamBlockedNetworkWriteTimesOut(t *testing.T) {
	server, client := net.Pipe()
	defer server.Close()
	defer client.Close()
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	w := newBoundedResponseWriter(&pipeResponseWriter{conn: server, header: http.Header{}}, cancel, 25*time.Millisecond)
	start := time.Now()
	_, err := w.Write([]byte("a client that never reads must release this request"))
	var timeout net.Error
	if !errors.As(err, &timeout) || !timeout.Timeout() {
		t.Fatalf("blocked real network write did not time out: %v", err)
	}
	if time.Since(start) > 3*time.Second {
		t.Fatal("write timeout did not bound client wait")
	}
	if !errors.Is(context.Cause(ctx), err) || !errors.Is(w.CompletionError(), err) {
		t.Fatal("timeout did not reach cancellation/completion state")
	}
	if _, repeated := w.Write([]byte("must not retry")); !errors.Is(repeated, err) {
		t.Fatal("failed connection was reused")
	}
}

func TestDownstreamHTTP2LongThinkingKeepsStreamOpen(t *testing.T) {
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(raw http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithCancelCause(r.Context())
		defer cancel(nil)
		w := newBoundedResponseWriter(raw, cancel, 25*time.Millisecond)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: first\n\n")
		_ = w.FlushError()
		time.Sleep(100 * time.Millisecond)
		if ctx.Err() != nil {
			return
		}
		_, _ = io.WriteString(w, "data: second\n\n")
		_ = w.FlushError()
	}))
	srv.EnableHTTP2 = true
	srv.StartTLS()
	defer srv.Close()
	client := srv.Client()
	client.Timeout = 3 * time.Second
	resp, err := client.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.ProtoMajor != 2 {
		t.Fatalf("test requires real HTTP/2, got %s", resp.Proto)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil || string(body) != "data: first\n\ndata: second\n\n" {
		t.Fatalf("idle model time inherited a write deadline: body=%q err=%v", body, err)
	}
}

func TestGlobalDiscoveryDoesNotInventLimits(t *testing.T) {
	resetModelsCache()
	t.Cleanup(resetModelsCache)
	auth.SetGlobalEnabled(true)
	t.Cleanup(func() { auth.SetGlobalEnabled(true) })
	fake := newGlobalModelsHandlerFake(t, 200, `{"code":0,"data":["unknown-limits"]}`)
	h := NewHandler(Config{Pool: testPoolWith(&auth.Auth{UID: "global", AccessToken: "at_gl", Domain: "www.workbuddy.ai", ExpiresAt: 9999999999}), Upstream: fake.up, GlobalEnabled: true})
	list := h.modelList()
	if len(list) != 1 || list[0]["id"] != "global:unknown-limits" {
		t.Fatalf("unexpected global discovery: %v", list)
	}
	if _, exists := list[0]["context_length"]; exists {
		t.Fatal("global narrow list invented a context window")
	}
}

func TestIgnoredBuiltinWarningNeverEchoesArbitraryHeaderText(t *testing.T) {
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	warnIgnoredBuiltinTools(w, r, []any{map[string]any{"type": "web_search_\r\nInjected: bad"}, map[string]any{"type": "web_search_preview"}})
	if got := w.Header().Get("X-WB2API-Ignored-Tools"); got != "web_search" {
		t.Fatalf("untrusted tool type reached response header: %q", got)
	}
	if strings.Contains(w.Header().Get("Warning"), "Injected") {
		t.Fatal("untrusted tool type reached warning")
	}
}
