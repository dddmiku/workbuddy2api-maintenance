// ═══ 更新日志 ═══
// 2026-10-17：渠道拒绝（11128）改为**按账号**先换号再试（softRotate 上限），
// 试满才返回；换号次数上限 0 时维持旧行为（同号断词重发后如实返回）。
// 2026-09-26：渠道拒绝会断词重发一次（上限固定、不轮换账号）。
// 2026-09-16：固定真实 Codex 渠道拒绝的诊断，防止误报违规词或换号放大请求。
package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"workbuddy2api/internal/auth"
)

func TestChannelRejectionIsExplicitAndDoesNotRotate(t *testing.T) {
	p := testPoolWith(
		&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999},
		&auth.Auth{UID: "u2", AccessToken: "at2", ExpiresAt: 9999999999},
	)
	requests := 0
	up := newFakeUpstream(t, func(string) (int, string, bool) {
		requests++
		return http.StatusBadRequest, `{"code":11128,"msg":"Illegal API invocation from an unapproved channel","displayMsg":{"en":"The request was blocked by security policy."}}`, false
	})
	h := NewHandler(Config{Pool: p, Upstream: up})
	recorder := httptest.NewRecorder()
	h.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"cn:deepseek-v4.1-flash","input":"fixture","stream":true}`)))
	if recorder.Code != http.StatusBadRequest || !strings.Contains(recorder.Body.String(), `"code":"upstream_channel_rejected"`) {
		t.Fatalf("channel rejection lost: %d %s", recorder.Code, recorder.Body)
	}
	// 2026-10-29：断词（插零宽）已被上游识别，升级档改为「改写字母」。
	// 这份正文里没有任何客户端归属句，因此没有可改写的内容——**只发一次是对的**，
	// 无谓的重发既浪费往返又掩盖真实原因。关键断言不变：不轮换账号、如实返回渠道拒绝。
	if strings.Contains(recorder.Body.String(), "content_blocked") || strings.Contains(recorder.Body.String(), "违规词") || requests != 1 {
		t.Fatalf("wrong diagnosis or rotation: requests=%d body=%s", requests, recorder.Body)
	}
	for _, uid := range []string{"u1", "u2"} {
		state, _ := p.Status(uid)
		if state.Cooling || state.ErrTotal != 0 || state.InFlight != 0 {
			t.Fatalf("request error damaged account %s: %+v", uid, state)
		}
	}
}

// TestChannelRejectionRotatesAccounts 渠道拒绝（11128）按账号判定：配置了换号
// 次数时，被拒的号要被换掉、下一个号重试；试满仍拒才把上游原文交给调用方。
//
// 2026-10-17 生产实测依据：同一请求形态下 uid=61f74aa0 被判 11128，而
// uid=1e04e34d / 8c10f0f0 带同一头组能 200——拒绝取决于该账号的上游风控画像。
// 修复前这一分支直接 return，global 请求只试到 1–2 个号就放弃，池里的健康号
// 根本没被选中（用户看到的现象：global 全部不可用，CN 正常）。
func TestChannelRejectionRotatesAccounts(t *testing.T) {
	p := testPoolWith(
		&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999},
		&auth.Auth{UID: "u2", AccessToken: "at2", ExpiresAt: 9999999999},
		&auth.Auth{UID: "u3", AccessToken: "at3", ExpiresAt: 9999999999},
	)
	var seen []string
	up := newFakeUpstream(t, func(authHeader string) (int, string, bool) {
		// authHeader 形如 "Bearer at1"，取出令牌即账号标识。
		seen = append(seen, strings.TrimPrefix(authHeader, "Bearer "))
		// 用**上游自证**的渠道校验形态（displayTips 点名 channel verification）：
		// 只有 msg 泛化为 request illegal 而不带 displayTips 时，分类器按既有契约
		// 归 ErrClient（见 upstream/channel_rejection_test.go 的 "code alone is not
		// channel" 用例），不走本分支。
		return http.StatusBadRequest,
			`{"code":11128,"msg":"request illegal",` +
				`"displayMsg":{"en":"Request blocked. Please send it again"},` +
				`"displayTips":{"en":"This call failed channel verification, possibly because it was not sent from an official client. Please resend from the official client."}}`,
			false
	})
	h := NewHandler(Config{Pool: p, Upstream: up, MaxSoftRotations: 2})
	recorder := httptest.NewRecorder()
	h.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v1/responses",
		strings.NewReader(`{"model":"cn:deepseek-v4.1-flash","input":"fixture","stream":true}`)))

	// 换号发生过：两个号都被试过（说明被拒的号被换掉了，不是同号死磕）。
	distinct := map[string]bool{}
	for _, uid := range seen {
		distinct[uid] = true
	}
	if len(distinct) < 2 {
		t.Fatalf("channel rejection did not rotate accounts: attempts=%v", seen)
	}
	// 换号耗尽后：回 400 upstream_channel_rejected（而不是 503 no_healthy_account）
	// ——池子不是「暂时不可用」，而是每一步都被上游判为不合规调用。修复前第一个
	// 号被拒就 return，池里其余健康号根本没机会被尝试。
	if recorder.Code != http.StatusBadRequest ||
		!strings.Contains(recorder.Body.String(), `"code":"upstream_channel_rejected"`) {
		t.Fatalf("want channel rejection after exhausting rotations, got %d %s",
			recorder.Code, recorder.Body)
	}
	// 换号不等于罚号：三个号都不该进入冷却或累计错误（拒绝可能只是请求形态）。
	for _, uid := range []string{"u1", "u2", "u3"} {
		state, _ := p.Status(uid)
		if state.Cooling || state.ErrTotal != 0 || state.InFlight != 0 {
			t.Fatalf("channel rejection damaged account %s: %+v", uid, state)
		}
	}
}
