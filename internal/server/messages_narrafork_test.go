// ═══ 更新日志 ═══
// 2026-09-25：复现 NF.chat 的 keep-all/xhigh 请求以及早到 prompt、晚到缓存拆分，守住完整历史和工具校验边界。
package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

const nfMessagesHistory = `{"model":"cn:fixture","max_tokens":64000,"stream":true,"thinking":{"type":"enabled","budget_tokens":10000},"system":[{"type":"text","text":"fixture instructions"}],"messages":[{"role":"user","content":"original user message"},{"role":"assistant","content":[{"type":"thinking","thinking":"original complete reasoning","signature":""},{"type":"tool_use","id":"history-tool","name":"read","input":{"path":"fixture.txt"}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"history-tool","content":"original complete tool output"},{"type":"text","text":"continue fixture"}]}],"tools":[{"name":"read","input_schema":{"type":"object","properties":{"path":{"type":"string"}}}}]}`

func nfMessagesWithOption(base, field, value string) string {
	return strings.TrimSuffix(base, "}") + `,"` + field + `":` + value + `}`
}

func TestNFMessagesKeepAllPreservesHistory(t *testing.T) {
	keep := `{"edits":[{"type":"clear_thinking_20251015","keep":"all"}]}`
	body := nfMessagesWithOption(nfMessagesHistory, "context_management", keep)
	baseline, _, _, err := messagesToChat([]byte(nfMessagesHistory))
	if err != nil {
		t.Fatal(err)
	}
	converted, _, _, err := messagesToChat([]byte(body))
	if err != nil {
		t.Fatalf("NF keep-all request rejected: %v", err)
	}
	if string(converted) != string(baseline) {
		t.Fatalf("keep-all changed the converted request/history:\n%s\n%s", baseline, converted)
	}
	h, outbound, calls, _ := messagesFixture(t, sseOK)
	r := httptest.NewRequest(http.MethodPost, "/v1/messages?beta=true", strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer fixture-key")
	r.Header.Set("Anthropic-Beta", "claude-code-20250219,context-management-2025-06-27")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusOK || *calls != 1 {
		t.Fatalf("NF official chat did not reach the shared handler: %d %d %s", w.Code, *calls, w.Body.String())
	}
	encoded, _ := json.Marshal(*outbound)
	for _, text := range []string{"original user message", "original complete reasoning", "original complete tool output", "continue fixture"} {
		if !strings.Contains(string(encoded), text) {
			t.Fatalf("keep-all lost historical content %q: %s", text, encoded)
		}
	}
	if _, exists := (*outbound)["context_management"]; exists {
		t.Fatal("a no-op control leaked an unsupported server-state request upstream")
	}
}

func TestNFMessagesRejectsActiveContextManagement(t *testing.T) {
	for name, value := range map[string]string{
		"empty":                `{}`,
		"clear all thinking":   `{"edits":[{"type":"clear_thinking_20251015","keep":0}]}`,
		"keep one":             `{"edits":[{"type":"clear_thinking_20251015","keep":1}]}`,
		"other edit":           `{"edits":[{"type":"clear_tool_uses_20250919","keep":"all"}]}`,
		"extra action":         `{"edits":[{"type":"clear_thinking_20251015","keep":"all"},{"type":"clear_tool_uses_20250919"}]}`,
		"extra edit field":     `{"edits":[{"type":"clear_thinking_20251015","keep":"all","clear_at_least":1}]}`,
		"extra context field":  `{"edits":[{"type":"clear_thinking_20251015","keep":"all"}],"truncation":"auto"}`,
		"wrong shape":          `{"edits":{"type":"clear_thinking_20251015","keep":"all"}}`,
		"keep is not a string": `{"edits":[{"type":"clear_thinking_20251015","keep":["all"]}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			h, _, calls, _ := messagesFixture(t, sseOK)
			r := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(nfMessagesWithOption(nfMessagesHistory, "context_management", value)))
			r.Header.Set("X-API-Key", "fixture-key")
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != http.StatusBadRequest || *calls != 0 || !strings.Contains(w.Body.String(), `"type":"invalid_request_error"`) {
				t.Fatalf("active/unknown context edit was accepted: %d %d %s", w.Code, *calls, w.Body.String())
			}
		})
	}
	for _, field := range []string{"container", "mcp_servers"} {
		if _, _, _, err := messagesToChat([]byte(nfMessagesWithOption(nfMessagesHistory, field, `{}`))); err == nil {
			t.Fatalf("unrelated server-state capability %s was accepted", field)
		}
	}
}

func TestNFMessagesXhighPreservesActualBudget(t *testing.T) {
	h, outbound, calls, _ := messagesFixture(t, sseOK)
	body := nfMessagesWithOption(nfMessagesHistory, "output_config", `{"effort":"xhigh"}`)
	r := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	r.Header.Set("X-API-Key", "fixture-key")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusOK || *calls != 1 {
		t.Fatalf("NF xhigh request rejected: %d %d %s", w.Code, *calls, w.Body.String())
	}
	if (*outbound)["reasoning_effort"] != "xhigh" || (*outbound)["max_tokens"] != float64(64000) {
		t.Fatalf("effort or output budget was invented: %#v", *outbound)
	}
	thinking, _ := (*outbound)["thinking"].(map[string]any)
	if thinking["budget_tokens"] != float64(10000) {
		t.Fatalf("NF's actual thinking budget changed: %#v", thinking)
	}
}

func nfMessagesSSEEvents(t *testing.T, body string) []map[string]any {
	t.Helper()
	var events []map[string]any
	for _, line := range strings.Split(body, "\n") {
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var event map[string]any
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &event); err != nil {
			t.Fatal(err)
		}
		events = append(events, event)
	}
	return events
}

func TestNFMessagesDefersUnknownInitialCacheSplit(t *testing.T) {
	reply := "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hello\"}}],\"usage\":{\"prompt_tokens\":100,\"completion_tokens\":0}}\n\n" +
		"data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_cache_hit_tokens\":100,\"completion_tokens\":7}}\n\ndata: [DONE]\n\n"
	h, _, _, _ := messagesFixture(t, reply)
	r := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"cn:fixture","max_tokens":64,"stream":true,"messages":[{"role":"user","content":"fixture"}]}`))
	r.Header.Set("X-API-Key", "fixture-key")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if path := os.Getenv("WB2API_NF_CACHE_SPLIT_EVIDENCE"); path != "" {
		if err := os.WriteFile(path, w.Body.Bytes(), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if w.Code != http.StatusOK {
		t.Fatalf("usage fixture failed: %d %s", w.Code, w.Body.String())
	}
	events := nfMessagesSSEEvents(t, w.Body.String())
	initial := events[0]["message"].(map[string]any)["usage"].(map[string]any)
	if initial["input_tokens"] != float64(0) {
		t.Errorf("unknown split treated gross prompt tokens as measured noncached input: %v", initial)
	}
	var final map[string]any
	for _, event := range events {
		if event["type"] == "message_delta" {
			final, _ = event["usage"].(map[string]any)
		}
	}
	if final["input_tokens"] != float64(0) || final["cache_read_input_tokens"] != float64(100) || final["output_tokens"] != float64(7) {
		t.Fatalf("actual terminal usage was modified to work around a client: %v", final)
	}
}

func TestNFMessagesKeepsReportedInitialCacheSplit(t *testing.T) {
	w := httptest.NewRecorder()
	m := newMessagesWriter(w)
	m.usage = map[string]any{"prompt_tokens": 100, "completion_tokens": 0, "prompt_cache_hit_tokens": 60}
	m.begin()
	usage := nfMessagesSSEEvents(t, w.Body.String())[0]["message"].(map[string]any)["usage"].(map[string]any)
	if usage["input_tokens"] != float64(40) || usage["cache_read_input_tokens"] != float64(60) {
		t.Fatalf("a reported split was distorted: %v", usage)
	}
}

func TestNFMessagesFinishResponseIsIdempotent(t *testing.T) {
	for _, complete := range []bool{false, true} {
		w := httptest.NewRecorder()
		m := newMessagesWriter(w)
		m.stream = true
		m.Header().Set("Content-Type", "text/event-stream")
		_, _ = m.Write([]byte("data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hello\"}}]}\n\n"))
		if complete {
			_, _ = m.Write([]byte("data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"))
		}
		first := m.FinishResponse()
		before := w.Body.String()
		second := m.FinishResponse()
		if !errors.Is(second, first) || w.Body.String() != before || m.Unwrap() != w {
			t.Fatalf("repeated finish changed the response: first=%v second=%v", first, second)
		}
		if complete && (first != nil || strings.Count(before, "event: message_stop") != 1) {
			t.Fatalf("valid response finalized incorrectly: %v %s", first, before)
		}
		if !complete && (first == nil || strings.Contains(before, "event: message_stop")) {
			t.Fatalf("truncated response became successful: %v %s", first, before)
		}
	}
}
