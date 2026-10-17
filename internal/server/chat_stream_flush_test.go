// ═══ 更新日志 ═══
// 2026-10-17：锁定 OpenAI Chat 流式内容帧必须立即 flush，避免 net/http 缓冲把
// 客户端首字节推迟到缓冲填满或收尾（慢流会等到 5s 心跳）。
package server

import (
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// flushCountingRecorder 记录 Flush 次数；ResponseController 优先找 FlushError，
// 没有时退回 http.Flusher，这里的 Flush 就是被调用的入口。
type flushCountingRecorder struct {
	*httptest.ResponseRecorder
	flushes int
}

func (r *flushCountingRecorder) Flush() {
	r.flushes++
	r.ResponseRecorder.Flush()
}

func TestChatStreamFlushesContentImmediately(t *testing.T) {
	rec := &flushCountingRecorder{ResponseRecorder: httptest.NewRecorder()}
	w := &chatContractWriter{inner: rec, lastWrite: time.Now()}
	w.Header().Set("Content-Type", "text/event-stream")

	// 纯角色帧不是客户端可见内容：不能 flush，否则响应头已发出，
	// 「零内容时换号干净重试」的契约就破了。
	role := `data: {"choices":[{"index":0,"delta":{"role":"assistant"}}]}` + "\n\n"
	if _, err := w.Write([]byte(role)); err != nil {
		t.Fatalf("write role frame: %v", err)
	}
	if rec.flushes != 0 {
		t.Fatalf("角色帧不应触发 flush，got %d 次", rec.flushes)
	}

	// 正文帧：必须立刻 flush，否则 net/http 的响应缓冲会把它攒到
	// 缓冲填满（慢流则是 5s 心跳或收尾）才发给客户端。
	content := `data: {"choices":[{"index":0,"delta":{"content":"你好"}}]}` + "\n\n"
	if _, err := w.Write([]byte(content)); err != nil {
		t.Fatalf("write content frame: %v", err)
	}
	if rec.flushes == 0 {
		t.Fatal("内容帧写出后必须立即 flush")
	}
	if body := rec.Body.String(); !strings.Contains(body, `"content":"你好"`) {
		t.Fatalf("正文未写出：%s", body)
	}
}
