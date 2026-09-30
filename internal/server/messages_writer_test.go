// ═══ 更新日志 ═══
// 2026-09-26：锁定首帧迟到时先开流与 ping，非流式不受影响。
package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// 上游请求已发出但迟迟没有首帧（循环保护压制期）时，客户端不应干等到超时：
// 网关先发 message_start，并周期性 ping。
//
// 写入器会在保活 goroutine 里写响应，测试不能并发读 httptest.ResponseRecorder
// （它不为并发设计），这里用带锁的包装读取。
func TestMessagesStreamStartsBeforeFirstUpstreamFrame(t *testing.T) {
	grace, ping := anthropicFirstFrameGrace, anthropicIdlePing
	anthropicFirstFrameGrace, anthropicIdlePing = 20*time.Millisecond, 20*time.Millisecond
	t.Cleanup(func() { anthropicFirstFrameGrace, anthropicIdlePing = grace, ping })

	sink := newLockedRecorder()
	writer := newMessagesWriter(sink)
	writer.model, writer.stream = "cn:fixture", true
	writer.UpstreamStarted()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		body := sink.body()
		if strings.Contains(body, "message_start") && strings.Contains(body, `"type":"ping"`) {
			writer.finish()
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	writer.finish()
	t.Fatalf("client saw nothing while waiting for the first frame: %q", sink.body())
}

// TestMessagesWriteIsSafeAgainstKeepAlive 覆盖「Write 与保活 goroutine 并发」这条路径。
//
// 上游请求已发出但首帧迟到时，UpstreamStarted 会起 keepAliveLoop；此后上游帧经
// 读 goroutine 进 Write，两条 goroutine 碰的是同一批字段（m.err / m.ended /
// m.usage / m.lastEvent）。修复前 Write 全程不加锁：m.usage 是 map，并发读
// （保活路径 beginLocked → anthropicUsage）与写（Write 里的 MergeUsage 赋值）
// 可能触发 Go 运行时的 "fatal error: concurrent map read and map write"；
// m.err 是接口值，撕裂读可能拿到损坏的类型指针。
//
// 局限（如实说明）：这是**冒烟测试**，不是竞态的判定性证据。真正重叠的窗口很窄
// （保活只在开流前读一次 m.usage），本用例在修复前后都可能通过。定论要靠服务器上
// 带 -race 跑同一路径（本机 Windows 无 gcc，-race 不可用）。修复后 Write 全程持锁、
// 内部只调 *Locked 变体，本用例稳定通过。
func TestMessagesWriteIsSafeAgainstKeepAlive(t *testing.T) {
	grace, ping := anthropicFirstFrameGrace, anthropicIdlePing
	anthropicFirstFrameGrace, anthropicIdlePing = time.Millisecond, time.Millisecond
	t.Cleanup(func() { anthropicFirstFrameGrace, anthropicIdlePing = grace, ping })

	sink := newLockedRecorder()
	writer := newMessagesWriter(sink)
	writer.model, writer.stream = "cn:fixture", true
	writer.UpstreamStarted()

	// 与保活 goroutine 并发地喂入带用量的帧：每帧都写 m.usage（map），
	// 保活路径的 beginLocked/anthropicUsage 会读同一个 map。
	frame := []byte(`data: {"choices":[{"index":0,"delta":{"content":"x"}}],"usage":{"prompt_tokens":1,"completion_tokens":1}}` + "\n\n")
	deadline := time.Now().Add(300 * time.Millisecond)
	for time.Now().Before(deadline) {
		if _, err := writer.Write(frame); err != nil {
			break
		}
	}
	writer.finish()
	if !strings.Contains(sink.body(), "message_start") {
		t.Fatalf("保活路径未开流：%q", sink.body())
	}
}

// lockedRecorder 给 ResponseRecorder 加锁：保活 goroutine 与测试读取并发访问。
type lockedRecorder struct {
	mu sync.Mutex
	rr *httptest.ResponseRecorder
}

func newLockedRecorder() *lockedRecorder {
	return &lockedRecorder{rr: httptest.NewRecorder()}
}

func (l *lockedRecorder) Header() http.Header {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.rr.Header()
}

func (l *lockedRecorder) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.rr.Write(p)
}

func (l *lockedRecorder) WriteHeader(code int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.rr.WriteHeader(code)
}

func (l *lockedRecorder) Flush() {}

func (l *lockedRecorder) body() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.rr.Body.String()
}

// 非流式请求不该被提前开流（否则状态码语义会变）。
func TestMessagesUpstreamStartedIgnoresNonStreamRequests(t *testing.T) {
	sink := newLockedRecorder()
	writer := newMessagesWriter(sink)
	writer.model, writer.stream = "cn:fixture", false
	writer.UpstreamStarted()
	time.Sleep(30 * time.Millisecond)
	writer.finish()
	// 非流式请求即使没有上游响应也不该出现 SSE：应当是普通 JSON 错误体。
	if strings.Contains(sink.body(), "message_start") || writer.started {
		t.Fatalf("non-stream request started a stream: %q", sink.body())
	}
}
