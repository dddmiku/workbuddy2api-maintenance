// ═══ 更新日志 ═══
// 2026-09-26：用量按 OpenAI 规范下发（仅显式 include_usage=true）。
// 2026-09-25：原生 Chat 显式关闭用量展示时仅过滤最终输出，协议校验与内部账本仍读取完整上游用量。
package server

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
)

// hideChatStreamUsage 报告是否应隐藏客户端可见的用量。
//
// 按 OpenAI 规范：只有显式 stream_options.include_usage=true 才下发用量（末尾那一帧
// choices 为空、只带 usage 的快照）；缺省与 false 都不下发。严格按 chunk.choices[0]
// 取值的客户端遇到空 choices 会直接报错，所以缺省必须隐藏。
//
// 隐藏只作用于客户端展示：网关内部的账本、协议校验与工具契约仍读取完整上游帧。
func hideChatStreamUsage(fields map[string]json.RawMessage) bool {
	var options struct {
		IncludeUsage *bool `json:"include_usage"`
	}
	if raw, present := fields["stream_options"]; present && json.Unmarshal(raw, &options) != nil {
		// stream_options 存在但不是合法对象：按「未请求用量」处理，不把畸形请求
		// 当成显式请求而多发一帧。
		return true
	}
	return options.IncludeUsage == nil || !*options.IncludeUsage
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
