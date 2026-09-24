// ═══ 更新日志 ═══
// 2026-09-25：锁定 messages 参数、工具增量、上下文错误与缓存口径的实际回归，避免协议转换伪装成功。
// 2026-09-25：官方 SDK 要求内容块顺序完成，工具回归改为全量校验后交付，并区分首帧临时计量与末帧扩展标记。
package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/upstream"
)

func TestMessagesReviewRejectsMalformedOptionsBeforeUpstream(t *testing.T) {
	base := `{"model":"cn:fixture","max_tokens":2048,"messages":[{"role":"user","content":"hi"}]}`
	cases := map[string]string{
		"negative zero maximum":          strings.Replace(base, `"max_tokens":2048`, `"max_tokens":-0`, 1),
		"temperature range":              strings.TrimSuffix(base, "}") + `,"temperature":-1}`,
		"top p range":                    strings.TrimSuffix(base, "}") + `,"top_p":2}`,
		"metadata type":                  strings.TrimSuffix(base, "}") + `,"metadata":["user"]}`,
		"thinking budget missing":        strings.TrimSuffix(base, "}") + `,"thinking":{"type":"enabled"}}`,
		"thinking budget exceeds output": strings.TrimSuffix(base, "}") + `,"thinking":{"type":"enabled","budget_tokens":2048}}`,
		"invalid effort":                 strings.TrimSuffix(base, "}") + `,"output_config":{"effort":"arbitrary"}}`,
		"missing output schema":          strings.TrimSuffix(base, "}") + `,"output_config":{"format":{"type":"json_schema"}}}`,
		"required absent tools":          strings.TrimSuffix(base, "}") + `,"tool_choice":{"type":"any"}}`,
		"undeclared forced tool":         strings.TrimSuffix(base, "}") + `,"tools":[{"name":"read","input_schema":{"type":"object"}}],"tool_choice":{"type":"tool","name":"delete"}}`,
		"empty content":                  `{"model":"cn:fixture","max_tokens":10,"messages":[{"role":"user","content":[]}]}`,
		"nonstring signature":            `{"model":"cn:fixture","max_tokens":10,"messages":[{"role":"user","content":"hi"},{"role":"assistant","content":[{"type":"thinking","thinking":"reason","signature":{}}]}]}`,
		"opaque signature only":          `{"model":"cn:fixture","max_tokens":10,"messages":[{"role":"user","content":"hi"},{"role":"assistant","content":[{"type":"thinking","thinking":"","signature":"opaque"},{"type":"text","text":"result"}]}]}`,
		"results after text":             `{"model":"cn:fixture","max_tokens":10,"messages":[{"role":"user","content":"read"},{"role":"assistant","content":[{"type":"tool_use","id":"one","name":"read","input":{}}]},{"role":"user","content":[{"type":"text","text":"after"},{"type":"tool_result","tool_use_id":"one","content":"data"}]}]}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			h, _, calls, _ := messagesFixture(t, sseOK)
			r := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
			r.Header.Set("X-API-Key", "fixture-key")
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != http.StatusBadRequest || *calls != 0 || !strings.Contains(w.Body.String(), `"type":"invalid_request_error"`) {
				t.Fatalf("malformed option reached upstream or wrong error: status=%d calls=%d body=%s", w.Code, *calls, w.Body.String())
			}
		})
	}
}

func TestMessagesReviewMalformedCompletionNeverReportsSuccess(t *testing.T) {
	for _, stream := range []string{
		"data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"one\"}},{\"index\":1,\"delta\":{\"content\":\"two\"}}]}\n\ndata: [DONE]\n\n",
		"data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"partial\"}}]}\n\ndata: [DONE]\n\n",
		"data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\ndata: [DONE]\n\n",
		"data: {\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"one\",\"function\":{\"name\":\"read\",\"arguments\":\"{}\"}},{\"index\":1,\"id\":\"one\",\"function\":{\"name\":\"read\",\"arguments\":\"{}\"}}]},\"finish_reason\":\"tool_calls\"}]}\n\ndata: [DONE]\n\n",
	} {
		w := httptest.NewRecorder()
		m := newMessagesWriter(w)
		m.stream = true
		m.Header().Set("Content-Type", "text/event-stream")
		_, _ = m.Write([]byte(stream))
		m.finish()
		if m.CompletionError() == nil || strings.Contains(w.Body.String(), "event: message_stop") || !strings.Contains(w.Body.String(), `"type":"error"`) {
			t.Fatalf("malformed completion became a success: %s", w.Body.String())
		}
	}
}

func TestMessagesReviewToolFinishWithStopStillRequestsExecution(t *testing.T) {
	chunk := `data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"read","arguments":"{}"}}]},"finish_reason":"stop"}],"usage":{"prompt_tokens":2,"completion_tokens":1}}` + "\n\ndata: [DONE]\n\n"
	for _, stream := range []bool{false, true} {
		t.Run(map[bool]string{false: "json", true: "stream"}[stream], func(t *testing.T) {
			h, _, _, _ := messagesFixture(t, chunk)
			body, _ := json.Marshal(map[string]any{"model": "cn:fixture", "max_tokens": 10, "stream": stream, "messages": []any{map[string]any{"role": "user", "content": "read"}}, "tools": []any{map[string]any{"name": "read", "input_schema": map[string]any{"type": "object"}}}})
			r := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body))
			r.Header.Set("X-API-Key", "fixture-key")
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"stop_reason":"tool_use"`) {
				t.Fatalf("valid tool cannot continue the agent loop: status=%d body=%s", w.Code, w.Body.String())
			}
		})
	}
}

func TestMessagesReviewContextFailureRetainsClassification(t *testing.T) {
	for _, prefix := range []string{"", "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"partial\"}}]}\n\n"} {
		for _, problem := range []string{
			`{"code":"context_length_exceeded","message":"request is too long"}`,
			`{"code":11115,"msg":"request is too long"}`,
		} {
			h, _, _, _ := messagesFixture(t, prefix+"data: {\"error\":"+problem+"}\n\ndata: [DONE]\n\n")
			r := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"cn:fixture","max_tokens":10,"stream":true,"messages":[{"role":"user","content":"hi"}]}`))
			r.Header.Set("X-API-Key", "fixture-key")
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if !strings.Contains(w.Body.String(), `"type":"invalid_request_error"`) || !strings.Contains(w.Body.String(), `"code":"context_length_exceeded"`) || strings.Contains(w.Body.String(), "event: message_stop") {
				t.Fatalf("context failure lost its classification: status=%d body=%s", w.Code, w.Body.String())
			}
			if prefix == "" && w.Code != http.StatusBadRequest {
				t.Fatalf("pre-stream context error must retain HTTP 400: %d", w.Code)
			}
		}
	}
}

func TestMessagesReviewToolsWaitForTerminalValidation(t *testing.T) {
	w := httptest.NewRecorder()
	m := newMessagesWriter(w)
	m.stream = true
	m.Header().Set("Content-Type", "text/event-stream")
	// A header may arrive after arguments. Neither an invented id nor an empty
	// name may be exposed before the complete identity is available.
	frames := []string{
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"x\":"}}]}}]}`,
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"one","function":{"name":"read"}}]}}]}`,
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"1}"}},{"index":1,"id":"two","function":{"name":"read","arguments":"{}"}}]}}]}`,
	}
	for i, frame := range frames {
		if _, err := m.Write([]byte("data: " + frame + "\n\n")); err != nil {
			t.Fatal(err)
		}
		if i == 0 && strings.Contains(w.Body.String(), `"type":"tool_use"`) {
			t.Fatal("incomplete tool identity became executable")
		}
	}
	if strings.Contains(w.Body.String(), `"type":"input_json_delta"`) || strings.Contains(w.Body.String(), `"type":"tool_use"`) {
		t.Fatalf("tools were delivered before the whole response validated: %s", w.Body.String())
	}
	if strings.Contains(w.Body.String(), "event: content_block_stop") || strings.Contains(w.Body.String(), "event: message_stop") {
		t.Fatal("tools became complete before the upstream terminal validation")
	}
	_, err := m.Write([]byte("data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\ndata: [DONE]\n\n"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(w.Body.String(), "event: message_stop") != 1 || strings.Count(w.Body.String(), "event: content_block_stop") != 2 || strings.Count(w.Body.String(), `"type":"tool_use"`) != 2 {
		t.Fatalf("invalid parallel tool lifecycle: %s", w.Body.String())
	}
	message := messagesReviewAssemble(t, w.Body.String())
	content := message["content"].([]any)
	if content[0].(map[string]any)["id"] != "one" || content[1].(map[string]any)["id"] != "two" {
		t.Fatalf("tool order changed: %v", content)
	}
}

func TestMessagesReviewIncompleteUsageOnlyAtTerminal(t *testing.T) {
	w := httptest.NewRecorder()
	m := newMessagesWriter(w)
	m.stream = true
	m.Header().Set("Content-Type", "text/event-stream")
	_, _ = m.Write([]byte("data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hello\"}}]}\n\n"))
	if strings.Contains(w.Body.String(), "gateway_usage_incomplete") {
		t.Fatalf("provisional extension would remain in SDK final usage: %s", w.Body.String())
	}
	_, err := m.Write([]byte("data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"))
	if err != nil || strings.Count(w.Body.String(), `"gateway_usage_incomplete":true`) != 1 {
		t.Fatalf("unknown usage lost its raw terminal marker: %v %s", err, w.Body.String())
	}
}

func TestMessagesReviewBufferedToolProgressKeepsStreamAlive(t *testing.T) {
	w := httptest.NewRecorder()
	m := newMessagesWriter(w)
	m.stream = true
	m.Header().Set("Content-Type", "text/event-stream")
	_, _ = m.Write([]byte("data: {\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"one\",\"function\":{\"name\":\"read\",\"arguments\":\"{\"}}]}}]}\n\n"))
	// Move only the last-write timestamp; no wall-clock wait or fake upstream
	// heartbeat hides the case where tools alone keep making progress.
	m.lastEvent = time.Now().Add(-11 * time.Second)
	_, _ = m.Write([]byte("data: {\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"\\\"x\\\":\"}}]}}]}\n\n"))
	if strings.Count(w.Body.String(), "event: ping") != 1 || strings.Contains(w.Body.String(), `"type":"tool_use"`) {
		t.Fatalf("active tool generation lost liveness or bypassed validation: %s", w.Body.String())
	}
	_, _ = m.Write([]byte("data: {\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"1}\"}}]}}]}\n\n"))
	if strings.Count(w.Body.String(), "event: ping") != 1 {
		t.Fatal("each small tool fragment produced an unnecessary heartbeat")
	}
}

func TestMessagesReviewHeartbeatAndPartialFrames(t *testing.T) {
	w := httptest.NewRecorder()
	m := newMessagesWriter(w)
	m.stream = true
	m.Header().Set("Content-Type", "text/event-stream")
	for _, data := range []string{": keep", "alive\r", "\n\r", "\n"} {
		_, _ = m.Write([]byte(data))
	}
	if !strings.Contains(w.Body.String(), "event: ping") {
		t.Fatalf("upstream heartbeat was swallowed: %s", w.Body.String())
	}
	m.finish()
	if strings.Contains(w.Body.String(), "event: message_stop") || !strings.Contains(w.Body.String(), "event: error") {
		t.Fatalf("heartbeat-only EOF became a success: %s", w.Body.String())
	}
}

func TestMessagesReviewCacheWriteUsageIsNotCountedTwice(t *testing.T) {
	usage := map[string]any{"prompt_tokens": 100, "completion_tokens": 5, "prompt_tokens_details": map[string]any{"cached_tokens": 30, "cache_write_tokens": 20, "cache_creation_tokens": 20}}
	actual := anthropicUsage(usage)
	if actual["input_tokens"] != 50 || actual["cache_read_input_tokens"] != 30 || actual["cache_creation_input_tokens"] != 20 {
		t.Fatalf("cache write usage was lost or counted twice: %#v", actual)
	}
	if usage["prompt_tokens"] != 100 {
		t.Fatal("wire conversion modified billing usage")
	}
}

func messagesReviewAssemble(t *testing.T, stream string) map[string]any {
	t.Helper()
	var message map[string]any
	var content []any
	open := map[int]bool{}
	arguments := map[int]string{}
	starts, stops := 0, 0
	for _, frame := range strings.Split(stream, "\n\n") {
		for _, line := range strings.Split(frame, "\n") {
			if !strings.HasPrefix(line, "data: ") {
				continue
			}
			var event map[string]any
			if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &event); err != nil {
				t.Fatal(err)
			}
			index, _ := event["index"].(float64)
			i := int(index)
			switch event["type"] {
			case "message_start":
				message, _ = event["message"].(map[string]any)
				starts++
			case "content_block_start":
				if i != len(content) || len(open) != 0 {
					t.Fatalf("invalid start index %d: %s", i, stream)
				}
				content = append(content, event["content_block"])
				open[i] = true
			case "content_block_delta":
				if !open[i] {
					t.Fatalf("delta for closed block %d: %s", i, stream)
				}
				delta := event["delta"].(map[string]any)
				block := content[i].(map[string]any)
				switch delta["type"] {
				case "input_json_delta":
					if block["type"] != "tool_use" {
						t.Fatal("tool delta applied to another block kind")
					}
					arguments[i] += stringField(delta, "partial_json")
				case "text_delta", "thinking_delta":
					kind := strings.TrimSuffix(stringField(delta, "type"), "_delta")
					if block["type"] != kind {
						t.Fatal("text delta applied to another block kind")
					}
					block[kind] = stringField(block, kind) + stringField(delta, kind)
				default:
					t.Fatalf("unexpected delta type: %v", delta)
				}
			case "content_block_stop":
				if !open[i] {
					t.Fatalf("duplicate stop for block %d", i)
				}
				block := content[i].(map[string]any)
				if block["type"] == "tool_use" {
					var input map[string]any
					if json.Unmarshal([]byte(arguments[i]), &input) != nil || input == nil {
						t.Fatalf("closed tool contains invalid JSON: %q", arguments[i])
					}
					block["input"] = input
				}
				delete(open, i)
			case "message_delta":
				delta := event["delta"].(map[string]any)
				message["stop_reason"] = delta["stop_reason"]
				message["usage"] = event["usage"]
			case "message_stop":
				if len(open) != 0 {
					t.Fatal("message ended with open content blocks")
				}
				stops++
			case "ping":
			default:
				t.Fatalf("unexpected event: %v", event)
			}
		}
	}
	if starts != 1 || stops != 1 {
		t.Fatalf("message lifecycle: starts=%d stops=%d", starts, stops)
	}
	message["content"] = content
	return message
}

func TestMessagesReviewThinkingToolHistoryRoundTrip(t *testing.T) {
	reply := "data: {\"choices\":[{\"index\":0,\"delta\":{\"reasoning_content\":\"consider\"}}]}\n\n" +
		"data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"reading\"}}]}\n\n" +
		"data: {\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"one\",\"function\":{\"name\":\"read\",\"arguments\":\"{\\\"x\\\":1}\"}}]},\"finish_reason\":\"tool_calls\"}],\"usage\":{\"prompt_tokens\":100,\"completion_tokens\":7,\"completion_thinking_tokens\":3}}\n\ndata: [DONE]\n\n"
	for _, stream := range []bool{false, true} {
		t.Run(map[bool]string{false: "json", true: "stream"}[stream], func(t *testing.T) {
			h, _, _, _ := messagesFixture(t, reply)
			body, _ := json.Marshal(map[string]any{"model": "cn:fixture", "max_tokens": 2048, "stream": stream, "thinking": map[string]any{"type": "enabled", "budget_tokens": 1024}, "messages": []any{map[string]any{"role": "user", "content": "read"}}, "tools": []any{map[string]any{"name": "read", "input_schema": map[string]any{"type": "object"}}}})
			r := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body))
			r.Header.Set("X-API-Key", "fixture-key")
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != http.StatusOK {
				t.Fatalf("%d: %s", w.Code, w.Body.String())
			}
			var message map[string]any
			if stream {
				message = messagesReviewAssemble(t, w.Body.String())
			} else if err := json.Unmarshal(w.Body.Bytes(), &message); err != nil {
				t.Fatal(err)
			}
			parts := message["content"].([]any)
			thinking := parts[0].(map[string]any)
			if thinking["type"] != "thinking" || thinking["thinking"] != "consider" || thinking["signature"] != "" || len(parts) != 3 {
				t.Fatalf("thinking or content order changed: %v", parts)
			}
			usage := message["usage"].(map[string]any)
			if usage["output_tokens"] != float64(7) {
				t.Fatalf("reasoning was counted a second time: %v", usage)
			}
			followup, _ := json.Marshal(map[string]any{"model": "cn:fixture", "max_tokens": 32, "messages": []any{map[string]any{"role": "user", "content": "read"}, map[string]any{"role": "assistant", "content": parts}, map[string]any{"role": "user", "content": []any{map[string]any{"type": "tool_result", "tool_use_id": "one", "content": "done"}}}}})
			chat, _, _, err := messagesToChat(followup)
			if err != nil || !strings.Contains(string(chat), `"reasoning_content":"consider"`) || !strings.Contains(string(chat), `"tool_call_id":"one"`) {
				t.Fatalf("generated history cannot be replayed: %v %s", err, chat)
			}
		})
	}
}

func TestMessagesReviewOneInvalidParallelToolPreventsAllToolStops(t *testing.T) {
	w := httptest.NewRecorder()
	m := newMessagesWriter(w)
	m.stream = true
	m.Header().Set("Content-Type", "text/event-stream")
	_, _ = m.Write([]byte("data: {\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"one\",\"function\":{\"name\":\"read\",\"arguments\":\"{}\"}},{\"index\":1,\"id\":\"two\",\"function\":{\"name\":\"read\",\"arguments\":\"{\"}}]}}]}\n\n"))
	_, err := m.Write([]byte("data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\ndata: [DONE]\n\n"))
	if err == nil || strings.Contains(w.Body.String(), "event: content_block_stop") || strings.Contains(w.Body.String(), "event: message_stop") || !strings.Contains(w.Body.String(), "event: error") {
		t.Fatalf("one invalid tool allowed an executable completion: %v %s", err, w.Body.String())
	}
}

func TestMessagesReviewErrorEnvelopeAndAuthenticationPriority(t *testing.T) {
	for _, c := range []struct {
		status int
		kind   string
	}{
		{413, "request_too_large"}, {429, "rate_limit_error"}, {503, "overloaded_error"}, {401, "authentication_error"},
	} {
		w := httptest.NewRecorder()
		m := newMessagesWriter(w)
		m.failure(c.status, "fixture", "fixture failure")
		if w.Code != c.status || !strings.Contains(w.Body.String(), `"type":"`+c.kind+`"`) {
			t.Fatalf("wrong Anthropic error type: status=%d body=%s", w.Code, w.Body.String())
		}
	}
	h, _, calls, _ := messagesFixture(t, sseOK)
	r := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"cn:fixture","max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`))
	r.Header.Set("X-API-Key", "fixture-key")
	r.Header.Set("Authorization", "Bearer invalid")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusUnauthorized || *calls != 0 {
		t.Fatalf("x-api-key bypassed explicit Authorization: %d %d", w.Code, *calls)
	}
}

type messagesReviewFlushFailure struct{ hdr http.Header }

func (w *messagesReviewFlushFailure) Header() http.Header         { return w.hdr }
func (w *messagesReviewFlushFailure) WriteHeader(int)             {}
func (w *messagesReviewFlushFailure) Write(p []byte) (int, error) { return len(p), nil }
func (w *messagesReviewFlushFailure) FlushError() error           { return io.ErrClosedPipe }

func TestMessagesReviewFlushFailureReachesCompletionError(t *testing.T) {
	m := newMessagesWriter(&messagesReviewFlushFailure{hdr: make(http.Header)})
	m.Header().Set("Content-Type", "text/event-stream")
	_, err := m.Write([]byte("data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"x\"}}]}\n\n"))
	if !errors.Is(err, io.ErrClosedPipe) || !errors.Is(m.CompletionError(), io.ErrClosedPipe) {
		t.Fatalf("client flush failure was hidden: write=%v completion=%v", err, m.CompletionError())
	}
}

func TestMessagesReviewClientCancellationStopsUpstream(t *testing.T) {
	canceled := make(chan struct{})
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"one\",\"function\":{\"name\":\"read\",\"arguments\":\"{\"}}]}}]}\n\n")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
		close(canceled)
	}))
	defer remote.Close()
	h := NewHandler(Config{APIKey: "fixture-key", Pool: testPoolWith(&auth.Auth{UID: "messages-cancel", AccessToken: "fixture", ExpiresAt: 9999999999}), Upstream: &upstream.Client{HTTP: remote.Client(), ChatHTTP: remote.Client(), ChatBaseCN: remote.URL}})
	gateway := httptest.NewServer(h)
	defer gateway.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	r, _ := http.NewRequestWithContext(ctx, http.MethodPost, gateway.URL+"/v1/messages", strings.NewReader(`{"model":"cn:fixture","max_tokens":10,"stream":true,"messages":[{"role":"user","content":"read"}],"tools":[{"name":"read","input_schema":{"type":"object"}}]}`))
	r.Header.Set("X-API-Key", "fixture-key")
	resp, err := gateway.Client().Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var received strings.Builder
	buf := make([]byte, 256)
	for !strings.Contains(received.String(), "message_start") {
		n, err := resp.Body.Read(buf)
		received.Write(buf[:n])
		if err != nil {
			t.Fatalf("no message started before cancellation: %v body=%s", err, received.String())
		}
	}
	cancel()
	select {
	case <-canceled:
	case <-time.After(time.Second):
		t.Fatal("client cancellation left the upstream request running")
	}
	if strings.Contains(received.String(), "message_stop") || strings.Contains(received.String(), `"type":"tool_use"`) {
		t.Fatal("canceled response delivered an unvalidated tool or reported success")
	}
}
