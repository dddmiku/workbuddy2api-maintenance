// ═══ 更新日志 ═══
// 2026-09-25：复现 Gemini 流的错误类型、空事件和参数完整性边界，验证工具整组交付与写失败传播。
package server

import (
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
)

func geminiStreamFixture() (*geminiWriter, *httptest.ResponseRecorder) {
	rec := httptest.NewRecorder()
	g := newGeminiWriter(rec)
	g.stream, g.model, g.includeThoughts = true, "global:hy3", true
	g.Header().Set("Content-Type", "text/event-stream")
	return g, rec
}

func TestGeminiWriterRejectsMalformedOutputInsteadOfDroppingIt(t *testing.T) {
	for _, body := range []string{
		`{"choices":[{"index":0,"delta":{"content":"ok","tool_calls":{}},"finish_reason":"stop"}]}`,
		`{"choices":[{"index":0,"delta":{"content":"ok","reasoning_content":[]},"finish_reason":"stop"}]}`,
		`{"choices":[{"index":0,"delta":{"content":"ok"},"finish_reason":"length"}]}` + "\n\ndata: " + `{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
	} {
		g, rec := geminiStreamFixture()
		_, _ = g.Write([]byte("data: " + body + "\n\ndata: [DONE]\n\n"))
		g.finish()
		if g.CompletionError() == nil || strings.Contains(rec.Body.String(), `"finishReason":"STOP"`) {
			t.Fatalf("malformed output was accepted: %s", rec.Body.String())
		}
	}
}

func TestGeminiWriterIgnoresEmptyDataAndPreservesRateLimitError(t *testing.T) {
	g, rec := geminiStreamFixture()
	_, _ = g.Write([]byte(": keepalive\n\ndata:\n\ndata: \n\ndata: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"}}]}\n\n"))
	if g.CompletionError() != nil || !strings.Contains(rec.Body.String(), `"text":"ok"`) {
		t.Fatalf("empty SSE events interrupted the response: %s", rec.Body.String())
	}
	_, _ = g.Write([]byte("data: {\"error\":{\"type\":\"rate_limit_error\",\"message\":\"try later\"}}\n\n"))
	g.finish()
	if g.CompletionError() == nil || !strings.Contains(rec.Body.String(), `"status":"RESOURCE_EXHAUSTED"`) || !strings.Contains(rec.Body.String(), `"finishReason":"OTHER"`) || strings.Contains(rec.Body.String(), `"finishReason":"STOP"`) {
		t.Fatalf("stream error lost its category or became success: %s", rec.Body.String())
	}
}

func TestGeminiWriterToolsWaitForWholeGroupValidation(t *testing.T) {
	for _, valid := range []bool{false, true} {
		g, rec := geminiStreamFixture()
		_, err := g.Write([]byte("data: {\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"a\",\"function\":{\"name\":\"read\",\"arguments\":\"{\\\"id\\\":1}\"}},{\"index\":1,\"id\":\"b\",\"function\":{\"name\":\"read\",\"arguments\":\"{\\\"id\\\":\"}}]}}]}\n\n"))
		if err != nil || strings.Contains(rec.Body.String(), "functionCall") {
			t.Fatalf("tool became executable before whole group was valid: %s (%v)", rec.Body.String(), err)
		}
		if valid {
			_, _ = g.Write([]byte("data: {\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":1,\"function\":{\"arguments\":\"2}\"}}]}}]}\n\n"))
		}
		_, _ = g.Write([]byte("data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\ndata: [DONE]\n\n"))
		g.finish()
		if valid {
			if g.CompletionError() != nil || strings.Count(rec.Body.String(), "functionCall") != 2 || strings.Count(rec.Body.String(), "finishReason") != 1 {
				t.Fatalf("valid tool group was lost or repeated: %s (%v)", rec.Body.String(), g.CompletionError())
			}
		} else if g.CompletionError() == nil || strings.Contains(rec.Body.String(), "functionCall") || strings.Contains(rec.Body.String(), "finishReason") {
			t.Fatalf("partially valid group leaked an executable tool: %s", rec.Body.String())
		}
	}
}

type geminiFaultWriter struct {
	*httptest.ResponseRecorder
	reject string
	flush  bool
}

func (w *geminiFaultWriter) Write(raw []byte) (int, error) {
	if w.reject != "" && strings.Contains(string(raw), w.reject) {
		return 0, errors.New("fixture downstream write failed")
	}
	return w.ResponseRecorder.Write(raw)
}

func (w *geminiFaultWriter) FlushError() error {
	if w.flush {
		return errors.New("fixture downstream flush failed")
	}
	return nil
}

func TestGeminiWriterCompletionAndFlushFailuresAreObservable(t *testing.T) {
	for _, flush := range []bool{false, true} {
		rec := &geminiFaultWriter{ResponseRecorder: httptest.NewRecorder(), reject: "finishReason", flush: flush}
		g := newGeminiWriter(rec)
		g.model, g.stream = "cn:fixture", true
		g.Header().Set("Content-Type", "text/event-stream")
		_, _ = g.Write([]byte("data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\n"))
		err := g.FinishResponse()
		if err == nil || g.CompletionError() == nil || strings.Contains(rec.Body.String(), "finishReason") {
			t.Fatalf("completion write/flush failure disappeared: %s (%v)", rec.Body.String(), err)
		}
	}
}

func TestGeminiWriterJSONSuccessKeepsRequestedSSETransport(t *testing.T) {
	g, rec := geminiStreamFixture()
	g.Header().Set("Content-Type", "application/json")
	_, err := g.Write([]byte(`{"choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":2,"completion_tokens":1}}`))
	g.finish()
	if err != nil || rec.Header().Get("Content-Type") != "text/event-stream" || !strings.HasPrefix(rec.Body.String(), "data: ") || strings.Count(rec.Body.String(), "finishReason") != 1 {
		t.Fatalf("streamGenerateContent returned JSON instead of SSE: %v %s (%v)", rec.Header(), rec.Body.String(), err)
	}
}

func TestGeminiStreamFailureCarriesNativeFailureTerminal(t *testing.T) {
	g, rec := geminiStreamFixture()
	_, _ = g.Write([]byte("data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"partial\"}}]}\n\ndata: {\"error\":{\"code\":\"fixture_failure\",\"message\":\"fixture stream failure\"}}\n\n"))
	g.finish()
	if g.CompletionError() == nil {
		t.Fatal("stream failure did not reach accounting")
	}
	for _, expected := range []string{`"error"`, `"code":502`, `"finishReason":"OTHER"`, `"finishMessage":"fixture stream failure"`} {
		if !strings.Contains(rec.Body.String(), expected) {
			t.Fatalf("NF/SDK failure signal missing %s: %s", expected, rec.Body.String())
		}
	}
	for _, forbidden := range []string{`"finishReason":"STOP"`, "functionCall", "usageMetadata"} {
		if strings.Contains(rec.Body.String(), forbidden) {
			t.Fatalf("stream failure invented %s: %s", forbidden, rec.Body.String())
		}
	}
	before := rec.Body.Len()
	_ = g.FinishResponse()
	if rec.Body.Len() != before {
		t.Fatal("failure terminal was emitted more than once")
	}
}
