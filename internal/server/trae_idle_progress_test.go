package server

import (
	"bufio"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"workbuddy2api/internal/upstream"
	"workbuddy2api/internal/upstream/trae"
)

func TestTraeIdleKeepaliveReachesGuardedChat(t *testing.T) {
	release := make(chan struct{})
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.(http.Flusher).Flush()
		select {
		case <-r.Context().Done():
		case <-release:
			_, _ = io.WriteString(w, "event: output\ndata: {\"response\":\"ok\"}\n\nevent: done\ndata: {}\n\n")
		}
	}))
	defer origin.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		req, _ := http.NewRequestWithContext(r.Context(), http.MethodGet, origin.URL, nil)
		resp, err := origin.Client().Do(req)
		if err != nil {
			return
		}
		stream := trae.TranslateStream(resp.Body, "deepseek-v4.1-flash")
		defer stream.Close()
		writer := &chatContractWriter{inner: w}
		_ = upstream.Stream(writer, stream, upstream.StreamOptions{
			Model: "trae:deepseek-v4.1-flash", ReasoningLoopGuard: true,
			OnHold: writer.HoldProgress,
		})
	}))
	defer server.Close()
	defer close(release)
	client := &http.Client{Timeout: 2 * time.Second}
	start := time.Now()
	resp, err := client.Get(server.URL)
	if err != nil {
		t.Fatalf("guarded Trae Chat hid idle keepalive beyond the first-byte grace: %v", err)
	}
	defer resp.Body.Close()
	line, err := bufio.NewReader(resp.Body).ReadString('\n')
	if err != nil || !strings.HasPrefix(line, ":") {
		t.Fatalf("first frame is not an SSE keepalive: %q, %v", line, err)
	}
	if elapsed := time.Since(start); elapsed < chatHoldBackBlindWindow-100*time.Millisecond {
		t.Fatalf("first byte bypassed the clean-error grace: %s", elapsed)
	}
}
