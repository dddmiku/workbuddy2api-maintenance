// ═══ 更新日志 ═══
// 2026-10-02：第二轮体检确认缺陷的回归测试（12153 误判、头值清洗）。
package upstream

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

// TestClassifyDoesNotTreat12153SubstringAsSessionDead "12153" 是裸数字串，
// 子串匹配会把任何正文里恰好含这五个数字的响应判成死会话，而 ErrSessionDead
// 的处置是永久禁用账号。
//
// 2026-10-02 第二轮体检实测（修复前这些全部误判为 session_dead）。
func TestClassifyDoesNotTreat12153SubstringAsSessionDead(t *testing.T) {
	// 这些都必须**不**是 ErrSessionDead。
	for _, c := range []struct {
		name   string
		status int
		body   string
	}{
		{"429 限流带毫秒时间戳", 429, `{"code":6004,"msg":"rate limit","ts":1759121530123}`},
		{"400 参数错误带 requestId", 400, `{"code":11101,"msg":"Unmarshal chat params failed","requestId":"req_12153"}`},
		{"500 服务错误带 trace", 500, `{"code":500,"msg":"internal","trace":"1759121530123"}`},
		{"200 正常响应带 token 计数", 200, `{"usage":{"total_tokens":12153}}`},
	} {
		if got := Classify(c.status, c.body); got == ErrSessionDead {
			t.Errorf("%s: 误判为 session_dead（会永久停用健康号）: %s", c.name, c.body)
		}
	}

	// 真正携带 12153 作为 code 的仍须识别。
	for _, c := range []struct {
		name   string
		status int
		body   string
	}{
		{"标准 12153 死会话", 401, `{"code":12153,"msg":"Offline user session not found"}`},
		{"code 带空格", 401, `{"code": 12153, "msg":"session gone"}`},
		{"code 为字符串", 401, `{"code":"12153","msg":"session gone"}`},
		{"仅英文文案", 401, `{"msg":"Offline user session not found"}`},
	} {
		if got := Classify(c.status, c.body); got != ErrSessionDead {
			t.Errorf("%s: 应识别为 session_dead，实际 %s: %s", c.name, got, c.body)
		}
	}
}

// TestSanitizeHeaderValueDropsUnsafeBytes 客户端可控的 conversation_id 直接进
// HTTP 头时，CR/LF 会让整个请求以 transport error 失败（503），客户端可据此
// 稳定打掉自己的请求并消耗账号。
//
// 2026-10-02 第二轮体检发现。
func TestSanitizeHeaderValueDropsUnsafeBytes(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"plain-id", "plain-id"},
		{"ok\r\nX-Injected: 1", "okX-Injected: 1"},
		{"tab\there", "tab\there"},
		{"\x00nul", "nul"},
		{"high\x80byte", "highbyte"},
	} {
		if got := sanitizeHeaderValue(c.in); got != c.want {
			t.Errorf("sanitizeHeaderValue(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestConversationHeaderRejectsInjection 端到端：注入 attempt 不应让请求失败，
// 且不得出现被注入的头。
func TestConversationHeaderRejectsInjection(t *testing.T) {
	c := New()
	req, _ := http.NewRequest("POST", "https://example.invalid/v2/chat/completions", strings.NewReader("{}"))
	c.injectConversationHeaders(req, ChatMeta{ConversationID: "ok\r\nX-Injected: 1"})
	if v := req.Header.Get("X-Injected"); v != "" {
		t.Fatalf("注入头被写入: %q", v)
	}
	if v := req.Header.Get("X-Conversation-ID"); strings.ContainsAny(v, "\r\n") {
		t.Fatalf("X-Conversation-ID 仍含 CR/LF: %q", v)
	}
	// 请求必须仍然可发（net/http 会拒绝含非法字节的头值）。
	if err := req.Header.Get("X-Conversation-ID"); err == "" {
		t.Fatal("未取到头值")
	}
}

// TestPromptEstimateIsLinearInBodySize 估算必须随请求体线性增长，不能是二次的。
//
// 正文含长串 'd'（`data:` 重复、直到很后面才有闭合引号）时，旧实现每步都重扫到
// 那个引号，实测 O(n²)：2MB 请求要约 10.7 秒 CPU；而 estimateInputTokens 会同时
// 调用 textBytes 与 estimatePromptTokens，11133/上下文路径还会反复调用同一 body，
// 单个请求即可烧掉几十秒 CPU 拖垮单进程网关。
//
// 2026-10-02 第二轮体检发现。
func TestPromptEstimateIsLinearInBodySize(t *testing.T) {
	measure := func(n int) time.Duration {
		body := []byte(`{"messages":[{"role":"user","content":"` + strings.Repeat("data:", n/5) + `"}]}`)
		start := time.Now()
		estimateInputTokens(body, 0)
		return time.Since(start)
	}
	small := measure(200_000)
	large := measure(1_600_000) // 8 倍体量

	// 线性应为 8 倍；二次会到 64 倍。用 24 倍作阈值，给足调度抖动余量，
	// 同时对二次复杂度（64 倍）有决定性区分度。
	if large > small*24+50*time.Millisecond {
		t.Fatalf("估算不是线性：200KB=%v，1.6MB=%v（比值 %.1fx，线性应约 8x，二次会到 64x）",
			small, large, float64(large)/float64(small))
	}
}

// TestBase64ImageDataStillExcluded 线性化不能改变语义：base64 图片数据仍须排除。
func TestBase64ImageDataStillExcluded(t *testing.T) {
	body := []byte(`{"content":[{"image_url":{"url":"data:image/png;base64,` +
		strings.Repeat("A", 5000) + `"}},{"text":"` + strings.Repeat("x", 100) + `"}]}`)
	if got := textBytes(body); got > 300 {
		t.Fatalf("base64 图片数据未被排除: textBytes=%d（总量 %d）", got, len(body))
	}
	// 非 base64 的 data: 字样照常计入正文。
	plain := []byte(`{"content":"data: not an image"}`)
	if got := textBytes(plain); got != int64(len(plain)) {
		t.Fatalf("非 base64 的 data: 不应被跳过: textBytes=%d want %d", got, len(plain))
	}
}
