// ═══ 更新日志 ═══
// 2026-09-25：只读观察每次真实HTTP尝试的起点和响应状态，不改变重试或发送内容。
package upstream

import (
	"context"
	"time"
)

type ChatAttemptEvent struct {
	Stage  string
	At     time.Time
	Status int
}

type chatAttemptObserverKey struct{}

func WithChatAttemptObserver(ctx context.Context, observe func(ChatAttemptEvent)) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, chatAttemptObserverKey{}, observe)
}

func observeChatAttempt(ctx context.Context, stage string, status int) {
	if observe, ok := ctx.Value(chatAttemptObserverKey{}).(func(ChatAttemptEvent)); ok && observe != nil {
		observe(ChatAttemptEvent{Stage: stage, At: time.Now(), Status: status})
	}
}
