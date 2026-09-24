// ═══ 更新日志 ═══
// 2026-09-25：以小限额复现 SSE 超长行及多 data 行累积，验证及时停止读取且合法长流不受总长度限制。
package upstream

import (
	"errors"
	"io"
	"net/http/httptest"
	"strings"
	"testing"
)

type sseCountingReader struct {
	source io.Reader
	bytes  int
}

type sseLimitFailureReader struct{ err error }

func (r sseLimitFailureReader) Read([]byte) (int, error) { return 0, r.err }

func TestSSEEventLimitFailureDoesNotPublishSuccess(t *testing.T) {
	limitErr := readSSEWithLimit(strings.NewReader("data: "+strings.Repeat("x", 1024)), func(sseEvent) (bool, error) {
		t.Fatal("oversized frame reached the parser")
		return true, nil
	}, nil, 128)
	if limitErr == nil {
		t.Fatal("fixture did not trigger the real event limit")
	}
	for _, mode := range []string{"stream", "aggregate"} {
		t.Run(mode, func(t *testing.T) {
			// The upstream sent an early finish marker and then violated the
			// event bound. The final response still has to be a failure.
			reader := io.MultiReader(strings.NewReader(integrityEvent(integrityContent)+integrityFinish("stop")), sseLimitFailureReader{err: limitErr})
			var err error
			if mode == "aggregate" {
				var response map[string]any
				response, err = Aggregate(reader)
				if response != nil {
					t.Error("event limit failure returned a successful completion")
				}
			} else {
				writer := httptest.NewRecorder()
				err = Stream(writer, reader)
				if strings.Contains(writer.Body.String(), `"finish_reason":"stop"`) {
					t.Error("early finish was published despite the later event limit error")
				}
				failures := integrityErrorFrames(t, writer.Body.String())
				if len(failures) != 1 || failures[0]["code"] != "upstream_event_too_large" {
					t.Errorf("typed event size failure did not reach the client: %v", failures)
				}
			}
			var failure *StreamError
			if !errors.As(err, &failure) || failure.Code != "upstream_event_too_large" {
				t.Errorf("event size failure was lost before accounting: %v", err)
			}
		})
	}
}

func (r *sseCountingReader) Read(out []byte) (int, error) {
	n, err := r.source.Read(out)
	r.bytes += n
	return n, err
}

func TestSSEEventLimitRejectsOversizeBeforeConsumption(t *testing.T) {
	const limit = 1024
	for _, tc := range []struct{ name, raw string }{
		{"single_line", "data: " + strings.Repeat("x", 64*limit) + "\n\n"},
		{"unfinished_line", "data: " + strings.Repeat("x", 64*limit)},
		{"comment_line", ": " + strings.Repeat("x", 64*limit) + "\n\n"},
		{"event_name_line", "event: " + strings.Repeat("x", 64*limit) + "\n\n"},
		{"multiline_data", strings.Repeat("data: "+strings.Repeat("x", 100)+"\n", 16) + "\n"},
		{"event_name_and_data", "event: " + strings.Repeat("x", 600) + "\ndata: " + strings.Repeat("x", 600) + "\n\n"},
		{"empty_data_lines", strings.Repeat("data:\n", 2*limit) + "\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reader := &sseCountingReader{source: strings.NewReader(tc.raw)}
			consumed := false
			err := readSSEWithLimit(reader, func(sseEvent) (bool, error) { consumed = true; return false, nil }, nil, limit)
			var failure *StreamError
			if !errors.As(err, &failure) || failure.Code != "upstream_event_too_large" {
				t.Fatalf("oversized SSE was not rejected explicitly: err=%v consumed=%v read=%d", err, consumed, reader.bytes)
			}
			if consumed {
				t.Fatal("oversized event was passed to the completion parser")
			}
			if strings.HasSuffix(tc.name, "line") && reader.bytes > 2*limit {
				t.Errorf("parser allocated/read the whole oversized line before checking: bytes=%d", reader.bytes)
			}
		})
	}
}

func TestSSEEventLimitPreservesValidLongStreams(t *testing.T) {
	const limit = 128
	const count = 1000
	raw := strings.Repeat(": keepalive\r\nevent: message\r\ndata: first\r\ndata: second\r\n\r\n", count) + "data: tail"
	consumed, comments := 0, 0
	err := readSSEWithLimit(strings.NewReader(raw), func(ev sseEvent) (bool, error) {
		if consumed < count {
			if ev.name != "message" || ev.data != "first\nsecond" {
				t.Errorf("event corrupted: %+v", ev)
			}
		} else if ev.name != "" || ev.data != "tail" {
			t.Errorf("EOF event corrupted: %+v", ev)
		}
		consumed++
		return false, nil
	}, func(line string) error { comments++; return nil }, limit)
	if err != nil || consumed != count+1 || comments != count {
		t.Fatalf("bounded events in long stream failed: err=%v events=%d comments=%d", err, consumed, comments)
	}
}
