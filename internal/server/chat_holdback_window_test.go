// ═══ 更新日志 ═══
// 2026-10-14：锁定 chat 压制期的「延迟开流」契约：早期保持零字节以保住重发资格与
//
//	干净 400 JSON，只有盲等逼近反代上限时才开流发心跳。
//
// 2026-10-17：盲等窗口从 85 秒降到 1 秒。压制期首字节现在被限制在 1 秒内（此前
//
//	deepseek 推理请求实测被推迟 1.7–2.7 秒），快速命中循环的干净 400 仍由 1 秒
//	零字节宽限保住；1 秒后命中循环的最终失败改为流内 error 帧，重试资格不变。
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

// TestChatHoldProgressKeepsZeroBytesWithinFirstByteGrace 锁定核心契约：压制期的前
// chatHoldBackBlindWindow 秒内，写入器必须**一个字节都不写**。
//
// 这是「快速命中循环时返回干净 400 + JSON」的前提——响应头一旦发出，失败就只能按
// SSE 帧交付。两个既有契约测试（TestContextLoopRetryHTTPFailurePreservesRecovery /
// TestContextHTTPClassificationContract）依赖这条性质；它们模拟的循环都在毫秒级
// 判定，远早于宽限窗口，因此行为不变。
func TestChatHoldProgressKeepsZeroBytesWithinFirstByteGrace(t *testing.T) {
	writer, recorder := chatHoldbackWriter(t)

	// 把起点拨到窗口内（剩余一半），不依赖真实时钟推进。
	writer.holdSince = time.Now().Add(-chatHoldBackBlindWindow / 2)
	writer.HoldProgress()

	if body := recorder.Body.String(); body != "" {
		t.Fatalf("wrote %q during the first-byte grace; clean-400 contract would be void", body)
	}
	if writer.streaming {
		t.Fatal("stream marked started during the first-byte grace")
	}
}

// TestChatHoldProgressOpensStreamAfterFirstByteGrace 锁定另一半：宽限一到就必须开流
// 发心跳，让客户端拿到首字节。此前窗口是 85 秒，只考虑了 Cloudflare 的 100 秒 524，
// 客户端在整段压制期收不到任何字节。
func TestChatHoldProgressOpensStreamAfterFirstByteGrace(t *testing.T) {
	writer, recorder := chatHoldbackWriter(t)
	// 拨到刚好越过宽限窗口：应触发开流。
	writer.holdSince = time.Now().Add(-chatHoldBackBlindWindow - time.Second)

	writer.HoldProgress()

	if !writer.streaming {
		t.Fatal("stream was not started after the first-byte grace elapsed")
	}
	body := recorder.Body.String()
	if !strings.Contains(body, ": keepalive") {
		t.Fatalf("no keepalive comment written when opening the stream: %q", body)
	}
	if !strings.HasPrefix(body, ": ") {
		t.Fatalf("first bytes must be an SSE comment, got %q", body)
	}
}

// TestChatHoldProgressFirstByteGraceBoundsClientTTFB 钉住取值意图：宽限必须足够短，
// 否则客户端感知的 TTFB 会被压制期拖长——这正是 2026-10-17 实测的问题
// （网关日志 TTFB=1032ms，客户端首字节 2731ms）。2 秒是体验上限，1 秒与 Anthropic
// 路径的 anthropicFirstFrameGrace 对齐。
func TestChatHoldProgressFirstByteGraceBoundsClientTTFB(t *testing.T) {
	const maxClientTTFBDelay = 2 * time.Second
	if chatHoldBackBlindWindow > maxClientTTFBDelay {
		t.Fatalf("first-byte grace %s exceeds the %s client-TTFB budget",
			chatHoldBackBlindWindow, maxClientTTFBDelay)
	}
	if chatHoldBackBlindWindow > anthropicFirstFrameGrace {
		t.Fatalf("chat first-byte grace %s is longer than the Anthropic grace %s; keep the two paths aligned",
			chatHoldBackBlindWindow, anthropicFirstFrameGrace)
	}
}

// TestChatHoldProgressBlindWindowFitsUnderCloudflare524 锁定常量与反代约束的关系。
//
// 这条链路是 Cloudflare(100 秒无响应判 524) → nginx(read_timeout 600s) → 网关。
// 最紧的一层是 Cloudflare，因此开流前的零字节时长必须显著小于它，否则心跳还没发出去，
// 连接就已经被判超时了。
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

// TestChatHoldProgressKeepaliveReachesClientAfterGrace 端到端（真实 HTTP 连接）验证：
// 压制期零字节宽限过后，客户端能在 1 秒量级收到第一个字节，而不是等推理结束。
//
// 这是 2026-10-17 那次修复的直接验收：此前窗口 85 秒，deepseek 推理请求的客户端
// 首字节被推迟 1.7–2.7 秒。这里不模拟客户端解析 SSE，只量第一个字节的到达时刻。
func TestChatHoldProgressKeepaliveReachesClientAfterGrace(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		writer := &chatContractWriter{inner: w}
		writer.HoldProgress() // 起始：零字节宽限开始计时
		time.Sleep(chatHoldBackBlindWindow + 200*time.Millisecond)
		writer.HoldProgress() // 超过宽限：写 keepalive 并 flush
	}))
	defer server.Close()

	requestStart := time.Now()
	response, err := http.Get(server.URL)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer response.Body.Close()
	buf := make([]byte, 32)
	n, err := response.Body.Read(buf)
	if err != nil && n == 0 {
		t.Fatalf("read first byte: %v", err)
	}
	elapsed := time.Since(requestStart)
	if got := string(buf[:n]); !strings.HasPrefix(got, ":") {
		t.Fatalf("first bytes are not an SSE comment: %q", got)
	}
	// 下界：宽限期内不应有字节（这里允许 100ms 调度抖动）。
	if elapsed < chatHoldBackBlindWindow-100*time.Millisecond {
		t.Fatalf("first byte arrived after %s, before the %s grace", elapsed, chatHoldBackBlindWindow)
	}
	// 上界：宽限 + 2 秒；CI 抖动下仍能区分「1 秒心跳」与「等到推理结束」。
	if elapsed > chatHoldBackBlindWindow+2*time.Second {
		t.Fatalf("first byte arrived after %s, far beyond the %s grace", elapsed, chatHoldBackBlindWindow)
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
