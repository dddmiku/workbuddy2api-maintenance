package trae

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

type countedRemoteBody struct {
	io.Reader
	calls atomic.Int32
	err   error
}

func (b *countedRemoteBody) Close() error {
	if b.calls.Add(1) == 1 {
		return b.err
	}
	return nil
}

func TestRemoteEventCloseOnceAfterEOF(t *testing.T) {
	closeErr := errors.New("test upstream close failure")
	body := &countedRemoteBody{
		Reader: strings.NewReader("event: done\ndata: {\"status\":\"completed\"}\n\n"),
		err:    closeErr,
	}
	r := translateRemoteEvents(body, "glm-5.3", "test-session")
	if _, err := io.ReadAll(r); err != nil {
		t.Fatal(err)
	}
	// EOF already closed the source. Concurrent callers must share that same
	// close operation and its error instead of overwriting it with a second Close.
	errs := make(chan error, 16)
	var wg sync.WaitGroup
	for range 16 {
		wg.Go(func() { errs <- r.Close() })
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if !errors.Is(err, closeErr) {
			t.Errorf("upstream Close error was lost: %v", err)
		}
	}
	if got := body.calls.Load(); got != 1 {
		t.Fatalf("upstream Close called %d times, want 1", got)
	}
}

func TestRemoteSessionCloseStopsOnce(t *testing.T) {
	var stops atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		stops.Add(1)
		_, _ = io.WriteString(w, "{}")
	}))
	defer srv.Close()
	old := RemoteBaseVar
	RemoteBaseVar = srv.URL
	defer func() { RemoteBaseVar = old }()
	closeErr := errors.New("fixture close failure")
	body := &countedRemoteBody{Reader: strings.NewReader(""), err: closeErr}
	r := &remoteStreamCloser{ReadCloser: body, auth: testAuth(), session: remoteSession{id: "test-session"}}
	var wg sync.WaitGroup
	results := make(chan error, 8)
	for range 8 {
		wg.Go(func() { results <- r.Close() })
	}
	wg.Wait()
	close(results)
	for err := range results {
		if !errors.Is(err, closeErr) {
			t.Error("repeated Remote session Close lost its initial error")
		}
	}
	if stops.Load() != 1 || body.calls.Load() != 1 {
		t.Fatal("repeated Remote session Close performed duplicate cleanup")
	}
}
