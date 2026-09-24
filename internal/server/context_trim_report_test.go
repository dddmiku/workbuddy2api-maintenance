// ═══ 更新日志 ═══
// 2026-09-24：新增 handler 级回归：上游 11115 被自动裁剪后，客户端收到的
//
//	input_tokens 必须是上游给出的**原始**体积，而不是裁剪后的值。
//	这是 Codex 压缩机制的唯一触发依据（修复前客户端只看到 74k）。
package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"workbuddy2api/internal/auth"
)

// TestTrimmedRequestReportsOriginalPromptTokens 自动裁剪过的那次请求，
// 回传给客户端的 input_tokens 必须是上游报的原始体积。
func TestTrimmedRequestReportsOriginalPromptTokens(t *testing.T) {
	const original = 1048691
	calls := 0
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		calls++
		if calls == 1 {
			return http.StatusBadRequest,
				`{"code":11115,"msg":"prompt is too long: 1048691 tokens > 1048576 maximum",` +
					`"extError":{"code":"context_length_exceeded"}}`, false
		}
		// 裁剪后的重发：上游按裁剪后的请求计费，这里给一个明显更小的 prompt_tokens。
		return http.StatusOK, sseUsage(1200, 30), true
	})
	p := testPoolWith(&auth.Auth{UID: "g1", Domain: "www.workbuddy.ai",
		AccessToken: "at-1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up})

	// 造一个多轮请求，让裁剪有轮可丢。
	body := multiTurnChatBody(t, 12)
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	usage := lastUsageFromSSE(t, rec.Body.String())
	if usage == nil {
		t.Fatalf("no usage frame: %s", rec.Body.String())
	}
	got := intOfAnyServer(usage["prompt_tokens"])
	if got != original {
		t.Fatalf("client saw prompt_tokens=%d; the trimmed request must report the original %d "+
			"so the client can trigger its own compaction", got, original)
	}
}

// TestLargeOvershootReachesClientAsError 大幅超限时不再裁剪：客户端必须收到 11115，
// 走自己的压缩流程。
func TestLargeOvershootReachesClientAsError(t *testing.T) {
	calls := 0
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		calls++
		return http.StatusBadRequest,
			`{"code":11115,"msg":"prompt is too long: 2015759 tokens > 1048576 maximum",` +
				`"extError":{"code":"context_length_exceeded"}}`, false
	})
	p := testPoolWith(&auth.Auth{UID: "g1", Domain: "www.workbuddy.ai",
		AccessToken: "at-1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(multiTurnChatBody(t, 12))))

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d want 400 so the client learns it must compact", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "prompt is too long") {
		t.Fatalf("the upstream error must reach the client: %s", rec.Body.String())
	}
	if calls != 1 {
		t.Fatalf("large overshoot must not be trimmed: calls=%d", calls)
	}
}

// multiTurnChatBody 造 n 轮 user/assistant 的 chat 请求体。
func multiTurnChatBody(t *testing.T, turns int) string {
	t.Helper()
	messages := []any{}
	for i := 0; i < turns; i++ {
		messages = append(messages,
			map[string]any{"role": "user", "content": "question"},
			map[string]any{"role": "assistant", "content": "answer"})
	}
	raw, err := json.Marshal(map[string]any{
		"model": "global:deepseek-v4.1-flash", "stream": true, "messages": messages,
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(raw)
}

// sseUsage 造一段只带 usage 的 SSE 流。
func sseUsage(prompt, completion int) string {
	return "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\n" +
		"data: {\"usage\":{\"prompt_tokens\":" + itoa(prompt) +
		",\"completion_tokens\":" + itoa(completion) +
		",\"total_tokens\":" + itoa(prompt+completion) + "}}\n\n" +
		"data: [DONE]\n\n"
}

func itoa(value int) string { return strconv.Itoa(value) }

// lastUsageFromSSE 取 SSE 里最后一个 usage 对象。
func lastUsageFromSSE(t *testing.T, body string) map[string]any {
	t.Helper()
	var found map[string]any
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" || payload == "[DONE]" {
			continue
		}
		var frame map[string]any
		if json.Unmarshal([]byte(payload), &frame) != nil {
			continue
		}
		if usage, ok := frame["usage"].(map[string]any); ok {
			found = usage
		}
	}
	return found
}

func intOfAnyServer(value any) int {
	switch v := value.(type) {
	case float64:
		return int(v)
	case int:
		return v
	case int64:
		return int(v)
	}
	return -1
}
