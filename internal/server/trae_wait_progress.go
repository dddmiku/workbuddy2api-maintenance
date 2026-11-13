package server

import (
	"context"
	"io"
	"net/http"
	"time"

	"workbuddy2api/internal/upstream"
)

type pendingChatResult struct {
	body     io.ReadCloser
	status   int
	response []byte
	err      error
}

// waitChatWithProgress covers catalog/create/response-header waiting. Only the
// caller goroutine touches protocol writers or accounting observers. Actual
// HTTP status, body and errors are returned without reinterpretation.
func waitChatWithProgress(ctx context.Context, w http.ResponseWriter, call func(context.Context) (io.ReadCloser, int, []byte, error)) (io.ReadCloser, int, []byte, error) {
	startWaitingProgress(w)
	callbacks := make(chan func(), 16)
	workerCtx := upstream.DispatchChatObservers(ctx, func(f func()) {
		select {
		case callbacks <- f:
		case <-ctx.Done():
		}
	})
	ready := make(chan pendingChatResult)
	go func() {
		r := pendingChatResult{}
		r.body, r.status, r.response, r.err = call(workerCtx)
		select {
		case ready <- r:
		case <-ctx.Done():
			if r.body != nil {
				_ = r.body.Close()
			}
		}
	}()
	beat := time.NewTicker(chatHoldBackBlindWindow)
	defer beat.Stop()
	drain := func() {
		for {
			select {
			case f := <-callbacks:
				f()
			default:
				return
			}
		}
	}
	for {
		select {
		case f := <-callbacks:
			f()
		case r := <-ready:
			drain()
			return r.body, r.status, r.response, r.err
		case <-beat.C:
			holdProgress(w)
		case <-ctx.Done():
			drain()
			return nil, 0, nil, ctx.Err()
		}
	}
}

// Chat's clean-error grace starts at setup entry. Other protocol adapters open
// their stream only on the first timer tick and keep their existing policies.
func startWaitingProgress(w http.ResponseWriter) {
	for current := w; current != nil; {
		if chat, ok := current.(*chatContractWriter); ok {
			if !chat.streaming && chat.holdSince.IsZero() {
				chat.holdSince = time.Now()
			}
			return
		}
		next, ok := current.(interface{ Unwrap() http.ResponseWriter })
		if !ok {
			return
		}
		current = next.Unwrap()
	}
}
