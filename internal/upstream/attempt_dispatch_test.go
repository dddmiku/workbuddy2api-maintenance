package upstream

import (
	"context"
	"testing"
)

func TestDispatchChatObserversSerializesAndCopies(t *testing.T) {
	var stages []string
	ctx := WithChatAttemptObserver(context.Background(), func(e ChatAttemptEvent) { stages = append(stages, e.Stage) })
	ctx = WithChatRetryObserver(ctx, func(body []byte) {
		if string(body) != "fixture" {
			t.Error("retry observer retained mutable source buffer")
		}
		stages = append(stages, "retry")
	})
	queue := make(chan func(), 3)
	ctx = DispatchChatObservers(ctx, func(f func()) { queue <- f })
	completed := make(chan struct{})
	go func() {
		defer close(completed)
		observeChatAttempt(ctx, "start", 0)
		body := []byte("fixture")
		observeChatRetry(ctx, body)
		body[0] = 'x'
		observeChatAttempt(ctx, "headers", 200)
	}()
	<-completed
	if len(stages) != 0 {
		t.Fatal("worker touched caller-owned observer state")
	}
	for range 3 {
		(<-queue)()
	}
	if len(stages) != 3 || stages[0] != "start" || stages[1] != "retry" || stages[2] != "headers" {
		t.Fatal("observer ordering changed")
	}
}
