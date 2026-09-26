// ═══ 更新日志 ═══
// 2026-09-25：原生 Chat 显式关闭用量展示时仅过滤最终输出，协议校验与内部账本仍读取完整上游用量。
package server

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
)

// hideChatStreamUsage 报告客户端是否显式关闭了流式用量展示。
//
// 注意：缺省（未给 stream_options）时**不隐藏**，这是本网关的既有选择（README 与
// docs/agent-compatibility.md 均按此描述）：只有显式 include_usage=false 才隐藏。
// 与 OpenAI 规范（缺省即不下发用量帧）不同，见 CHANGELOG「已知偏差」。
func hideChatStreamUsage(fields map[string]json.RawMessage) bool {
	var options struct {
		IncludeUsage *bool `json:"include_usage"`
	}
	return json.Unmarshal(fields["stream_options"], &options) == nil && options.IncludeUsage != nil && !*options.IncludeUsage
}

// chatUsageVisibilityWriter sits below the Chat contract writer. Thus schema,
// tool completion and accounting all observe the original frames; this changes
// only the output option explicitly selected by the native Chat client.
type chatUsageVisibilityWriter struct {
	inner  http.ResponseWriter
	buffer []byte
	err    error
}

func (w *chatUsageVisibilityWriter) Header() http.Header         { return w.inner.Header() }
func (w *chatUsageVisibilityWriter) Unwrap() http.ResponseWriter { return w.inner }
func (w *chatUsageVisibilityWriter) WriteHeader(status int)      { w.inner.WriteHeader(status) }

func (w *chatUsageVisibilityWriter) Write(raw []byte) (int, error) {
	if w.err != nil {
		return 0, w.err
	}
	if !strings.HasPrefix(w.Header().Get("Content-Type"), "text/event-stream") {
		return w.inner.Write(raw)
	}
	w.buffer = append(w.buffer, raw...)
	for {
		end := bytes.Index(w.buffer, []byte("\n\n"))
		if end < 0 {
			break
		}
		frame := w.buffer[:end+2]
		w.buffer = w.buffer[end+2:]
		if filtered := hideUsageFrame(frame); len(filtered) > 0 {
			_, w.err = w.inner.Write(filtered)
			if w.err != nil {
				return 0, w.err
			}
		}
	}
	return len(raw), nil
}

func hideUsageFrame(frame []byte) []byte {
	line := bytes.TrimSpace(frame)
	if !bytes.HasPrefix(line, []byte("data:")) {
		return frame
	}
	payload := bytes.TrimSpace(bytes.TrimPrefix(line, []byte("data:")))
	var object map[string]json.RawMessage
	if json.Unmarshal(payload, &object) != nil || object == nil {
		return frame
	}
	if _, hasUsage := object["usage"]; !hasUsage {
		return frame
	}
	var choices []json.RawMessage
	_ = json.Unmarshal(object["choices"], &choices)
	problem := bytes.TrimSpace(object["error"])
	if len(choices) == 0 && (len(problem) == 0 || bytes.Equal(problem, []byte("null"))) {
		// 只含用量的帧：未请求就整帧丢弃（客户端拿到空 choices 会报错）。
		return nil
	}
	delete(object, "usage")
	encoded, err := json.Marshal(object)
	if err != nil {
		return frame
	}
	return append(append([]byte("data: "), encoded...), '\n', '\n')
}

func (w *chatUsageVisibilityWriter) Flush() { _ = w.FlushError() }
func (w *chatUsageVisibilityWriter) FlushError() error {
	if w.err == nil {
		w.err = flushHTTPResponse(w.inner)
	}
	return w.err
}
func (w *chatUsageVisibilityWriter) CompletionError() error {
	if w.err != nil {
		return w.err
	}
	if len(bytes.TrimSpace(w.buffer)) > 0 {
		return io.ErrUnexpectedEOF
	}
	if checker, ok := w.inner.(interface{ CompletionError() error }); ok {
		return checker.CompletionError()
	}
	return nil
}
