// ═══ 更新日志 ═══
// 2026-09-23：新增「上游 11115 → 网关自动裁剪最旧历史并重发」的端到端回归：
//
//	第一次返回 context_too_long，第二次必须收到更短的请求体并成功。
package upstream

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"workbuddy2api/internal/auth"
)

// TestContextTooLongTriggersTrimmedRetry 上游首答 11115 时，网关要丢掉最旧轮次重发。
func TestContextTooLongTriggersTrimmedRetry(t *testing.T) {
	var bodies [][]byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, readErr := io.ReadAll(r.Body)
		if readErr != nil {
			t.Errorf("read request body: %v", readErr)
		}
		bodies = append(bodies, raw)
		if len(bodies) == 1 {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"code":11115,"msg":"prompt is too long: 1048691 tokens > 1048576 maximum",` +
				`"extError":{"code":"context_length_exceeded"}}`))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\n" +
			"data: {\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":1,\"total_tokens\":11}}\n\n" +
			"data: [DONE]\n\n"))
	}))
	defer srv.Close()

	client := &Client{ChatBaseCN: srv.URL, HTTP: srv.Client(), ChatHTTP: srv.Client()}
	body := buildTurns(t, 8)
	rc, status, raw, err := client.ChatStreamContext(
		context.Background(), &auth.Auth{UID: "account-trim"}, body, "", ChatMeta{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if status != http.StatusOK {
		t.Fatalf("status=%d body=%s", status, raw)
	}
	if rc != nil {
		_ = rc.Close()
	}
	if len(bodies) != 2 {
		t.Fatalf("expected exactly one retry, got %d upstream attempts", len(bodies))
	}
	if len(bodies[1]) >= len(bodies[0]) {
		t.Fatalf("retry body must be smaller: %d -> %d", len(bodies[0]), len(bodies[1]))
	}
	// 重发体必须仍是完整 JSON 且保留了前导 system 消息。
	var obj struct {
		Messages []map[string]any `json:"messages"`
	}
	if err := json.Unmarshal(bodies[1], &obj); err != nil {
		t.Fatalf("retry body is not valid JSON: %v", err)
	}
	if len(obj.Messages) == 0 {
		t.Fatalf("retry body lost every message")
	}
	if role, _ := obj.Messages[0]["role"].(string); role != "system" {
		t.Fatalf("retry body dropped the system preamble: first role=%q", role)
	}
}

// TestContextTooLongGivesUpAfterAllLevels 裁到最后一档仍超限时，要如实回上游错误。
func TestContextTooLongGivesUpAfterAllLevels(t *testing.T) {
	attempts := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"code":11115,"msg":"prompt is too long: 1048691 tokens > 1048576 maximum"}`))
	}))
	defer srv.Close()

	client := &Client{ChatBaseCN: srv.URL, HTTP: srv.Client(), ChatHTTP: srv.Client()}
	body := buildTurns(t, 40)
	rc, status, raw, err := client.ChatStreamContext(
		context.Background(), &auth.Auth{UID: "account-trim"}, body, "", ChatMeta{})
	if rc != nil {
		_ = rc.Close()
	}
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if status != http.StatusBadRequest {
		t.Fatalf("status=%d want 400", status)
	}
	if !strings.Contains(string(raw), "prompt is too long") {
		t.Fatalf("upstream error must be passed through: %s", raw)
	}
	// 首答 + 每个裁剪档位各重发一次，之后收手。
	if attempts != ContextTrimLevels()+1 {
		t.Fatalf("attempts=%d want %d", attempts, ContextTrimLevels()+1)
	}
}
