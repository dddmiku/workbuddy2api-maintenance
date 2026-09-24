// ═══ 更新日志 ═══
// 2026-09-25：验证前置用量读取器也限制流行/事件分配，并保留已观测用量和原始读错误。
package server

import (
	"errors"
	"io"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"workbuddy2api/internal/upstream"
)

type countedStatsSource struct {
	reader io.Reader
	bytes  int
}

func (r *countedStatsSource) Read(p []byte) (int, error) {
	n, err := r.reader.Read(p)
	r.bytes += n
	return n, err
}

func TestUsageReaderRejectsOversizedLinesAndEvents(t *testing.T) {
	for _, tc := range []struct{ name, body string }{
		{"single_data_line", "data: " + strings.Repeat("x", 1<<20) + "\n\n"},
		{"single_comment_line", ":" + strings.Repeat("x", 1<<20) + "\n\n"},
		{"multiline_data", strings.Repeat("data: 0123456789\n", 20) + "\n"},
		{"empty_data_lines", strings.Repeat("data:\n", 132) + "\n"},
		{"bare_data_lines", strings.Repeat("data\n", 132) + "\n"},
		{"event_and_data", "event: " + strings.Repeat("e", 70) + "\ndata: " + strings.Repeat("d", 70) + "\n\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			source := &countedStatsSource{reader: strings.NewReader(tc.body)}
			r := newChatStatsReaderSince(source, time.Now())
			r.maxEventBytes = 128
			_, err := io.Copy(io.Discard, r)
			var tooLarge *upstream.StreamError
			if !errors.As(err, &tooLarge) || tooLarge.Code != "upstream_event_too_large" {
				t.Fatalf("unbounded input did not retain typed size error: %v", err)
			}
			if len(tc.body) > 1<<20 && source.bytes > 64<<10 {
				t.Fatalf("observer consumed the oversized stream before rejecting it: %d bytes", source.bytes)
			}
			if r.hasUsage {
				t.Fatal("oversized event was accepted as a usage observation")
			}
		})
	}
}

func TestUsageReaderKeepsKnownUsageBeforeOversizedEvent(t *testing.T) {
	valid := "data: {\"usage\":{\"prompt_tokens\":6,\"completion_tokens\":2,\"credit\":0.1}}\n\n"
	body := valid + "data: {\"usage\":{\"prompt_tokens\":999},\"padding\":\"" + strings.Repeat("x", 512) + "\"}\n\n"
	r := newChatStatsReaderSince(strings.NewReader(body), time.Now())
	r.maxEventBytes = 128
	_, err := io.Copy(io.Discard, r)
	if err == nil {
		t.Fatal("oversized event was not rejected")
	}
	if r.PromptTokens() != 6 || r.TotalTokens() != 8 {
		t.Fatalf("known usage was lost or rejected data was counted: prompt=%d total=%d", r.PromptTokens(), r.TotalTokens())
	}
}

func TestUsageReaderLimitsEachEventRatherThanWholeStream(t *testing.T) {
	event := "data: {\"usage\":{\"prompt_tokens\":6,\"completion_tokens\":2}}\n\n"
	body := strings.Repeat(event, 50)
	r := newChatStatsReaderSince(strings.NewReader(body), time.Now())
	r.maxEventBytes = 128
	out, err := io.ReadAll(r)
	if err != nil || string(out) != body || r.TotalTokens() != 8 {
		t.Fatalf("legal long stream was truncated or double-counted: bytes=%d total=%d err=%v", len(out), r.TotalTokens(), err)
	}
}

type statsDataAndError struct {
	data []byte
	err  error
}

func (r *statsDataAndError) Read(p []byte) (int, error) {
	n := copy(p, r.data)
	r.data = r.data[n:]
	if len(r.data) == 0 {
		return n, r.err
	}
	return n, nil
}

func TestUsageReaderRetainsDataAndOriginalReadError(t *testing.T) {
	for _, sourceErr := range []error{io.EOF, io.ErrUnexpectedEOF, errors.New("transport reset")} {
		t.Run(sourceErr.Error(), func(t *testing.T) {
			body := "data: {\"usage\":{\"prompt_tokens\":6,\"completion_tokens\":2}}\n\n" + "data: {\"usage\":{\"prompt_tokens\":9,\"completion_tokens\":3}}"
			r := newChatStatsReaderSince(&statsDataAndError{data: []byte(body), err: sourceErr}, time.Now())
			r.maxEventBytes = 128
			var out strings.Builder
			buffer := make([]byte, 3)
			_, err := io.CopyBuffer(&out, r, buffer)
			if string(out.String()) != body {
				t.Fatal("reader lost data accompanying terminal error")
			}
			want := 8
			if sourceErr == io.EOF {
				want = 12
				if err != nil {
					t.Fatal(err)
				}
			} else if !errors.Is(err, sourceErr) {
				t.Fatalf("original transport failure was lost: %v", err)
			}
			if r.TotalTokens() != want {
				t.Fatalf("EOF/non-EOF pending usage contract changed: total=%d want=%d", r.TotalTokens(), want)
			}
		})
	}
}

func TestOversizedUsageReaderEventAfterFinishCannotBecomeSuccess(t *testing.T) {
	prefix := "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hello\"}}]}\n\n" +
		"data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":6,\"completion_tokens\":2}}\n\n"
	r := newChatStatsReaderSince(strings.NewReader(prefix+"data: "+strings.Repeat("x", 2048)+"\n\n"), time.Now())
	r.maxEventBytes = 512
	w := httptest.NewRecorder()
	err := upstream.Stream(w, r, upstream.StreamOptions{Model: "m", ReasoningLoopGuard: false})
	var streamErr *upstream.StreamError
	if !errors.As(err, &streamErr) || streamErr.Code != "upstream_event_too_large" {
		t.Fatalf("usage observer masked size failure: %v", err)
	}
	if strings.Contains(w.Body.String(), `"finish_reason":"stop"`) || !strings.Contains(w.Body.String(), "upstream_event_too_large") {
		t.Fatalf("early finish became success despite oversized tail: %s", w.Body)
	}
	if r.TotalTokens() != 8 {
		t.Fatalf("known usage changed after failed tail: %d", r.TotalTokens())
	}
}
