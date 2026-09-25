// ═══ 更新日志 ═══
// 2026-09-26：长回答（超过旧 16MB 整流缓冲、约 7.4 万分片）完整交付，末尾工具调用仍按整组校验；截断与空响应语义不变。
package server

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

func longAnswerContract(t *testing.T) *chatContractWriter {
	t.Helper()
	var fields map[string]json.RawMessage
	_ = json.Unmarshal([]byte(`{"model":"global:hy3","stream":true,"messages":[],"tools":[{"type":"function","function":{"name":"Read","parameters":{"type":"object"}}}],"tool_choice":"auto"}`), &fields)
	contract, err := newChatOutputContract(fields)
	if err != nil || contract == nil {
		t.Fatalf("contract not installed: %v", err)
	}
	writer := &chatContractWriter{inner: httptest.NewRecorder(), req: contract}
	writer.Header().Set("Content-Type", "text/event-stream")
	return writer
}

func TestChatContractDeliversAnswersBeyondOldBuffer(t *testing.T) {
	writer := longAnswerContract(t)
	recorder := writer.inner.(*httptest.ResponseRecorder)
	chunk := []byte("data: " + `{"id":"c1","model":"m","choices":[{"index":0,"delta":{"content":"` + strings.Repeat("长", 60) + `","reasoning_content":"` + strings.Repeat("r", 40) + `"}}]}` + "\n\n")
	total := 0
	for total < 20<<20 { // 超过旧的 16MB 整流上限
		if _, err := writer.Write(chunk); err != nil {
			t.Fatalf("long answer cut after %d bytes: %v", total, err)
		}
		total += len(chunk)
	}
	_, _ = writer.Write([]byte("data: " + nfBoundaryToolFrame(0, "Read", `{"path":"file.txt"}`) + "\n\n"))
	_, _ = writer.Write(sseStream(`{"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`))
	if err := writer.CompletionError(); err != nil {
		t.Fatalf("long answer failed validation: %v", err)
	}
	body := recorder.Body.String()
	if !strings.Contains(body, `"tool_calls":[`) || strings.Contains(body, "response_contract_violation") {
		t.Fatal("validated tool call missing after a long answer")
	}
	if writer.raw.Len() > 64<<10 {
		t.Fatalf("validation buffer grew with the answer text: %d bytes", writer.raw.Len())
	}
}

func TestChatContractStillDetectsTruncatedAndEmptyStreams(t *testing.T) {
	truncated := longAnswerContract(t)
	_, _ = truncated.Write([]byte("data: " + `{"choices":[{"index":0,"delta":{"content":"partial"}}]}` + "\n\n"))
	if err := truncated.CompletionError(); err == nil || !strings.Contains(err.Error(), "terminal marker") {
		t.Fatalf("truncated stream not reported: %v", err)
	}
	empty := longAnswerContract(t)
	_, _ = empty.Write([]byte("data: [DONE]\n\n"))
	if err := empty.CompletionError(); err == nil {
		t.Fatal("empty stream accepted")
	}
}
