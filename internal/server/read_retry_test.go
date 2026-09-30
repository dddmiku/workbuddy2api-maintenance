// ═══ 更新日志 ═══
// 2026-09-30：锁定「上游掐流且客户端零字节时换号重试」；已交付内容则不得重试。
package server

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/upstream"
)

// truncatingUpstream 第一次调用就掐断流（模拟上游 INTERNAL_ERROR），
// 之后返回完整回复。count 记录上游被调用的次数。
func truncatingUpstream(t *testing.T, cutAfter string, good string) (*upstream.Client, *atomic.Int32) {
	t.Helper()
	var calls atomic.Int32
	client := &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			n := calls.Add(1)
			body := cutAfter
			if n > 1 {
				body = good
			}
			// 第一轮掐流（读到 cutAfter 就报错）；后续轮次正常结束。
			var reader io.Reader
			if n == 1 {
				reader = &truncatingReader{data: []byte(cutAfter), failAt: len(cutAfter)}
			} else {
				reader = strings.NewReader(body)
			}
			return &http.Response{
				StatusCode: 200,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:       io.NopCloser(reader),
			}, nil
		})},
		ChatBaseCN:    "https://fake.example",
		BillingBaseCN: "https://fake.example",
	}
	return client, &calls
}

// truncatingReader 在输出 failAt 字节后返回读错误，模拟上游把连接掐断。
type truncatingReader struct {
	data   []byte
	pos    int
	failAt int
}

func (r *truncatingReader) Read(p []byte) (int, error) {
	if r.pos >= r.failAt {
		return 0, io.ErrUnexpectedEOF
	}
	remaining := r.failAt - r.pos
	if remaining > len(p) {
		remaining = len(p)
	}
	n := copy(p, r.data[r.pos:r.pos+remaining])
	r.pos += n
	return n, nil
}

// TestUpstreamCutBeforeContentRotates 上游掐流且客户端零字节时，应换号重试并成功。
//
// 2026-09-30 线上实测：上游偶发 INTERNAL_ERROR（约占 0.7%），此前直接失败给客户端；
// 客户端什么都没收到，换号重发是安全且有效的。
func TestUpstreamCutBeforeContentRotates(t *testing.T) {
	// 第一轮只发一个空的开流帧就断——客户端拿不到任何正文。
	cut := "data: {\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\"}}]}\n\n"
	good := "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"recovered\"},\"finish_reason\":null}]}\n\n" +
		"data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":2}}\n\n" +
		"data: [DONE]\n\n"
	up, calls := truncatingUpstream(t, cut, good)
	h := NewHandler(Config{
		APIKey: "k",
		Pool: testPoolWith(
			&auth.Auth{UID: "acct-a", AccessToken: "at-a", ExpiresAt: 9999999999},
			&auth.Auth{UID: "acct-b", AccessToken: "at-b", ExpiresAt: 9999999999},
		),
		Upstream: up,
	})
	body := `{"model":"cn:fixture","stream":true,"max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer k")
	h.ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}
	if got := calls.Load(); got < 2 {
		t.Fatalf("上游掐流后应换号重试，实际只调用 %d 次", got)
	}
	if !strings.Contains(w.Body.String(), "recovered") {
		t.Fatalf("重试后的内容没有交付给客户端: %s", w.Body.String())
	}
}

// TestUpstreamCutAfterContentDoesNotRetry 已经交付过内容后再掐流，不得重试——
// 重试会让客户端看到重复或矛盾的两段输出。
func TestUpstreamCutAfterContentDoesNotRetry(t *testing.T) {
	// 先交付一段正文，然后掐断。
	cut := "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"partial answer\"},\"finish_reason\":null}]}\n\n"
	good := "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"SHOULD NOT APPEAR\"},\"finish_reason\":null}]}\n\n" +
		"data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n" +
		"data: [DONE]\n\n"
	up, calls := truncatingUpstream(t, cut, good)
	h := NewHandler(Config{
		APIKey: "k",
		Pool: testPoolWith(
			&auth.Auth{UID: "acct-a", AccessToken: "at-a", ExpiresAt: 9999999999},
			&auth.Auth{UID: "acct-b", AccessToken: "at-b", ExpiresAt: 9999999999},
		),
		Upstream: up,
	})
	body := `{"model":"cn:fixture","stream":true,"max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer k")
	h.ServeHTTP(w, r)
	if got := calls.Load(); got != 1 {
		t.Fatalf("已交付内容后不得重试，实际调用 %d 次", got)
	}
	if strings.Contains(w.Body.String(), "SHOULD NOT APPEAR") {
		t.Fatalf("重试内容泄漏给了客户端: %s", w.Body.String())
	}
}

// TestDeliveredContentCoversStructuredOutput 结构化输出（keepText=true）时也必须
// 标记「已交付内容」。
//
// 2026-09-30 深度体检发现：chatContractWriter.keepSlim 只在 keepText=false 的分支里
// 设置 marked，于是结构化输出路径下 DeliveredContent() 恒为 false——上游掐流时
// handler 会以为「客户端还没收到东西」而重发，客户端因此看到重复输出。
func TestDeliveredContentCoversStructuredOutput(t *testing.T) {
	w := &chatContractWriter{marked: map[any]bool{}}
	w.keepSlim(map[string]any{"choices": []any{map[string]any{
		"index": float64(0),
		"delta": map[string]any{"content": "hello"},
	}}})
	if !w.DeliveredContent() {
		t.Fatal("结构化输出已下发正文，DeliveredContent 必须为 true，否则掐流重试会重复输出")
	}
}

// TestDeliveredContentFalseBeforeAnyText 只有 role 之类的信封字段不算内容。
func TestDeliveredContentFalseBeforeAnyText(t *testing.T) {
	w := &chatContractWriter{marked: map[any]bool{}}
	w.keepSlim(map[string]any{"choices": []any{map[string]any{
		"index": float64(0),
		"delta": map[string]any{"role": "assistant"},
	}}})
	if w.DeliveredContent() {
		t.Fatal("只有 role 的帧不算内容，此时重试是安全的")
	}
}

// TestResponsesResetForRetryClearsContent Responses 的写入器贯穿轮转循环，
// 换号重发前必须清掉上一轮的内容累积与序号，否则重试的内容接在旧状态后面。
//
// 2026-09-30 审计发现：此前没有重置，掐流重试在 /v1/responses 上完全失效。
func TestResponsesResetForRetryClearsContent(t *testing.T) {
	rw := &responsesWriter{inner: httptest.NewRecorder(), calls: map[int]*respToolCall{}}
	rw.text.WriteString("previous answer")
	rw.reason.WriteString("previous reasoning")
	rw.calls[0] = &respToolCall{opened: true, sentArgs: 12}
	rw.order = []int{0}
	rw.nextIdx, rw.seq = 3, 7
	rw.finishReason, rw.sawDone = "tool_calls", true
	rw.ResetForRetry()
	if rw.text.Len() != 0 || rw.reason.Len() != 0 || len(rw.calls) != 0 || len(rw.order) != 0 {
		t.Fatalf("内容未清空: text=%q reason=%q calls=%d order=%d", rw.text.String(), rw.reason.String(), len(rw.calls), len(rw.order))
	}
	if rw.nextIdx != 0 {
		t.Fatalf("输出索引未归零: nextIdx=%d", rw.nextIdx)
	}
	// seq 必须**保留**：beginStream 已把 response.created(seq 0) 与
	// response.in_progress(seq 1) 发给客户端，归零会让整条流的 sequence_number
	// 变成 [0 1 0 1 …]，严格按单调递增消费的客户端会丢弃事件
	// （2026-09-30 深度体检发现：这是掐流重试引入的回归）。
	if rw.seq != 7 {
		t.Fatalf("seq 不应归零（应保持单调递增）: seq=%d", rw.seq)
	}
	if rw.finishReason != "" || rw.sawDone {
		t.Fatalf("终态标志未清空: finish=%q sawDone=%v", rw.finishReason, rw.sawDone)
	}
}

// TestResponsesDeliveredContentIgnoresBufferedToolCalls 只收到 id/name、参数还没
// emit 给客户端时，不能算「已交付内容」——否则掐流重试会被误判而放弃。
func TestResponsesDeliveredContentIgnoresBufferedToolCalls(t *testing.T) {
	rw := &responsesWriter{inner: httptest.NewRecorder(), calls: map[int]*respToolCall{}}
	rw.calls[0] = &respToolCall{outIdx: -1} // 刚建缓冲，未 open、未发参数
	if rw.DeliveredContent() {
		t.Fatal("参数还没 emit 给客户端，不应算已交付")
	}
	rw.calls[0].opened = true // 已发 response.output_item.added
	if !rw.DeliveredContent() {
		t.Fatal("已 emit 工具条目，应算已交付")
	}
}

// TestResetForRetryClearsUsage 重发前必须清空用量：被丢弃的那一轮已经 absorb 过，
// 不清的话重试成功后末帧会把上一轮的计数再报一次（客户端与账本都翻倍）。
//
// 2026-09-30 审计发现：ResetForRetry 最初只清内容字段，漏了 usage/usageEnvelope。
func TestResetForRetryClearsUsage(t *testing.T) {
	w := &chatContractWriter{inner: httptest.NewRecorder(), marked: map[any]bool{}}
	w.usage = map[string]any{"prompt_tokens": 100, "completion_tokens": 50}
	w.usageEnvelope = map[string]any{"id": "chatcmpl-x"}
	w.ResetForRetry()
	if w.usage != nil || w.usageEnvelope != nil {
		t.Fatalf("用量未清空: usage=%v envelope=%v", w.usage, w.usageEnvelope)
	}
}

// TestStreamFailureMarksChecked StreamFailure 写过错误帧与 [DONE] 后必须标记完成，
// 否则 defer 的 finishResponseWriters 会再进 CompletionError，重复写终态。
func TestStreamFailureMarksChecked(t *testing.T) {
	rec := httptest.NewRecorder()
	w := &chatContractWriter{inner: rec, marked: map[any]bool{}, streaming: true}
	if !w.StreamFailure("upstream_read_error", "cut") {
		t.Fatal("流已开启时 StreamFailure 应交付")
	}
	if !w.checked {
		t.Fatal("StreamFailure 之后必须置 checked，否则终态会重复写入")
	}
}

// TestChatRefusalFrameMarksDelivered 拒答是**立即下发**给客户端的可见内容，
// 掐流重试必须把它算作「已交付」，否则会在用户已经读到拒答后重发，
// 用户看到两段互相矛盾的回答。
//
// 工具调用帧刻意不算（它们在 frame() 里被删掉、要等整组校验通过才交付），
// 见 TestChatToolFramesDoNotBlockRetry。
//
// 2026-09-30 深度体检发现。
func TestChatRefusalFrameMarksDelivered(t *testing.T) {
	w := &chatContractWriter{inner: httptest.NewRecorder(), marked: map[any]bool{}}
	w.keepSlim(map[string]any{"choices": []any{map[string]any{
		"index": 0, "delta": map[string]any{"refusal": "I cannot help with that"},
	}}})
	if !w.DeliveredContent() {
		t.Fatal("拒答帧已下发，必须算作已交付内容")
	}
}

// TestChatToolFramesDoNotBlockRetry 工具调用帧不下发（frame() 里被删除，等整组
// 参数校验通过才在收尾时交付），所以此刻客户端什么都还没收到，掐流重试是安全的。
// 若把它们标记为「已交付」，掐流重试会白白失效。
func TestChatToolFramesDoNotBlockRetry(t *testing.T) {
	w := &chatContractWriter{inner: httptest.NewRecorder(), marked: map[any]bool{}}
	w.keepSlim(map[string]any{"choices": []any{map[string]any{
		"index": 0, "delta": map[string]any{"tool_calls": []any{map[string]any{
			"index": 0, "id": "call_1", "function": map[string]any{"name": "read", "arguments": `{"a":1}`},
		}}},
	}}})
	if w.DeliveredContent() {
		t.Fatal("工具帧尚未交付给客户端，不应阻止换号重试")
	}
}

// failingRetryUpstream 第一轮开流后掐断（触发换号重试），第二轮直接返回 HTTP 4xx。
func failingRetryUpstream(t *testing.T, cutAfter string, status int, body string) (*upstream.Client, *atomic.Int32) {
	t.Helper()
	var calls atomic.Int32
	client := &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			n := calls.Add(1)
			if n == 1 {
				return &http.Response{
					StatusCode: 200,
					Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
					Body:       io.NopCloser(&truncatingReader{data: []byte(cutAfter), failAt: len(cutAfter)}),
				}, nil
			}
			return &http.Response{
				StatusCode: status,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(strings.NewReader(body)),
			}, nil
		})},
		ChatBaseCN:    "https://fake.example",
		BillingBaseCN: "https://fake.example",
	}
	return client, &calls
}

// TestRotationRetryFailureStaysInsideStream 换号重试没能建立时（第二轮直接 4xx），
// 失败必须交付在**已经开启的 SSE 流内**。
//
// 修复前这里走 upstream.WriteStreamError：它按 Chat 形状写 data 帧，会绕过
// Responses / Anthropic 适配器；而它的兜底 writeOpenAIError 更糟——往已开的流中间
// 粘一段裸 JSON（无 data: 前缀、无空行分隔），客户端解析失败且看不到任何终态，
// 随后契约写入器的 defer 还会补上上一轮的错误帧与第二个 [DONE]。
//
// 2026-09-30 深度体检发现。
func TestRotationRetryFailureStaysInsideStream(t *testing.T) {
	cut := "data: {\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\"}}]}\n\n"
	up, calls := failingRetryUpstream(t, cut, http.StatusBadRequest, `{"code":40001,"msg":"upstream rejected the retry"}`)
	h := NewHandler(Config{
		APIKey: "k",
		Pool: testPoolWith(
			&auth.Auth{UID: "acct-a", AccessToken: "at-a", ExpiresAt: 9999999999},
			&auth.Auth{UID: "acct-b", AccessToken: "at-b", ExpiresAt: 9999999999},
		),
		Upstream: up,
	})
	body := `{"model":"cn:fixture","stream":true,"max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer k")
	h.ServeHTTP(w, r)
	if calls.Load() < 2 {
		t.Fatalf("应发生换号重试，实际只调用 %d 次", calls.Load())
	}
	out := w.Body.String()
	// 每一段非空行要么是 SSE 注释/字段行，要么是 data: 行——不允许出现裸 JSON。
	for _, line := range strings.Split(out, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "data:") || strings.HasPrefix(trimmed, ":") || strings.HasPrefix(trimmed, "event:") {
			continue
		}
		t.Fatalf("SSE 流里出现了非 SSE 行（裸 JSON）: %q\n完整响应: %s", trimmed, out)
	}
	if !strings.Contains(out, "data: [DONE]") {
		t.Fatalf("失败没有以 [DONE] 收尾: %s", out)
	}
	if strings.Count(out, "data: [DONE]") != 1 {
		t.Fatalf("终态重复写入: %s", out)
	}
}

// TestStreamFailureIsIdempotent 失败终态只能交付一次。此前 StreamFailure 没有
// 「已完成」检查：若别处已经写过终态（或它自己被调用两次），会再写一个
// error 帧与 [DONE]，客户端看到重复终态。
//
// 2026-09-30 深度体检发现。
func TestStreamFailureIsIdempotent(t *testing.T) {
	rec := httptest.NewRecorder()
	w := &chatContractWriter{inner: rec, marked: map[any]bool{}, streaming: true}
	if !w.StreamFailure("upstream_read_error", "cut") {
		t.Fatal("首次调用应交付")
	}
	first := rec.Body.String()
	if again := w.StreamFailure("upstream_read_error", "cut"); again {
		t.Fatal("第二次调用不应再交付终态")
	}
	if rec.Body.String() != first {
		t.Fatalf("重复调用改写了响应体:\n第一次: %s\n之后: %s", first, rec.Body.String())
	}
	if strings.Count(rec.Body.String(), "data: [DONE]") != 1 {
		t.Fatalf("终态重复写入: %s", rec.Body.String())
	}
}
