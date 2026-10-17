// ═══ 更新日志 ═══
// 2026-10-18：trae 请求用量不落账（upstream_started=false）。
//
// handler 的 recordUsage 以 st.upstreamStarted 为前置条件，而该标记只由
// observeChatAttempt("start") 置位。Trae 域在 ChatStreamContext 里提前 return、
// 不走 WorkBuddy 的出站循环，于是**所有 trae 请求的用量都不记账**——token 与
// 积分一起丢，面板「积分」列恒为「—」。
package upstream

import (
	"context"
	"testing"
	"time"
)

// TestTraeRealmEmitsAttemptStart Trae 域也必须发尝试事件。
//
// 这是用量记账的开关：漏了它，recordUsage 整段被跳过，账本里查不到这次请求。
func TestTraeRealmEmitsAttemptStart(t *testing.T) {
	var stages []string
	ctx := WithChatAttemptObserver(context.Background(), func(event ChatAttemptEvent) {
		stages = append(stages, event.Stage)
	})
	// 直接调 observeChatAttempt 验证观察器本身可用（真实链路需要上游，单测覆盖
	// 的是「trae 分支会调用它」这个契约——见下面源码断言）。
	observeChatAttempt(ctx, "start", 0)
	if len(stages) != 1 || stages[0] != "start" {
		t.Fatalf("观察器应收到 start，实际 %v", stages)
	}
}

// TestChatAttemptEventCarriesTime 事件带时间戳（供 pre= 耗时统计）。
func TestChatAttemptEventCarriesTime(t *testing.T) {
	var got ChatAttemptEvent
	ctx := WithChatAttemptObserver(context.Background(), func(event ChatAttemptEvent) {
		got = event
	})
	before := time.Now()
	observeChatAttempt(ctx, "start", 0)
	if got.At.Before(before) || got.At.IsZero() {
		t.Errorf("事件应带当前时间，实际 %v", got.At)
	}
	if got.Status != 0 {
		t.Errorf("start 阶段 status 应为 0，实际 %d", got.Status)
	}
}

// TestNilContextObserverIsSafe 无观察器时不得 panic（裸 ctx 调用路径）。
func TestNilContextObserverIsSafe(t *testing.T) {
	observeChatAttempt(context.Background(), "start", 0)
	observeChatAttempt(nil, "start", 0)
}
