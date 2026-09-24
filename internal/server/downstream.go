// ═══ 更新日志 ═══
// 2026-09-25：按每次实际写出限制慢客户端等待时间，写出后清除期限以保留长推理，并将失败取消传回上游。
package server

import (
	"context"
	"errors"
	"io"
	"net/http"
	"time"
)

const downstreamWriteTimeout = 30 * time.Second

// boundedResponseWriter guards the actual network writer, outside protocol
// adapters. Its deadline is active only while writing: a model may legitimately
// think for longer than this without producing another chunk. In particular,
// leaving a deadline armed during that silence can reset an HTTP/2 stream.
type boundedResponseWriter struct {
	inner      http.ResponseWriter
	controller *http.ResponseController
	timeout    time.Duration
	cancel     context.CancelCauseFunc
	err        error
}

func newBoundedResponseWriter(inner http.ResponseWriter, cancel context.CancelCauseFunc, timeout time.Duration) *boundedResponseWriter {
	return &boundedResponseWriter{inner: inner, controller: http.NewResponseController(inner), cancel: cancel, timeout: timeout}
}

func (w *boundedResponseWriter) Header() http.Header         { return w.inner.Header() }
func (w *boundedResponseWriter) Unwrap() http.ResponseWriter { return w.inner }
func (w *boundedResponseWriter) CompletionError() error      { return w.err }

func (w *boundedResponseWriter) fail(err error) error {
	if err != nil && w.err == nil {
		w.err = err
		w.cancel(err)
	}
	return w.err
}

func (w *boundedResponseWriter) begin() error {
	if w.err != nil {
		return w.err
	}
	if err := w.controller.SetWriteDeadline(time.Now().Add(w.timeout)); err != nil && !errors.Is(err, http.ErrNotSupported) {
		return w.fail(err)
	}
	return nil
}

func (w *boundedResponseWriter) complete(err error) error {
	_ = w.fail(err) // fail records the error and cancels the upstream request.
	if resetErr := w.controller.SetWriteDeadline(time.Time{}); resetErr != nil && !errors.Is(resetErr, http.ErrNotSupported) {
		_ = w.fail(resetErr)
	}
	return w.err
}

func (w *boundedResponseWriter) WriteHeader(status int) {
	if w.begin() != nil {
		return
	}
	w.inner.WriteHeader(status)
	_ = w.complete(nil) // WriteHeader has no error return; CompletionError retains it.
}

func (w *boundedResponseWriter) Write(p []byte) (int, error) {
	if err := w.begin(); err != nil {
		return 0, err
	}
	n, err := w.inner.Write(p)
	if err == nil && n != len(p) {
		err = io.ErrShortWrite
	}
	return n, w.complete(err)
}

func (w *boundedResponseWriter) Flush() { _ = w.FlushError() }

func (w *boundedResponseWriter) FlushError() error {
	if err := w.begin(); err != nil {
		return err
	}
	return w.complete(flushHTTPResponse(w.inner))
}

// Unlike http.Flusher, ResponseController exposes a failed final network flush.
// Recorders and other deliberate non-network writers may not support flushing.
func flushHTTPResponse(w http.ResponseWriter) error {
	err := http.NewResponseController(w).Flush()
	if errors.Is(err, http.ErrNotSupported) {
		return nil
	}
	return err
}
