// ═══ 更新日志 ═══
// 2026-09-25：锁定上下文超限终态：完整保留系统、最早与最新历史及工具结果，禁止隐式删轮重发。
package upstream

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync/atomic"
	"testing"

	"workbuddy2api/internal/auth"
)

func contextHistoryBody(t *testing.T, turns int) []byte {
	t.Helper()
	messages := []map[string]any{{"role": "system", "content": "Keep the complete project history."}}
	for turn := 0; turn < turns; turn++ {
		callID := fmt.Sprintf("history-call-%d", turn)
		messages = append(messages,
			map[string]any{"role": "user", "content": fmt.Sprintf("Original requirement from turn %d", turn)},
			map[string]any{
				"role": "assistant", "content": "",
				"tool_calls": []any{map[string]any{
					"id": callID, "type": "function",
					"function": map[string]any{"name": "read_project", "arguments": fmt.Sprintf(`{"turn":%d}`, turn)},
				}},
			},
			map[string]any{"role": "tool", "tool_call_id": callID, "content": fmt.Sprintf("Project evidence from turn %d", turn)},
		)
	}
	body, err := json.Marshal(map[string]any{"model": "deepseek-v4.1-flash", "messages": messages})
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func contextHistoryMessages(t *testing.T, body []byte) []map[string]any {
	t.Helper()
	var request struct {
		Messages []map[string]any `json:"messages"`
	}
	if err := json.Unmarshal(body, &request); err != nil {
		t.Fatalf("invalid request body: %v", err)
	}
	return request.Messages
}

func TestContextTooLongPreservesHistory(t *testing.T) {
	globalEnabled := auth.GlobalEnabled()
	auth.SetGlobalEnabled(true)
	t.Cleanup(func() { auth.SetGlobalEnabled(globalEnabled) })
	cases := []struct {
		name string
		body string
	}{
		{"literal_one_token", `{"code":11115,"msg":"prompt is too long: 1048577 tokens > 1048576 maximum","extError":{"code":"context_length_exceeded"}}`},
		{"escaped_one_token", `{"code":11115,"msg":"prompt is too long: 1048577 tokens \u003e 1048576 maximum","extError":{"code":"context_length_exceeded"}}`},
		{"literal_small", `{"code":11115,"msg":"prompt is too long: 1048691 tokens > 1048576 maximum"}`},
		{"escaped_small", `{"code":11115,"msg":"prompt is too long: 1048691 tokens \u003e 1048576 maximum"}`},
		{"literal_large", `{"code":11115,"msg":"prompt is too long: 2015759 tokens > 1048576 maximum"}`},
		{"escaped_large", `{"code":11115,"msg":"prompt is too long: 2015759 tokens \u003e 1048576 maximum"}`},
		{"no_counts", `{"code":11115,"msg":"prompt is too long"}`},
		{"code_only", `{"code":11115,"msg":"request exceeds context capacity"}`},
	}
	for _, realm := range []string{"cn", "global"} {
		t.Run(realm, func(t *testing.T) {
			for _, tc := range cases {
				t.Run(tc.name, func(t *testing.T) {
					for _, turns := range []int{1, 2, 3, 40} {
						t.Run(fmt.Sprintf("turns_%d", turns), func(t *testing.T) {
							var attempts, retries atomic.Int32
							requests := make(chan []byte, 16)
							srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
								defer r.Body.Close()
								body, err := io.ReadAll(r.Body)
								if err != nil {
									t.Errorf("read upstream request: %v", err)
								}
								attempts.Add(1)
								requests <- body
								w.WriteHeader(http.StatusBadRequest)
								_, _ = io.WriteString(w, tc.body)
							}))
							defer srv.Close()
							client := &Client{
								ChatBaseCN: srv.URL, ChatBaseGlobal: srv.URL, GlobalEnabled: true,
								HTTP: srv.Client(), ChatHTTP: srv.Client(),
							}
							account := &auth.Auth{UID: "history-fixture"}
							if realm == "global" {
								account.Domain = "www.workbuddy.ai"
							}
							ctx := WithChatRetryObserver(context.Background(), func([]byte) { retries.Add(1) })
							body := contextHistoryBody(t, turns)
							original := bytes.Clone(body)
							wantMessages := contextHistoryMessages(t, body)
							rc, status, raw, err := client.ChatStreamContext(ctx, account, body, "", ChatMeta{})
							if rc != nil {
								_ = rc.Close()
								t.Error("context error became a successful response stream")
							}
							if err != nil || status != http.StatusBadRequest || string(raw) != tc.body {
								t.Errorf("context error changed: status=%d body=%q err=%v", status, raw, err)
							}
							if got := attempts.Load(); got != 1 {
								t.Errorf("context overflow made %d upstream requests; want exactly one", got)
							}
							if got := retries.Load(); got != 0 {
								t.Errorf("context error was discarded for %d internal retries", got)
							}
							if !bytes.Equal(body, original) {
								t.Error("caller history was mutated")
							}
							for len(requests) > 0 {
								gotMessages := contextHistoryMessages(t, <-requests)
								if !reflect.DeepEqual(gotMessages, wantMessages) {
									t.Errorf("upstream history changed: got %d messages, want all %d original messages", len(gotMessages), len(wantMessages))
								}
							}
						})
					}
				})
			}
		})
	}
}
