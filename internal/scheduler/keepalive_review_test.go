// ═══ 更新日志 ═══
// 2026-10-07：新增。内容审核标记的停用号不得被保活自动复活：它的令牌是好的，
//
//	refresh 一定成功，但拒绝发生在模型调用层；自动复活会形成
//	「复活 → 再被拒 → 再停用」的循环，每轮白烧上游请求。
package scheduler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/upstream"
)

// TestKeepaliveDoesNotReviveReviewFlagged 审核标记号：refresh 成功也不复活。
func TestKeepaliveDoesNotReviveReviewFlagged(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		w.Write([]byte(`{"code":0,"data":{"access_token":"new","refresh_token":"rt2"}}`))
	}))
	defer srv.Close()

	p := pool.New("")
	a := &auth.Auth{UID: "u1", AccessToken: "old", RefreshToken: "rt", ExpiresAt: 1}
	p.Add(a)
	// 用真实入口把它按「内容审核标记」停用。
	for i := 0; i < pool.ReviewFailThreshold(); i++ {
		p.NoteContentBlocked("u1")
	}
	st, _ := p.Status("u1")
	if !st.Disabled {
		t.Fatalf("前置条件失败：应已被审核阈值停用: %+v", st)
	}
	if st.DisabledReason != pool.ReviewFailReason() {
		t.Fatalf("disabled_reason=%q want %q", st.DisabledReason, pool.ReviewFailReason())
	}

	up := &upstream.Client{
		HTTP:          srv.Client(),
		ChatBaseCN:    srv.URL,
		BillingBaseCN: srv.URL,
	}
	s := New(Config{Pool: p, Upstream: up})
	s.RunKeepaliveNow()

	st2, _ := p.Status("u1")
	if !st2.Disabled {
		t.Fatal("审核标记的号在 refresh 成功后不应被自动复活")
	}
	if st2.DisabledReason != pool.ReviewFailReason() {
		t.Errorf("复活后原因被改写: %q", st2.DisabledReason)
	}
}

// TestKeepaliveStillRevivesSessionDeadByRefresh 既有语义不受影响：
// 12153 停用的号 refresh 成功后仍会自动复活（那种情况下 refresh 正是有效证据）。
func TestKeepaliveStillRevivesSessionDeadByRefresh(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path != "/v2/plugin/auth/token/refresh" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"code": 0, "data": map[string]any{
			"accessToken": "at-new", "refreshToken": "rt-new", "expiresIn": 3600,
		}})
	}))
	defer srv.Close()

	p := pool.New("")
	a := &auth.Auth{UID: "u1", AccessToken: "old", RefreshToken: "rt", ExpiresAt: 1}
	p.Add(a)
	for i := 0; i < pool.SessionDeadThreshold(); i++ {
		p.NoteSessionDead("u1")
	}
	if st, _ := p.Status("u1"); !st.Disabled {
		t.Fatalf("前置条件失败：12153 达阈值应停用: %+v", st)
	}

	up := &upstream.Client{
		HTTP:          srv.Client(),
		ChatBaseCN:    srv.URL,
		BillingBaseCN: srv.URL,
	}
	s := New(Config{Pool: p, Upstream: up})
	s.RunKeepaliveNow()

	if st, _ := p.Status("u1"); st.Disabled {
		t.Fatal("12153 停用的号 refresh 成功应被自动复活（既有语义）")
	}
}
