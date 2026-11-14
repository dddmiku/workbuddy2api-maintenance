package server

import (
	"bufio"
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/upstream"
	"workbuddy2api/internal/upstream/trae"
)

// Real HTTP/2 client -> handler -> real HTTP origin. The origin waits beyond
// the production 30s downstream write budget before sending any response header.
// A tiny generic idle budget also proves that Trae never enters monitorBody.
func TestTraeLongHeaderSilenceKeepsHTTP2Stream(t *testing.T) {
	t.Setenv("WB2API_TRAE_REMOTE", "0")
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != trae.ChatPath {
			io.WriteString(w, "{}")
			return
		}
		select {
		case <-time.After(32 * time.Second):
		case <-r.Context().Done():
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "event: output\ndata: {\"response\":\"fixture\"}\n\nevent: done\ndata: {}\n\n")
	}))
	defer origin.Close()
	old := trae.GatewayBaseVar
	trae.GatewayBaseVar = origin.URL
	defer func() { trae.GatewayBaseVar = old }()
	acct, err := auth.Parse([]byte(`{"auth":{"realm":"trae","accessToken":"fixture","expiresAt":9999999999},"account":{"uid":"silence-fixture"}}`))
	if err != nil {
		t.Fatal(err)
	}
	up := upstream.New()
	up.IdleTimeout = 50 * time.Millisecond
	local := httptest.NewUnstartedServer(NewHandler(Config{Pool: testPoolWith(acct), Upstream: up}))
	local.EnableHTTP2 = true
	local.StartTLS()
	defer local.Close()
	client := local.Client()
	client.Timeout = 40 * time.Second
	started := time.Now()
	resp, err := client.Post(local.URL+"/v1/messages", "application/json", strings.NewReader(`{"model":"trae:deepseek-v4.1-flash","stream":true,"max_tokens":128,"messages":[{"role":"user","content":"fixture"}]}`))
	if err != nil {
		t.Fatal("stream closed while origin headers were pending")
	}
	defer resp.Body.Close()
	if resp.ProtoMajor != 2 {
		t.Fatal("fixture did not exercise HTTP/2")
	}
	reader := bufio.NewReader(resp.Body)
	first, err := reader.ReadString('\n')
	if err != nil || first == "" {
		t.Fatal("waiting stream had no progress")
	}
	firstElapsed := time.Since(started)
	data, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("connection lost mid-response after long silence: %T", err)
	}
	if bytes.Count(data, []byte("event: message_stop")) != 1 || !bytes.Contains(data, []byte("fixture")) {
		t.Fatal("long wait lost actual content or its single terminal event")
	}
	if firstElapsed > 3*time.Second || time.Since(started) < 32*time.Second {
		t.Fatal("fixture did not separate early keepalive from late model output")
	}
	t.Logf("client_first_progress_ms=%d header_silence_ms=32000 terminal=true protocol=HTTP/2 total_ms=%d", firstElapsed.Milliseconds(), time.Since(started).Milliseconds())
}
