// ═══ 更新日志 ═══
// 2026-10-17：契约反转——渠道拒绝实测为**账号绑定**，改为隔离账号（此前是断词重发）。
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

// TestChannelRejectionQuarantinesAccountAndIsExplicit 锁住 2026-10-17 的实测结论：
// 渠道校验是**账号绑定**的，不是请求绑定——因此正确处置是隔离该账号，而不是
// 反复换号/断词重试。
//
// 实测依据（36 账号 × 同一极简 body × 同一套头，两轮完全复现）：
//
//	通过组 8 个账号：200 / 429，全部**从未**出现在渠道拒绝落盘里（0/8）
//	被拒组 16 个账号：11128 / 11140，13/16 有落盘记录，间隔 20s 重测恒拒
//
// 健康账号带 Claude Code 完整系统提示、带中和后的零宽形态照样 200；被烧账号发最简
// 「You are a helpful assistant.」照样 400。头部（含 X-Conversation-Request-ID）、
// 请求体、零宽形态、metadata / prompt_cache_key / thinking 全部被排除。
//
// 旧契约（「断词重发一次、不轮换账号」）建立在「拒绝与请求内容相关」的假设上；
// 该假设已被上述对照实验证伪，故本条测试随契约一并更新。
func TestChannelRejectionQuarantinesAccountAndIsExplicit(t *testing.T) {
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

	// ① 客户端必须看到「渠道校验」这个真实原因，而不是被误判成内容违规，
	//    也不是笼统的 no_healthy_account（那会掩盖问题、让运维查错方向）。
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status=%d want=400 body=%s", recorder.Code, recorder.Body)
	}
	if !strings.Contains(recorder.Body.String(), `"code":"upstream_channel_rejected"`) {
		t.Fatalf("channel rejection lost: %d %s", recorder.Code, recorder.Body)
	}
	if strings.Contains(recorder.Body.String(), "content_blocked") || strings.Contains(recorder.Body.String(), "违规词") {
		t.Fatalf("wrong diagnosis: %s", recorder.Body)
	}

	// ② 上游原文透传：排查必须能看到 11128 与 displayTips。
	if !strings.Contains(recorder.Body.String(), "11128") {
		t.Fatalf("upstream body not passed through: %s", recorder.Body)
	}

	// ③ 两个号都被隔离，且是被**关**起来（Cooling），不是被记成「账号故障」
	//    （ErrTotal 保持 0：渠道校验是渠道问题，不是账号自身出错）。
	for _, uid := range []string{"u1", "u2"} {
		state, _ := p.Status(uid)
		if !state.Cooling {
			t.Fatalf("account %s was not quarantined: %+v", uid, state)
		}
		if state.ErrTotal != 0 {
			t.Fatalf("channel rejection must not count as account error %s: %+v", uid, state)
		}
		if state.InFlight != 0 {
			t.Fatalf("in-flight leak on %s: %+v", uid, state)
		}
	}

	// ④ 关键：被烧账号**不再被反复轮转**。上游层的分级中和仍会在单次尝试内
	//    重发（上限 maxChannelNeutralizeLevels，与账号无关的既有机制），但
	//    handler 层不得换到别的账号再撞一遍——每个账号最多 2 次（首发 + 一次中和）。
	if requests > 2*2 {
		t.Fatalf("requests=%d 超出上限（每个账号最多 2 次：首发 + 一次中和）", requests)
	}
}

// TestChannelRejectionDoesNotRetryQuarantinedAccount 已被隔离的账号不应被再次选中：
// 第二次请求时池里没有健康号，应直接给 no_healthy_account（且不产生新的上游请求），
// 而不是又去撞一遍已知被烧的号。
func TestChannelRejectionDoesNotRetryQuarantinedAccount(t *testing.T) {
	p := testPoolWith(
		&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999},
	)
	requests := 0
	up := newFakeUpstream(t, func(string) (int, string, bool) {
		requests++
		// 生产真实形态：msg 是「request illegal」，渠道校验的判据在 displayTips
		// （见 upstream.mentionsChannelVerification）。只用 msg 的桩会让这条路径
		// 走不到渠道拒绝分支，测出来的就不是生产行为了。
		return http.StatusBadRequest, `{"code":11128,"msg":"request illegal","requestId":"req-1",` +
			`"displayMsg":{"en":"Request blocked. Please send it again"},` +
			`"displayTips":{"en":"This call failed channel verification, possibly because it was not sent from an official client."}}`, false
	})
	h := NewHandler(Config{Pool: p, Upstream: up})

	body := `{"model":"cn:deepseek-v4.1-flash","input":"fixture","stream":true}`
	rec1 := httptest.NewRecorder()
	h.ServeHTTP(rec1, httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body)))
	after1 := requests
	// 第一次请求内允许上游层的分级中和重发（上限 3 档，见 upstream.maxChannelNeutralizeLevels），
	// 但绝不换号——单账号池里全部出站都属于这一个账号。
	if after1 < 1 || after1 > 3 {
		t.Fatalf("first request: requests=%d want 1..3", after1)
	}
	state, _ := p.Status("u1")
	if !state.Cooling {
		t.Fatalf("account was not quarantined after channel rejection: %+v", state)
	}

	// 第二次：唯一账号已被隔离 → **不得**再发任何上游请求（计数不再增长）
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body)))
	if requests != after1 {
		t.Fatalf("quarantined account was retried: requests=%d want=%d（隔离后不得再撞）", requests, after1)
	}
	if rec2.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d want=503 body=%s", rec2.Code, rec2.Body)
	}
	if !strings.Contains(rec2.Body.String(), "no_healthy_account") {
		t.Fatalf("expected no_healthy_account: %s", rec2.Body)
	}
}
