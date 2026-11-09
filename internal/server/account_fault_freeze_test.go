// 11140 冻结档：收到账号级授权封禁时不换号，直接把上游原文回给调用方。
//
// 用户 2026-11-09 的需求：「检测到一个号返回 11140 则不切任何号，这样死一个号
// 还能抓包改改救一下，原样返回 11140」。
//
// 背景：11140 意味着「这个号在上游被判异常」。默认行为是换号重试——能救活当次
// 请求，但事故现场（哪个号、什么形态、上游原文）被冲掉了。冻结后当次请求失败，
// 换来的是可以抓包比对的完整证据。
//
// 边界必须窄：
//   - 只认 code 11140（14017 trial not activated 是配置缺失，继续轮转更有价值）
//   - 11140 也承载模型级限流文案，那种必须保持 soft_rate，不能冻结
//   - 关闭开关时行为必须与改动前逐字一致
package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/upstream"
)

// accountFaultBody 11140 生产形态（request illegal 为主 msg，含上游 requestId）。
const accountFaultBody = `{"code":11140,"msg":"request illegal","requestId":"01a1b4d6fbdc756482b2e50c1732abe2","displayMsg":{"en":"Request blocked. Please send it again"}}`

// 识别函数本身：只认 11140，且排除限流形态与 14017。
func TestIsAccountFaultCode(t *testing.T) {
	cases := []struct {
		name string
		body string
		want bool
	}{
		{"observed 11140", accountFaultBody, true},
		{"11140 code as string", `{"code":"11140","msg":"request illegal"}`, true},
		{"11140 nested in error", `{"error":{"code":11140,"msg":"request illegal"}}`, true},
		// 14017 是配置缺失（trial not activated），不冻结。
		{"14017 is not freeze-worthy", `{"code":14017,"msg":"The trial version is not yet activated"}`, false},
		// 11140 也承载模型级限流，必须保持 soft_rate 语义。
		{"11140 rate limiting variant", `{"code":11140,"msg":"The model provider is rate-limiting requests.","displayMsg":{"en":"Too many requests. Please retry later."}}`, false},
		{"11128 is not account fault", `{"code":11128,"msg":"Illegal API invocation from an unapproved channel"}`, false},
		{"empty body", "", false},
		{"plain text", "request illegal", false},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			if got := upstream.IsAccountFaultCode(c.body); got != c.want {
				t.Fatalf("IsAccountFaultCode(%q) = %v want %v", c.body, got, c.want)
			}
		})
	}
}

// 开启冻结档：撞 11140 后必须**只打一次上游**，并把原文回给调用方。
func TestFreezeOnAccountFaultStopsRotation(t *testing.T) {
	p := testPoolWith(
		&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999},
		&auth.Auth{UID: "u2", AccessToken: "at2", ExpiresAt: 9999999999},
		&auth.Auth{UID: "u3", AccessToken: "at3", ExpiresAt: 9999999999},
	)
	attempts := 0
	up := newFakeUpstream(t, func(string) (int, string, bool) {
		attempts++
		return http.StatusForbidden, accountFaultBody, false
	})
	h := NewHandler(Config{
		Pool: p, Upstream: up,
		MaxSoftRotations: 6, MaxRotate: 6,
		FreezeOnAccountFault: true,
	})
	recorder := httptest.NewRecorder()
	h.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"cn:deepseek-v4.1-flash","stream":true,`+
			`"messages":[{"role":"system","content":"hi"},{"role":"user","content":"hi"}]}`)))

	if attempts != 1 {
		t.Fatalf("上游被打了 %d 次，冻结档应只打 1 次（不换号）", attempts)
	}
	body := recorder.Body.String()
	// 原样透传：code/msg 都要在（便于抓包比对），不是网关自造文案。
	if !strings.Contains(body, "11140") || !strings.Contains(body, "request illegal") {
		t.Fatalf("未原样透传上游 11140，实际: %s", body)
	}
	if !strings.Contains(body, "upstream_account_fault") {
		t.Fatalf("缺少专属错误码，实际: %s", body)
	}
}

// 关闭冻结档（默认）：必须仍然换号——证明改动没有破坏既有契约。
func TestFreezeDisabledStillRotates(t *testing.T) {
	p := testPoolWith(
		&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999},
		&auth.Auth{UID: "u2", AccessToken: "at2", ExpiresAt: 9999999999},
		&auth.Auth{UID: "u3", AccessToken: "at3", ExpiresAt: 9999999999},
	)
	attempts := 0
	up := newFakeUpstream(t, func(string) (int, string, bool) {
		attempts++
		return http.StatusForbidden, accountFaultBody, false
	})
	h := NewHandler(Config{Pool: p, Upstream: up, MaxRotate: 3})
	recorder := httptest.NewRecorder()
	h.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"cn:deepseek-v4.1-flash","stream":true,`+
			`"messages":[{"role":"system","content":"hi"},{"role":"user","content":"hi"}]}`)))

	if attempts < 2 {
		t.Fatalf("上游只被打了 %d 次，关闭冻结档时应继续换号", attempts)
	}
	if strings.Contains(recorder.Body.String(), "upstream_account_fault") {
		t.Fatalf("关闭冻结档却返回了冻结专属错误码: %s", recorder.Body.String())
	}
}

// 14017 不受冻结档影响：它是配置缺失、补完 register 可自愈，继续轮转。
func TestFreezeDoesNotBlock14017(t *testing.T) {
	p := testPoolWith(
		&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999},
		&auth.Auth{UID: "u2", AccessToken: "at2", ExpiresAt: 9999999999},
	)
	attempts := 0
	up := newFakeUpstream(t, func(string) (int, string, bool) {
		attempts++
		return http.StatusTooManyRequests, `{"code":14017,"msg":"The trial version is not yet activated"}`, false
	})
	h := NewHandler(Config{
		Pool: p, Upstream: up,
		MaxRotate: 2, FreezeOnAccountFault: true,
	})
	recorder := httptest.NewRecorder()
	h.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"cn:deepseek-v4.1-flash","stream":true,`+
			`"messages":[{"role":"system","content":"hi"},{"role":"user","content":"hi"}]}`)))

	if attempts < 2 {
		t.Fatalf("14017 被冻结档拦住了（只打 %d 次），应继续轮转", attempts)
	}
	if strings.Contains(recorder.Body.String(), "upstream_account_fault") {
		t.Fatalf("14017 不该返回冻结专属错误码: %s", recorder.Body.String())
	}
}
