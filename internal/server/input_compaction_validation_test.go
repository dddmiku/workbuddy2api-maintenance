// ═══ 更新日志 ═══
// 2026-09-25：原生压缩条目和压缩配置必须明确拒绝，避免静默丢上下文；本地文本摘要继续保留。
package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"workbuddy2api/internal/auth"
)

func TestResponsesNativeCompactionRejectedBeforeUpstream(t *testing.T) {
	cases := []struct {
		name              string
		input             string
		contextManagement string
		field             string
	}{
		{"encrypted_history", `[{"type":"compaction","id":"cmp_1","encrypted_content":"opaque-context-canary"},{"role":"user","content":"continue"}]`, "", "input[0]"},
		{"encrypted_after_message", `[{"role":"user","content":"original"},{"type":"compaction","encrypted_content":"opaque-context-canary"},{"role":"user","content":"continue"}]`, "", "input[1]"},
		{"encrypted_at_end", `[{"role":"user","content":"continue"},{"type":"compaction","encrypted_content":"opaque-context-canary"}]`, "", "input[1]"},
		{"compaction_with_readable_content", `[{"type":"compaction","encrypted_content":"opaque-context-canary","content":"partial summary"},{"role":"user","content":"continue"}]`, "", "input[0]"},
		{"compaction_without_ciphertext", `[{"type":"compaction"},{"role":"user","content":"continue"}]`, "", "input[0]"},
		{"server_compaction", `"continue"`, `[{"type":"compaction","compact_threshold":700000}]`, "context_management"},
		{"server_compaction_defaults", `"continue"`, `[{"type":"compaction"}]`, "context_management"},
		{"management_object", `"continue"`, `{"type":"compaction"}`, "context_management"},
		{"management_string", `"continue"`, `"compaction"`, "context_management"},
		{"management_empty_object", `"continue"`, `{}`, "context_management"},
	}
	for _, tc := range cases {
		for _, stream := range []bool{false, true} {
			mode := map[bool]string{false: "json", true: "stream"}[stream]
			t.Run(tc.name+"/"+mode, func(t *testing.T) {
				var calls atomic.Int32
				up := newFakeUpstream(t, func(string) (int, string, bool) {
					calls.Add(1)
					return http.StatusOK, sseOK, true
				})
				p := testPoolWith(&auth.Auth{UID: "compaction-fixture", AccessToken: "fixture", ExpiresAt: 9999999999})
				h := NewHandler(Config{Pool: p, Upstream: up})
				request := map[string]any{"model": "cn:deepseek-v4.1-flash", "input": json.RawMessage(tc.input), "stream": stream}
				if tc.contextManagement != "" {
					request["context_management"] = json.RawMessage(tc.contextManagement)
				}
				body, err := json.Marshal(request)
				if err != nil {
					t.Fatal(err)
				}
				recorder := httptest.NewRecorder()
				h.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(string(body))))
				if calls.Load() != 0 {
					t.Errorf("unsupported compaction reached upstream after dropping context: calls=%d", calls.Load())
				}
				if recorder.Code != http.StatusBadRequest || !strings.HasPrefix(recorder.Header().Get("Content-Type"), "application/json") {
					t.Fatalf("unsupported compaction must be a JSON parameter error: status=%d type=%s", recorder.Code, recorder.Header().Get("Content-Type"))
				}
				var envelope struct {
					Error struct {
						Code    string `json:"code"`
						Message string `json:"message"`
					} `json:"error"`
				}
				if err := json.Unmarshal(recorder.Body.Bytes(), &envelope); err != nil {
					t.Fatal(err)
				}
				if envelope.Error.Code != "invalid_request" || !strings.Contains(envelope.Error.Message, tc.field) {
					t.Fatalf("parameter error lost the unsupported field: %s", recorder.Body.String())
				}
				if strings.Contains(recorder.Body.String(), "opaque-context-canary") {
					t.Fatal("parameter error must not echo encrypted history")
				}
				state, _ := p.Status("compaction-fixture")
				if state.ErrTotal != 0 || !state.Until.IsZero() || state.InFlight != 0 {
					t.Fatalf("parameter error changed account health or acquired a lease: %+v", state)
				}
			})
		}
	}
}

func TestResponsesClientCompactionTextPreserved(t *testing.T) {
	const summary = `Earlier context: retain project-canary. The literal {"type":"compaction"} is documentation.`
	for _, management := range []string{"", "null", "[]"} {
		for _, stringInput := range []bool{false, true} {
			name := management + "/" + map[bool]string{false: "messages", true: "string"}[stringInput]
			t.Run(name, func(t *testing.T) {
				var input any = []any{
					map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": summary}}},
					map[string]any{"type": "message", "role": "user", "content": "continue-canary"},
				}
				if stringInput {
					input = summary
				}
				request := map[string]any{"model": "cn:deepseek-v4.1-flash", "instructions": "instruction-canary", "input": input}
				if management != "" {
					request["context_management"] = json.RawMessage(management)
				}
				body, err := json.Marshal(request)
				if err != nil {
					t.Fatal(err)
				}
				chatBody, _, err := responsesToChat(body)
				if err != nil {
					t.Fatalf("client-generated text summary was rejected: %v", err)
				}
				messages := decodeChat(t, chatBody)["messages"].([]any)
				wantLen := 3
				if stringInput {
					wantLen = 2
				}
				if len(messages) != wantLen || messages[0].(map[string]any)["content"] != "instruction-canary" ||
					messages[1].(map[string]any)["content"] != summary || messages[1].(map[string]any)["role"] != "user" {
					t.Fatalf("client-generated summary was changed or dropped: %s", chatBody)
				}
				if !stringInput && messages[2].(map[string]any)["content"] != "continue-canary" {
					t.Fatalf("continuation after text compaction was lost: %s", chatBody)
				}
			})
		}
	}
}
