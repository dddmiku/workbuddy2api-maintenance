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
