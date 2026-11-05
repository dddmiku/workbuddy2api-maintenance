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
	"strings"
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

// historyFingerprint 把 messages 拍平成「角色 + 内容」序列，用于断言历史未被丢弃。
//
// 为什么不用条数比较：2026-11-06 起出站 system 被 FlattenSystemIntoUser 合并
// （客户端 system 内容并入第一条 user，system 只留官方固定句），消息条数会变，
// 但每一条用户/助手消息的内容与顺序都必须原样保留。这里比对的是「内容清单」
// 而不是「条数」，守的仍然「历史不许丢」这条语义。
func historyFingerprint(msgs []map[string]any) []string {
	out := make([]string, 0, len(msgs))
	for _, m := range msgs {
		role, _ := m["role"].(string)
		switch c := m["content"].(type) {
		case string:
			out = append(out, role+"|"+c)
		case []any:
			for _, raw := range c {
				if part, ok := raw.(map[string]any); ok {
					if text, ok := part["text"].(string); ok {
						out = append(out, role+"|"+text)
					}
				}
			}
		}
	}
	return out
}

// contentPreserved 报告 want 里每条消息的**正文**是否都还出现在 got 里。
//
// 2026-11-06：出站 system 被 FlattenSystemIntoUser 合并进第一条 user
// （见 internal/upstream/system_flatten.go），条数与 role 序列必然变化。
// 守的语义不变——**历史正文一律不许丢**——所以这里检查「每条原文内容都是
// got 全部文本的子串」，而不是 DeepEqual 整条消息。
func contentPreserved(want, got []map[string]any) bool {
	var all strings.Builder
	for _, m := range got {
		switch c := m["content"].(type) {
		case string:
			all.WriteString(c)
			all.WriteString("\n")
		case []any:
			for _, raw := range c {
				if part, ok := raw.(map[string]any); ok {
					if text, ok := part["text"].(string); ok {
						all.WriteString(text)
						all.WriteString("\n")
					}
				}
			}
		}
		// 工具结果/调用也要算进「历史正文」，否则丢一条 tool 结果查不出来。
		if calls, ok := m["tool_calls"].([]any); ok {
			for _, rc := range calls {
				if call, ok := rc.(map[string]any); ok {
					all.WriteString(fmt.Sprintf("%v", call))
				}
			}
		}
		if id, ok := m["tool_call_id"].(string); ok {
			all.WriteString(id)
		}
	}
	haystack := all.String()
	for _, m := range want {
		switch c := m["content"].(type) {
		case string:
			if c == "" {
				continue // assistant 的 content 常为空串，无可比对正文
			}
			if !strings.Contains(haystack, c) {
				return false
			}
		case []any:
			for _, raw := range c {
				if part, ok := raw.(map[string]any); ok {
					if text, ok := part["text"].(string); ok && text != "" {
						if !strings.Contains(haystack, text) {
							return false
						}
					}
				}
			}
		}
	}
	return true
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
								body := readReqBody(r)
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
								// 2026-11-06：system 会被 FlattenSystemIntoUser 合并进 user
								// （见 internal/upstream/system_flatten.go），条数与 role 序列
								// 必然变化。守的语义不变——**历史一律不许丢**，所以比对
								// 内容清单而非 DeepEqual 整条消息。
								if !contentPreserved(wantMessages, gotMessages) {
									t.Errorf("upstream history lost content: got %d messages, want all %d original messages preserved",
										len(gotMessages), len(wantMessages))
								}
							}
						})
					}
				})
			}
		})
	}
}
