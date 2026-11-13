package trae

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"strings"
	"sync"
	"testing"
	"time"

	"workbuddy2api/internal/upstreamerr"
)

func TestChatTransportReusesConnections(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "ok")
	}))
	defer srv.Close()
	for i := 0; i < 2; i++ {
		transport := chatTransport().(*http.Transport)
		defer transport.CloseIdleConnections()
		reused := false
		ctx := httptrace.WithClientTrace(context.Background(), &httptrace.ClientTrace{
			GotConn: func(info httptrace.GotConnInfo) { reused = info.Reused },
		})
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, nil)
		resp, err := (&http.Client{Transport: transport}).Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		if i == 1 && !reused {
			t.Fatal("successive Trae calls opened a new connection instead of reusing the pool")
		}
	}
}

func TestChatTransportHTTP2CancelIsolated(t *testing.T) {
	canceled := make(chan struct{})
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/waiting" {
			w.Header().Set("Content-Type", "text/event-stream")
			w.(http.Flusher).Flush()
			<-r.Context().Done()
			close(canceled)
			return
		}
		_, _ = io.WriteString(w, r.Header.Get("X-Test-State"))
	}))
	srv.EnableHTTP2 = true
	srv.StartTLS()
	defer srv.Close()
	transport := chatTransport().(*http.Transport).Clone()
	transport.TLSClientConfig = srv.Client().Transport.(*http.Transport).TLSClientConfig.Clone()
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 2 * time.Second}
	waiting, err := client.Get(srv.URL + "/waiting")
	if err != nil {
		t.Fatal(err)
	}
	defer waiting.Body.Close()
	if waiting.ProtoMajor != 2 {
		t.Fatalf("expected HTTP/2, got %s", waiting.Proto)
	}
	for _, state := range []string{"first-request", "second-request"} {
		reused := false
		ctx := httptrace.WithClientTrace(context.Background(), &httptrace.ClientTrace{
			GotConn: func(info httptrace.GotConnInfo) { reused = info.Reused },
		})
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, nil)
		req.Header.Set("X-Test-State", state)
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		if state == "first-request" {
			_ = waiting.Body.Close()
		}
		data, err := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if err != nil || string(data) != state || !reused {
			t.Fatalf("shared HTTP/2 state/cancellation leaked: body=%q reused=%v err=%v", data, reused, err)
		}
	}
	select {
	case <-canceled:
	case <-time.After(time.Second):
		t.Fatal("closing the waiting stream did not cancel its upstream request")
	}
}

// Real single-attempt successes can have TTFB above 120s. Until header timing
// is measured separately, retain room for a successful delayed response.
func TestTraeHeaderWaitAllowsSlowSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(220 * time.Millisecond):
			_, _ = io.WriteString(w, "late headers")
		}
	}))
	defer srv.Close()
	transport := chatTransport().(*http.Transport).Clone()
	defer transport.CloseIdleConnections()
	// Scale seconds to milliseconds while retaining the production wait policy.
	transport.ResponseHeaderTimeout /= 1000
	resp, err := (&http.Client{Transport: transport}).Get(srv.URL)
	if err != nil {
		t.Fatalf("Trae header budget killed a delayed successful response: %v", err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil || string(data) != "late headers" {
		t.Fatalf("delayed successful response was lost: body=%q err=%v", data, err)
	}
}

func TestTraeHeaderWaitBudget(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer srv.Close()
	transport := chatTransport().(*http.Transport).Clone()
	defer transport.CloseIdleConnections()
	transport.ResponseHeaderTimeout /= 1000
	resp, err := (&http.Client{Transport: transport, Timeout: time.Second}).Get(srv.URL)
	if resp != nil {
		_ = resp.Body.Close()
	}
	if err == nil || !strings.Contains(err.Error(), "timeout awaiting response headers") {
		t.Fatalf("stalled headers did not hit the transport budget: %v", err)
	}
}

func TestTraeHeaderBudgetDoesNotLimitStream(t *testing.T) {
	transport := chatTransport().(*http.Transport).Clone()
	defer transport.CloseIdleConnections()
	transport.ResponseHeaderTimeout /= 1000
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.(http.Flusher).Flush()
		time.Sleep(2 * transport.ResponseHeaderTimeout)
		_, _ = io.WriteString(w, "event: output\ndata: {\"response\":\"ok\"}\n\nevent: done\ndata: {}\n\n")
	}))
	defer srv.Close()
	resp, err := (&http.Client{Transport: transport}).Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil || !strings.Contains(string(data), "event: done") {
		t.Fatalf("header budget interrupted an established stream: %v", err)
	}
}

// Exercise actual net/http bodies and the Remote io.Pipe, rather than a fake
// body exposing SetReadDeadline (which production HTTP bodies do not expose).
func silentHTTPStream(t *testing.T, remote bool) (io.ReadCloser, func()) {
	t.Helper()
	release := make(chan struct{})
	var once sync.Once
	finish := func() { once.Do(func() { close(release) }) }
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.(http.Flusher).Flush()
		select {
		case <-r.Context().Done():
		case <-release:
			_, _ = io.WriteString(w, "event: output\ndata: {\"response\":\"ok\"}\n\nevent: done\ndata: {}\n\n")
		}
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(finish)
	resp, err := srv.Client().Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	var body io.ReadCloser = resp.Body
	if remote {
		body = translateRemoteEvents(newRemoteFirstEventReader(body), "deepseek-v4.1-flash", "test-session")
	}
	var stream io.ReadCloser = TranslateStream(body, "deepseek-v4.1-flash")
	if !remote {
		// The production legacy path wraps the translator in the 3004 gate.
		stream = newRemoteFallbackReader(context.Background(), nil, nil, "", stream, false)
	}
	t.Cleanup(func() {
		finish()
		_ = resp.Body.Close()
		_ = stream.Close()
	})
	return stream, finish
}

func TestTranslateHTTPIdleHeartbeat(t *testing.T) {
	for _, remote := range []bool{false, true} {
		name := "legacy"
		if remote {
			name = "remote"
		}
		t.Run(name, func(t *testing.T) {
			stream, finish := silentHTTPStream(t, remote)
			first := make(chan string, 1)
			go func() {
				buf := make([]byte, 256)
				n, _ := stream.Read(buf)
				first <- string(buf[:n])
			}()
			select {
			case frame := <-first:
				if !strings.HasPrefix(frame, ":") || !strings.HasSuffix(frame, "\n\n") {
					t.Fatalf("idle progress must be a complete SSE comment, got %q", frame)
				}
			case <-time.After(1500 * time.Millisecond):
				t.Fatal("silent HTTP stream produced no first-byte keepalive within 1.5s")
			}
			finish()
			data, err := io.ReadAll(stream)
			if err != nil || contentOf(t, string(data)) != "ok" || strings.Count(string(data), "data: [DONE]") != 1 {
				t.Fatalf("idle keepalive changed the real answer/terminal: %q, %v", data, err)
			}
		})
	}
}

func TestTranslateCloseUnblocksSilentHTTP(t *testing.T) {
	for _, remote := range []bool{false, true} {
		name := "legacy"
		if remote {
			name = "remote"
		}
		t.Run(name, func(t *testing.T) {
			stream, _ := silentHTTPStream(t, remote)
			closed := make(chan struct{})
			go func() { _ = stream.Close(); close(closed) }()
			select {
			case <-closed:
			case <-time.After(200 * time.Millisecond):
				t.Fatal("closing a silent stream did not cancel the blocked upstream read")
			}
		})
	}
}

func TestRemoteFallbackKeepalivePreserves3004Retry(t *testing.T) {
	for _, size := range []int{4096, 10, 3} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			testRemoteFallbackKeepalivePreserves3004Retry(t, size)
		})
	}
}

func testRemoteFallbackKeepalivePreserves3004Retry(t *testing.T, size int) {
	t.Helper()
	stub := &remoteStub{
		createBody: `{"code":0,"data":{"chat_session_id":"sid-progress","message_id":"mid-progress"}}`,
		eventsBody: "event: plan_item\ndata: {\"content\":\"REMOTE-ANSWER\"}\n\n" +
			"event: done\ndata: {\"status\":\"completed\"}\n\n",
	}
	srv := stub.start(t)
	old := RemoteBaseVar
	oldGateway := GatewayBaseVar
	RemoteBaseVar = srv.URL
	GatewayBaseVar = srv.URL
	t.Cleanup(func() { RemoteBaseVar, GatewayBaseVar = old, oldGateway })
	role := `data: {"choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}` + "\n\n"
	comment := ": relay-keepalive\n\n"
	primary := newStubReadCloser(role+comment, &upstreamerr.Error{Code: 3004, Message: "exceeded the rate limit"})
	prepared := []byte(`{"model":"glm-5.3","messages":[{"role":"user","content":"hi"}]}`)
	r := newRemoteFallbackReader(context.Background(), testAuth(), prepared, "", primary, false)
	defer r.Close()
	buf := make([]byte, size)
	var first strings.Builder
	for first.Len() < len(comment) {
		n, err := r.Read(buf)
		if err != nil {
			t.Fatalf("fallback gate hid the comment: %q, %v", first.String(), err)
		}
		first.Write(buf[:n])
	}
	if first.String() != comment {
		t.Fatalf("fallback gate released the role frame: %q", first.String())
	}
	if r.sawContent || r.released || stub.createCalls != 0 {
		t.Fatal("keepalive changed content state or triggered an early retry")
	}
	data, err := io.ReadAll(r)
	if err != nil || !strings.Contains(string(data), "REMOTE-ANSWER") || stub.createCalls != 1 {
		t.Fatalf("keepalive prevented the later 3004 retry: %q, %v", data, err)
	}
	if strings.Count(string(data), `"role":"assistant"`) != 1 {
		t.Fatalf("fallback leaked a duplicate role frame: %q", data)
	}
}

func TestRemoteFallbackFragmentedContentPreventsRetry(t *testing.T) {
	for _, size := range []int{10, 3} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			frame := `data: {"choices":[{"index":0,"delta":{"content":"partial"}}]}` + "\n\n"
			failure := &upstreamerr.Error{Code: 3004, Message: "rate limit"}
			primary := newStubReadCloser(frame, failure)
			r := newRemoteFallbackReader(context.Background(), nil, nil, "", primary, false)
			defer r.Close()
			var got strings.Builder
			buf := make([]byte, size)
			for got.Len() < len(frame) {
				n, err := r.Read(buf)
				if err != nil {
					t.Fatalf("content was lost before release: %v", err)
				}
				got.Write(buf[:n])
			}
			if got.String() != frame || !r.sawContent || !r.released {
				t.Fatalf("fragmented content must prevent fallback: %q", got.String())
			}
			if _, err := r.Read(buf); err != failure {
				t.Fatalf("original error was replaced after content: %v", err)
			}
		})
	}
}

func TestRemoteFallbackKeepaliveSmallReads(t *testing.T) {
	comment := ": relay-keepalive\n\n"
	r := newRemoteFallbackReader(context.Background(), nil, nil, "", io.NopCloser(strings.NewReader(comment)), false)
	defer r.Close()
	var got strings.Builder
	buf := make([]byte, 3)
	for {
		n, err := r.Read(buf)
		got.Write(buf[:n])
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	if got.String() != comment || r.sawContent || r.released {
		t.Fatalf("fragmented keepalive was lost or treated as content: %q", got.String())
	}
}

func TestRemoteFallbackDrainsHeldFramesAndPreservesError(t *testing.T) {
	frame := `data: {"choices":[{"index":0,"delta":{"role":"assistant"}}]}` + "\n\n"
	for _, failure := range []error{io.EOF, errors.New("test upstream read failure")} {
		for _, withData := range []bool{false, true} {
			t.Run(fmt.Sprintf("%v/withData=%v", failure, withData), func(t *testing.T) {
				primary := &terminalRemoteBody{Reader: strings.NewReader(frame), err: failure, withData: withData}
				r := newRemoteFallbackReader(context.Background(), nil, nil, "", primary, false)
				defer r.Close()
				var got strings.Builder
				buf := make([]byte, 3)
				var final error
				for i := 0; i < len(frame)+1; i++ {
					n, err := r.Read(buf)
					got.Write(buf[:n])
					if err != nil {
						final = err
						break
					}
				}
				if got.String() != frame || final != failure {
					t.Fatalf("held frames/error were lost: data=%q err=%v", got.String(), final)
				}
			})
		}
	}
}

type terminalRemoteBody struct {
	*strings.Reader
	err      error
	withData bool
}

func (b *terminalRemoteBody) Read(p []byte) (int, error) {
	n, err := b.Reader.Read(p)
	if err == io.EOF || (b.withData && b.Reader.Len() == 0) {
		err = b.err
	}
	return n, err
}

func (b *terminalRemoteBody) Close() error { return nil }
