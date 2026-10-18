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

// observeChatAttempt 只读地广播一次尝试事件。
//
// ctx 必须判空：ChatStreamContext 的若干调用路径（测试、裸上游调用）会传 nil ctx，
// 而 ctx.Value 在 nil 接口上直接 panic（attempt_20261018_test.go 的
// TestNilContextObserverIsSafe 锁的就是这条）。
func observeChatAttempt(ctx context.Context, stage string, status int) {
	if ctx == nil {
		return
	}
	if observe, ok := ctx.Value(chatAttemptObserverKey{}).(func(ChatAttemptEvent)); ok && observe != nil {
		observe(ChatAttemptEvent{Stage: stage, At: time.Now(), Status: status})
	}
}
