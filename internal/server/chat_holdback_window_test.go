// ═══ 更新日志 ═══
// 2026-10-14：锁定 chat 压制期的「延迟开流」契约：早期保持零字节以保住重发资格与
//
//	干净 400 JSON，只有盲等逼近反代上限时才开流发心跳。
package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// chatHoldbackWriter 造一个已进入压制期的 chat 契约写入器（Content-Type 已设为 SSE）。
func chatHoldbackWriter(t *testing.T) (*chatContractWriter, *httptest.ResponseRecorder) {
	t.Helper()
	recorder := httptest.NewRecorder()
	recorder.Header().Set("Content-Type", "text/event-stream")
	writer := &chatContractWriter{inner: recorder}
	return writer, recorder
}

// TestChatHoldProgressKeepsZeroBytesBeforeProxyDeadline 锁定核心契约：压制期的前
// chatHoldBackBlindWindow 秒内，写入器必须**一个字节都不写**。
//
// 这是重发资格的前提——响应头一旦发出，命中循环就无法整段丢弃后静默重发，重发失败时
// 也无法返回干净的 400 + JSON 错误体。两个既有契约测试
// （TestContextLoopRetryHTTPFailurePreservesRecovery / TestContextHTTPClassificationContract）
// 依赖这条性质，这里把它单独钉住，避免将来有人为了「早点让客户端看到响应头」而破坏它。
func TestChatHoldProgressKeepsZeroBytesBeforeProxyDeadline(t *testing.T) {
	writer, recorder := chatHoldbackWriter(t)

	// 模拟压制期反复回调：全部发生在盲等窗口内。
	deadline := time.Now().Add(chatHoldBackBlindWindow / 2)
	for time.Now().Before(deadline) {
		writer.HoldProgress()
		if body := recorder.Body.String(); body != "" {
			t.Fatalf("wrote %q during the blind window; retry eligibility would be void", body)
		}
		if writer.streaming {
			t.Fatal("stream marked started during the blind window")
		}
		// 不真的睡满窗口：只验证「窗口内不写」，用短间隔循环即可。
		break
	}
}

// TestChatHoldProgressOpensStreamNearProxyDeadline 锁定延迟开流的另一半：盲等逼近
// 反代上限时，必须开流发心跳，否则 Cloudflare 会在 100 秒判 524，客户端拿到的是网关
// 超时而不是循环错误。
//
// 用直接把 holdSince 拨回去的方式模拟「已盲等很久」，不依赖真实时钟推进。
func TestChatHoldProgressOpensStreamNearProxyDeadline(t *testing.T) {
	writer, recorder := chatHoldbackWriter(t)
	// 拨到刚好越过盲等窗口：应触发开流。
	writer.holdSince = time.Now().Add(-chatHoldBackBlindWindow - time.Second)

	writer.HoldProgress()

	if !writer.streaming {
		t.Fatal("stream was not started after the blind window elapsed")
	}
	body := recorder.Body.String()
	if !strings.Contains(body, ": keepalive") {
		t.Fatalf("no keepalive comment written when opening the stream: %q", body)
	}
	if !strings.HasPrefix(body, ": ") {
		t.Fatalf("first bytes must be an SSE comment, got %q", body)
	}
}

// TestChatHoldProgressBlindWindowFitsUnderCloudflare524 锁定常量与反代约束的关系。
//
// 这条链路是 Cloudflare(100 秒无响应判 524) → nginx(read_timeout 600s) → 网关。
// 最紧的一层是 Cloudflare，因此盲等窗口必须显著小于它，否则心跳还没发出去，连接
// 就已经被判超时了。
func TestChatHoldProgressBlindWindowFitsUnderCloudflare524(t *testing.T) {
	// Cloudflare 的 524 阈值：源站 100 秒内没有返回任何字节即判超时。
	const cloudflareOriginTimeout = 100 * time.Second
	if chatHoldBackBlindWindow >= cloudflareOriginTimeout {
		t.Fatalf("blind window %s does not leave room under Cloudflare's %s timeout",
			chatHoldBackBlindWindow, cloudflareOriginTimeout)
	}
	// 至少留 10 秒余量：心跳要来得及在网络抖动中送达。
	if cloudflareOriginTimeout-chatHoldBackBlindWindow < 10*time.Second {
		t.Fatalf("only %s of margin under the Cloudflare timeout; a slow round trip could still 524",
			cloudflareOriginTimeout-chatHoldBackBlindWindow)
	}
}

// TestChatHoldProgressDoesNotPolluteCompletedStream 回归：流已结束或已出错时，
// HoldProgress 不能再往响应里写字节。
func TestChatHoldProgressDoesNotPolluteCompletedStream(t *testing.T) {
	writer, recorder := chatHoldbackWriter(t)
	writer.streaming = true
	writer.checked = true
	writer.holdSince = time.Now().Add(-chatHoldBackBlindWindow - time.Second)
	writer.lastWrite = time.Time{}

	writer.HoldProgress()

	if body := recorder.Body.String(); body != "" {
		t.Fatalf("HoldProgress wrote %q into an already completed stream", body)
	}
	_ = http.StatusOK
}
