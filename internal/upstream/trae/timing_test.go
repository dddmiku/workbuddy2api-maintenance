package trae

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestTimingSeparatesHeaderAndEventWait(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(25 * time.Millisecond)
		w.Header().Set("Content-Type", "text/event-stream")
		w.(http.Flusher).Flush()
		time.Sleep(40 * time.Millisecond)
		io.WriteString(w, ": heartbeat\n\n")
	}))
	defer srv.Close()
	var mu sync.Mutex
	var events []TimingEvent
	ctx := withTimingRoute(WithTimingObserver(context.Background(), func(e TimingEvent) { mu.Lock(); events = append(events, e); mu.Unlock() }), "remote")
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, nil)
	resp, err := doTimed(ctx, srv.Client(), req, "header")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil || string(data) != ": heartbeat\n\n" {
		t.Fatal("timing changed the upstream bytes")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(events) != 2 || events[0].Stage != "header" || events[1].Stage != "first_event" || events[0].DurationMS < 20 || events[1].DurationMS < 30 {
		t.Fatal("header delay and event delay were conflated")
	}
}

func TestRemoteFirstEventLargeReadRecognized(t *testing.T) {
	frame := "event: plan_item\ndata: {\"thought\":\"" + strings.Repeat("x", 4096) + "\"}\n\n"
	r := newRemoteFirstEventReader(io.NopCloser(strings.NewReader(frame)))
	defer r.Close()
	r.deadline = time.Now().Add(-time.Second)
	buf := make([]byte, len(frame)+1)
	if n, err := r.Read(buf); n != len(frame) || err != nil {
		t.Fatal("model frame read failed")
	}
	if !r.sawFirst {
		t.Fatal("large model event lost its prefix before the first-event check")
	}
	if _, err := r.Read(buf); err != io.EOF {
		t.Fatal("recognized large model event was later mistaken for a first-event timeout")
	}
}
