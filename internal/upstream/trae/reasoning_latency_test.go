package trae

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// An origin that cannot send content until the translated reasoning is read.
// The unchanged translator deadlocks behind its content-triggered buffer;
// the bounded timeout is a failure detector, not a simulated model timeout.
func TestReasoningDeliveredBeforeOriginContent(t *testing.T) {
	release := make(chan struct{})
	var once sync.Once
	finish := func() { once.Do(func() { close(release) }) }
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "event: output\ndata: {\"reasoning_content\":\"phase-a\"}\n\n")
		w.(http.Flusher).Flush()
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		io.WriteString(w, "event: output\ndata: {\"reasoning_content\":\"phase-a phase-b\",\"response\":\"fixture\"}\n\nevent: done\ndata: {}\n\n")
	}))
	defer origin.Close()
	var mu sync.Mutex
	var events []TimingEvent
	ctx := withTimingRoute(WithTimingObserver(context.Background(), func(e TimingEvent) { mu.Lock(); events = append(events, e); mu.Unlock() }), "legacy")
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, origin.URL, nil)
	resp, err := doTimed(ctx, origin.Client(), req, "header")
	if err != nil {
		t.Fatal(err)
	}
	stream := translateStreamWithCredit(resp.Body, "deepseek-v4.1-flash", nil, nil, ctx)
	defer stream.Close()
	defer finish()
	first := make(chan struct{}, 1)
	done := make(chan error, 1)
	go func() {
		r := bufio.NewReader(stream)
		var err error
		for err == nil {
			var line string
			line, err = r.ReadString('\n')
			if strings.HasPrefix(line, "data:") {
				var frame struct {
					Choices []struct {
						Delta struct {
							Reasoning string `json:"reasoning_content"`
						} `json:"delta"`
					} `json:"choices"`
				}
				if json.Unmarshal([]byte(strings.TrimSpace(strings.TrimPrefix(line, "data:"))), &frame) == nil {
					for _, c := range frame.Choices {
						if c.Delta.Reasoning != "" {
							select {
							case first <- struct{}{}:
							default:
							}
						}
					}
				}
			}
		}
		done <- err
	}()
	select {
	case <-first:
		finish()
	case <-time.After(500 * time.Millisecond):
		finish()
		<-done
		t.Fatal("raw reasoning was buffered until origin content instead of reaching the stream")
	}
	if err := <-done; err != io.EOF {
		t.Fatal("stream did not finish cleanly")
	}
	mu.Lock()
	defer mu.Unlock()
	var raw, translated *TimingEvent
	for i := range events {
		if events[i].Stage == "upstream_reasoning" {
			raw = &events[i]
		}
		if events[i].Stage == "first_reasoning" {
			translated = &events[i]
		}
	}
	if raw == nil || translated == nil || translated.ElapsedMS < raw.ElapsedMS || translated.DurationMS >= 500 {
		t.Fatal("raw and translated reasoning stages were not independently measured")
	}
	t.Logf("raw_reasoning_ms=%.3f translated_reasoning_ms=%.3f translation_buffer_ms=%.3f", raw.ElapsedMS, translated.ElapsedMS, translated.DurationMS)
}
