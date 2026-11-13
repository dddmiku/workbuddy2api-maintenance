package server

import (
	"bufio"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/upstream"
	"workbuddy2api/internal/upstream/trae"
)

// Actual handler -> Trae catalog/create/events. The create response is kept
// pending until the client receives progress, so only a pre-header heartbeat
// can satisfy this test. A translator-only heartbeat cannot pass it.
func TestTraeCreateWaitProgressBeforeHeaders(t *testing.T) {
	release := make(chan struct{})
	var once sync.Once
	finish := func() { once.Do(func() { close(release) }) }
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/chat_sessions":
			select {
			case <-release:
			case <-r.Context().Done():
				return
			}
			io.WriteString(w, `{"code":0,"data":{"chat_session_id":"fixture-session","message_id":"fixture-message"}}`)
		case strings.Contains(r.URL.Path, "/events"):
			w.Header().Set("Content-Type", "text/event-stream")
			io.WriteString(w, "event: plan_item\ndata: {\"content\":\"ok\"}\n\nevent: done\ndata: {\"status\":\"completed\"}\n\n")
		default:
			io.WriteString(w, "{}")
		}
	}))
	defer origin.Close()
	oldGateway, oldRemote := trae.GatewayBaseVar, trae.RemoteBaseVar
	trae.GatewayBaseVar, trae.RemoteBaseVar = origin.URL, origin.URL
	defer func() { trae.GatewayBaseVar, trae.RemoteBaseVar = oldGateway, oldRemote }()
	trae.InvalidateCatalog("fixture-wait-account")
	defer trae.InvalidateCatalog("fixture-wait-account")
	acct, err := auth.Parse([]byte(`{"auth":{"accessToken":"fixture","realm":"trae","expiresAt":9999999999},"account":{"uid":"fixture-wait-account"}}`))
	if err != nil {
		t.Fatal(err)
	}
	h := NewHandler(Config{Pool: testPoolWith(acct), Upstream: upstream.New()})
	local := httptest.NewServer(h)
	defer local.Close()
	defer finish()
	client := &http.Client{Timeout: 1800 * time.Millisecond}
	start := time.Now()
	resp, err := client.Post(local.URL+"/v1/chat/completions", "application/json", strings.NewReader(`{"model":"trae:glm-5.3","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatalf("Trae create wait hid all client progress: %v", err)
	}
	defer resp.Body.Close()
	line, err := bufio.NewReader(resp.Body).ReadString('\n')
	if err != nil || !strings.HasPrefix(line, ":") {
		t.Fatal("pending create did not emit an SSE comment")
	}
	if time.Since(start) < chatHoldBackBlindWindow-100*time.Millisecond {
		t.Fatal("progress bypassed clean-error grace")
	}
	finish()
}

type lateChatBody struct {
	closed chan struct{}
	calls  atomic.Int32
}

func (b *lateChatBody) Read([]byte) (int, error) { return 0, io.EOF }
func (b *lateChatBody) Close() error {
	if b.calls.Add(1) == 1 {
		close(b.closed)
	}
	return nil
}

func TestTraeWaitCancellationClosesLateBody(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	started, release := make(chan struct{}), make(chan struct{})
	body := &lateChatBody{closed: make(chan struct{})}
	returned := make(chan error, 1)
	go func() {
		_, _, _, err := waitChatWithProgress(ctx, &chatContractWriter{inner: httptest.NewRecorder()}, func(context.Context) (io.ReadCloser, int, []byte, error) {
			close(started)
			<-release
			return body, 200, nil, nil
		})
		returned <- err
	}()
	<-started
	cancel()
	select {
	case err := <-returned:
		if err != context.Canceled {
			t.Fatal("cancellation cause changed")
		}
	case <-time.After(time.Second):
		t.Fatal("canceled wait did not return")
	}
	close(release)
	select {
	case <-body.closed:
	case <-time.After(time.Second):
		t.Fatal("late response body leaked after caller cancellation")
	}
	if body.calls.Load() != 1 {
		t.Fatal("late body was closed more than once")
	}
}

func TestTraeFastSetupFailureKeepsCleanResponse(t *testing.T) {
	recorder := httptest.NewRecorder()
	w := &chatContractWriter{inner: recorder}
	want := []byte(`{"code":3004}`)
	_, status, body, err := waitChatWithProgress(context.Background(), w, func(context.Context) (io.ReadCloser, int, []byte, error) { return nil, 400, want, nil })
	if err != nil || status != 400 || string(body) != string(want) || recorder.Body.Len() != 0 || w.streaming {
		t.Fatal("fast setup failure lost its original status/body or clean-response grace")
	}
}
