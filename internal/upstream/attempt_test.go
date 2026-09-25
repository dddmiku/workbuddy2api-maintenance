// ═══ 更新日志 ═══
// 2026-09-25：显式使用测试上下文，遵循 Context 调用契约并通过静态检查。
// 2026-09-25：按真实 HTTP 调用核对尝试事件及内部重试顺序，防止消费明细漏算或重复计数。
package upstream

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
)

func TestChatAttemptObserverTracksInternalRetries(t *testing.T) {
	previousGlobal := auth.GlobalEnabled()
	auth.SetGlobalEnabled(true)
	t.Cleanup(func() { auth.SetGlobalEnabled(previousGlobal) })
	const final = "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"
	cases := []struct {
		name, realm, input, rejected string
		status                       int
		paths                        []string
	}{
		{
			name: "global-path", realm: "global", input: "audit",
			rejected: `{"code":404,"msg":"not found","usage":{"prompt_tokens":7,"completion_tokens":2,"credit":0.5}}`,
			status:   http.StatusNotFound, paths: []string{chatCompletionsPath, globalChatConsolePath},
		},
		{
			name: "channel", realm: "cn", input: "audit",
			rejected: `{"code":11128,"msg":"Illegal API invocation from an unapproved channel","usage":{"prompt_tokens":7,"completion_tokens":2,"credit":0.5}}`,
			status:   http.StatusBadRequest, paths: []string{chatCompletionsPath, chatCompletionsPath},
		},
		{
			name: "waf", realm: "cn", input: "<script>fixture</script>",
			rejected: `<html><head><title>WAF Block Page</title></head><body>request blocked by WAF</body></html>`,
			status:   http.StatusForbidden, paths: []string{chatCompletionsPath, chatCompletionsPath},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			paths := make(chan string, 8)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				paths <- r.URL.Path
				if len(paths) == 1 {
					w.WriteHeader(tc.status)
					_, _ = io.WriteString(w, tc.rejected)
					return
				}
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, final)
			}))
			defer server.Close()
			client := &Client{HTTP: server.Client(), ChatHTTP: server.Client(), ChatBaseCN: server.URL, ChatBaseGlobal: server.URL, GlobalEnabled: true}
			var events []ChatAttemptEvent
			var order []string
			var discarded [][]byte
			ctx := WithChatAttemptObserver(context.Background(), func(event ChatAttemptEvent) {
				events = append(events, event)
				order = append(order, event.Stage)
			})
			ctx = WithChatRetryObserver(ctx, func(body []byte) {
				discarded = append(discarded, append([]byte(nil), body...))
				order = append(order, "retry")
			})
			body := `{"model":"deepseek-v4.1-flash","messages":[{"role":"system","content":"Codex CLI is an open source project led by OpenAI"},{"role":"user","content":"` + tc.input + `"}]}`
			account := &auth.Auth{UID: "attempt-fixture", AccessToken: "synthetic-only", Domain: "www.codebuddy.cn"}
			if tc.realm == "global" {
				account.Domain = "www.workbuddy.ai"
			}
			started := time.Now()
			rc, status, raw, err := client.ChatStreamContext(ctx, account, []byte(body), "", ChatMeta{})
			if err != nil || status != http.StatusOK || rc == nil {
				t.Fatalf("retry did not complete: status=%d error=%v body=%s", status, err, raw)
			}
			defer rc.Close()
			got, err := io.ReadAll(rc)
			if err != nil || string(got) != final {
				t.Fatalf("observer changed final response: error=%v body=%q", err, got)
			}
			if want := []string{"start", "headers", "retry", "start", "headers"}; !reflect.DeepEqual(order, want) {
				t.Fatalf("attempt/retry order=%v want=%v", order, want)
			}
			if len(discarded) != 1 || string(discarded[0]) != tc.rejected {
				t.Fatalf("discarded response was lost, repeated or modified: %q", discarded)
			}
			if len(paths) != len(tc.paths) {
				t.Fatalf("actual HTTP calls=%d want=%d", len(paths), len(tc.paths))
			}
			for _, want := range tc.paths {
				if got := <-paths; got != want {
					t.Errorf("HTTP path=%s want=%s", got, want)
				}
			}
			if events[0].Status != 0 || events[1].Status != tc.status || events[2].Status != 0 || events[3].Status != 200 {
				t.Fatalf("HTTP status events=%+v", events)
			}
			previous := started
			for _, event := range events {
				if event.At.Before(previous) || event.At.After(time.Now()) {
					t.Errorf("attempt timestamp is outside actual event order: %+v", event)
				}
				previous = event.At
			}
		})
	}
}

func TestChatAttemptObserverDistinguishesLocalAndTransportFailure(t *testing.T) {
	for _, local := range []bool{false, true} {
		name := "transport"
		if local {
			name = "local-request-construction"
		}
		t.Run(name, func(t *testing.T) {
			calls := 0
			client := testClient(func(*http.Request) (*http.Response, error) {
				calls++
				return nil, errors.New("synthetic transport failure")
			})
			if local {
				client.ChatBaseCN = "://invalid"
			}
			var events []ChatAttemptEvent
			ctx := WithChatAttemptObserver(context.Background(), func(event ChatAttemptEvent) { events = append(events, event) })
			rc, _, _, err := client.ChatStreamContext(ctx, &auth.Auth{UID: "attempt-fixture"}, []byte(`{"model":"glm-5.2","messages":[{"role":"user","content":"audit"}]}`), "", ChatMeta{})
			if rc != nil {
				_ = rc.Close()
			}
			if err == nil {
				t.Fatal("failure fixture unexpectedly succeeded")
			}
			if local {
				if calls != 0 || len(events) != 0 {
					t.Fatalf("local rejection counted as an HTTP attempt: calls=%d events=%+v", calls, events)
				}
			} else if calls != 1 || len(events) != 1 || events[0].Stage != "start" || events[0].Status != 0 {
				t.Fatalf("transport failure invented response headers or lost start: calls=%d events=%+v", calls, events)
			}
		})
	}
}

func TestChatAttemptObserverDoesNotReportFinalRejectionAsRetry(t *testing.T) {
	const rejected = `{"code":11101,"msg":"Unmarshal chat params failed"}`
	client := testClient(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 400, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(rejected))}, nil
	})
	var events []ChatAttemptEvent
	retries := 0
	ctx := WithChatAttemptObserver(context.Background(), func(event ChatAttemptEvent) { events = append(events, event) })
	ctx = WithChatRetryObserver(ctx, func([]byte) { retries++ })
	rc, status, raw, err := client.ChatStreamContext(ctx, &auth.Auth{UID: "attempt-fixture"}, []byte(`{"model":"glm-5.2","messages":[{"role":"user","content":"audit"}]}`), "", ChatMeta{})
	if rc != nil {
		_ = rc.Close()
	}
	if err != nil || rc != nil || status != 400 || string(raw) != rejected || retries != 0 {
		t.Fatalf("final rejection changed or counted twice: status=%d retries=%d body=%s error=%v", status, retries, raw, err)
	}
	if len(events) != 2 || events[0].Stage != "start" || events[1].Stage != "headers" || events[1].Status != 400 {
		t.Fatalf("rejection attempt events=%+v", events)
	}
}
