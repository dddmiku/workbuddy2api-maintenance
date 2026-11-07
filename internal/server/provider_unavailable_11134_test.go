// 11134「模型提供方暂时不可用」不得熔断账号（2026-11-07 生产实测）。
//
// 事故形态：上游回 HTTP 500 + code 11134
//
//	"code":11134,"msg":"the model provider is temporarily unavailable,
//	 please retry later or switch to another model"
//
// Classify 把它判成 ErrServer（status>=500 的兜底），applyErrorPolicy 调 NoteError
// 喂熔断计数，连续 3 次就把账号摘出池子 30 分钟。
//
// 实测证据表明这是模型后端问题、与账号无关：
//   - 15:05-15:22 五分钟内，五个不同账号各中 3-4 次；
//   - 这些号积分 5298-6348、disabled=false、成功率 EMA 0.66-0.73；
//   - 熔断到期后 b0eedd14 立刻连续 7 个请求全 200（TTFB 2.1-3.9s）；
//   - 文案自己写着 "switch to another model"。
//
// 对照 14003「Model busy」：语义等价，早就豁免了（见 IsTransientModelBusy）。
// 11134 只是状态码落在通用 5xx 分支，漏了豁免。
package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/upstream"
)

const providerUnavailableBody = `{"code":11134,"msg":"the model provider is temporarily unavailable, ` +
	`please retry later or switch to another model","requestId":"01a1b4d6fbdc756482b2e50c1732abe2"}`

func TestIsTransientProviderUnavailable(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   bool
	}{
		{"observed 500", 500, providerUnavailableBody, true},
		{"502 wrapping same code", 502, providerUnavailableBody, true},
		{"lowercase msg", 500, `{"code":11134,"msg":"The Model Provider is Temporarily Unavailable"}`, true},
		{"code as string", 500, `{"code":"11134","msg":"model provider is temporarily unavailable"}`, true},
		{"400 with same code", 400, providerUnavailableBody, false},
		{"500 without marker", 500, `{"code":11134,"msg":"internal server error"}`, false},
		{"500 with other code", 500, `{"code":50001,"msg":"model provider is temporarily unavailable"}`, false},
		{"marker in quoted text only", 500,
			`{"code":11101,"msg":"bad params","request_excerpt":"11134 model provider is temporarily unavailable"}`, false},
		{"empty body", 500, "", false},
		{"plain html", 500, "<html>model provider is temporarily unavailable</html>", false},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			if got := upstream.IsTransientProviderUnavailable(c.status, c.body); got != c.want {
				t.Fatalf("IsTransientProviderUnavailable(%d, %q) = %v want %v", c.status, c.body, got, c.want)
			}
		})
	}
}

// 同账号连续撞 3 次 11134（= breakerThreshold 生产默认值）仍不得开熔断。
//
// 直接打 applyErrorPolicy：这是「错误 → 台账」的唯一决策点，绕开轮转循环后
// 才能确定是**同一个账号**被喂了三次，而不是分摊到不同账号（分摊就证明不了
// 熔断计数没累加）。
func TestProviderUnavailableDoesNotTripBreaker(t *testing.T) {
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	p.SetBreaker(3, 30*time.Minute, 6*time.Hour)

	h := NewHandler(Config{Pool: p, Upstream: newFakeUpstream(t,
		func(string) (int, string, bool) { return http.StatusInternalServerError, "", false })})
	for i := 0; i < 3; i++ {
		h.applyErrorPolicy("u1", upstream.ErrServer, providerUnavailableBody, "deepseek-v4.1-flash")
	}

	state, _ := p.Status("u1")
	if state.Cooling || state.CoolRemaining > 0 {
		t.Fatalf("11134 撞 3 次后账号被熔断（应只记观测时间）: %+v", state)
	}
}

// 对照：普通 5xx 仍要喂熔断——豁免必须是窄判据，不能把 5xx 全放掉。
func TestGenericServerErrorStillTripsBreaker(t *testing.T) {
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	p.SetBreaker(1, 30*time.Minute, 6*time.Hour)
	up := newFakeUpstream(t, func(string) (int, string, bool) {
		return http.StatusInternalServerError, `{"code":500,"msg":"internal error"}`, false
	})
	h := NewHandler(Config{Pool: p, Upstream: up})
	recorder := httptest.NewRecorder()
	h.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"cn:deepseek-v4.1-flash","stream":true,`+
			`"messages":[{"role":"system","content":"hi"},{"role":"user","content":"hi"}]}`)))

	state, _ := p.Status("u1")
	if !state.Cooling {
		t.Fatalf("普通 5xx 未熔断账号（豁免范围过宽）: %+v", state)
	}
}
