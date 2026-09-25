// ═══ 更新日志 ═══
// 2026-09-25：复现长工具缓冲期间保活被吞的问题；确认空 JSON 保活不泄露工具或计量并传播写失败。
package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func geminiProgressWriter(t *testing.T, output http.ResponseWriter) *chatContractWriter {
	t.Helper()
	chat, _, err := geminiToChat([]byte(`{"contents":[{"parts":[{"text":"read"}]}],"tools":[{"functionDeclarations":[{"name":"read","parameters":{"type":"OBJECT","properties":{"path":{"type":"STRING"}},"required":["path"]}}]}]}`), "cn:fixture", true)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(chat, &fields); err != nil {
		t.Fatal(err)
	}
	contract, err := newChatOutputContract(fields)
	if err != nil || contract == nil {
		t.Fatalf("missing tool contract: %v", err)
	}
	g := newGeminiWriter(output)
	g.model, g.stream = "cn:fixture", true
	w := &chatContractWriter{inner: g, req: contract, lastWrite: time.Now()}
	w.Header().Set("Content-Type", "text/event-stream")
	return w
}

func geminiProgressToolFragment(t *testing.T, arguments string, initial bool) []byte {
	t.Helper()
	function := map[string]any{"arguments": arguments}
	call := map[string]any{"index": 0, "function": function}
	if initial {
		call["id"], call["type"], function["name"] = "one", "function", "read"
	}
	raw, err := json.Marshal(map[string]any{"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"tool_calls": []any{call}}}}})
	if err != nil {
		t.Fatal(err)
	}
	return append(append([]byte("data: "), raw...), '\n', '\n')
}

func TestGeminiKeepsBufferedToolProgressUntilValidated(t *testing.T) {
	rec := httptest.NewRecorder()
	writer := geminiProgressWriter(t, rec)
	if _, err := writer.Write(geminiProgressToolFragment(t, `{"path":"`, true)); err != nil {
		t.Fatal(err)
	}
	if rec.Body.Len() != 0 {
		t.Fatalf("unvalidated tool content was sent before a heartbeat was due: %s", rec.Body.String())
	}
	writer.lastWrite = time.Now().Add(-2 * protocolProgressInterval)
	if _, err := writer.Write(geminiProgressToolFragment(t, "more", false)); err != nil {
		t.Fatal(err)
	}
	if rec.Body.Len() == 0 || !rec.Flushed || rec.Header().Get("Content-Type") != "text/event-stream" {
		t.Fatalf("due tool-buffer keepalive produced no flushed Gemini stream: %s", rec.Body.String())
	}
	for _, frame := range strings.Split(rec.Body.String(), "\n\n") {
		if strings.TrimSpace(frame) == "" {
			continue
		}
		var payload map[string]any
		if !strings.HasPrefix(frame, "data: ") || json.Unmarshal([]byte(strings.TrimPrefix(frame, "data: ")), &payload) != nil || payload == nil || len(payload) != 0 {
			t.Fatalf("keepalive is not an empty JSON data frame: %q", frame)
		}
	}
	if _, err := writer.Write(geminiProgressToolFragment(t, `"}`, false)); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(rec.Body.String(), "functionCall") {
		t.Fatal("a complete argument string bypassed terminal group validation")
	}
	_, _ = writer.Write([]byte("data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\ndata: [DONE]\n\n"))
	if err := finishResponseWriters(writer); err != nil {
		t.Fatal(err)
	}
	if strings.Count(rec.Body.String(), "functionCall") != 1 || strings.Count(rec.Body.String(), "finishReason") != 1 || !strings.Contains(rec.Body.String(), `"path":"more"`) {
		t.Fatalf("validated tool was lost or duplicated after keepalive: %s", rec.Body.String())
	}
}

func TestGeminiBufferedToolKeepalivePropagatesWriteAndFlushFailures(t *testing.T) {
	for _, flush := range []bool{false, true} {
		output := &geminiFaultWriter{ResponseRecorder: httptest.NewRecorder(), flush: flush}
		if !flush {
			output.reject = "data: {}"
		}
		writer := geminiProgressWriter(t, output)
		if _, err := writer.Write(geminiProgressToolFragment(t, `{"path":"`, true)); err != nil {
			t.Fatal(err)
		}
		writer.lastWrite = time.Now().Add(-2 * protocolProgressInterval)
		_, err := writer.Write(geminiProgressToolFragment(t, "more", false))
		if err == nil || writer.inner.(*geminiWriter).CompletionError() == nil || finishResponseWriters(writer) == nil {
			t.Fatalf("buffered tool heartbeat write/flush failure was swallowed: flush=%v body=%s err=%v", flush, output.Body.String(), err)
		}
		for _, forbidden := range []string{"functionCall", "usageMetadata", "finishReason", "\"name\"", "\"arguments\""} {
			if strings.Contains(output.Body.String(), forbidden) {
				t.Fatalf("failed heartbeat leaked %s: %s", forbidden, output.Body.String())
			}
		}
	}
}
