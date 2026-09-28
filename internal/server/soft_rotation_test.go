// ═══ 更新日志 ═══
// 2026-09-28：锁定「内容审核 / 未知 4xx 先换号再试」：换号成功则客户端无感，试满才透传上游原文。
package server

import (
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
	"workbuddy2api/internal/session"

	"workbuddy2api/internal/auth"
)

const contentReviewBody = `{"code":11140,"msg":"request illegal","requestId":"af11e881","displayMsg":{"en":"The content did not pass the safety review. Please adjust and retry.","zh":"内容未通过安全审核，请修改后重试。"}}`

func softRotationFixture(t *testing.T, behavior func(uid string) (int, string, bool)) (*Handler, *[]string) {
	t.Helper()
	var mu sync.Mutex
	var seen []string
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		uid := strings.TrimPrefix(authz, "Bearer ")
		mu.Lock()
		seen = append(seen, uid)
		mu.Unlock()
		return behavior(uid)
	})
	pool := testPoolWith(
		&auth.Auth{UID: "a", AccessToken: "a", ExpiresAt: 9999999999},
		&auth.Auth{UID: "b", AccessToken: "b", ExpiresAt: 9999999999},
		&auth.Auth{UID: "c", AccessToken: "c", ExpiresAt: 9999999999},
	)
	pool.SetCredits("a", 3000)
	pool.SetCredits("b", 2000)
	pool.SetCredits("c", 1000)
	return NewHandler(Config{Pool: pool, Upstream: up, MaxSoftRotations: 2, RotateOnClientError: true}), &seen
}

func TestContentReviewRotatesToAnotherAccount(t *testing.T) {
	h, seen := softRotationFixture(t, func(uid string) (int, string, bool) {
		if uid == "a" || uid == "b" {
			return 403, contentReviewBody, false
		}
		return 200, sseOK, true
	})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[]}`)))
	if rec.Code != 200 {
		t.Fatalf("换号后应成功: code=%d body=%s", rec.Code, rec.Body)
	}
	if len(*seen) != 3 {
		t.Fatalf("应依次尝试 3 个账号，实际 %v", *seen)
	}
}

func TestContentReviewSurfacesAfterRotationsExhausted(t *testing.T) {
	h, seen := softRotationFixture(t, func(string) (int, string, bool) {
		return 403, contentReviewBody, false
	})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[]}`)))
	if rec.Code != 400 || !strings.Contains(rec.Body.String(), "safety review") {
		t.Fatalf("试满后应透传上游原文: code=%d body=%s", rec.Code, rec.Body)
	}
	if len(*seen) != 1+2 {
		t.Fatalf("应尝试 %d 个账号，实际 %v", 1+2, *seen)
	}
}

func TestUnknownClientErrorRotatesBeforeFailing(t *testing.T) {
	h, seen := softRotationFixture(t, func(uid string) (int, string, bool) {
		if uid == "a" {
			return 400, `{"code":12345,"msg":"unexpected account state"}`, false
		}
		return 200, sseOK, true
	})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[]}`)))
	if rec.Code != 200 {
		t.Fatalf("未知 4xx 换号后应成功: code=%d body=%s", rec.Code, rec.Body)
	}
	if len(*seen) != 2 {
		t.Fatalf("应先试 a 再换 b，实际 %v", *seen)
	}
}

// 粘性会话下也必须真的换号：被风控标记的号否则会被反复选中（线上实测）。
func TestContentReviewRotationUnbindsStickySession(t *testing.T) {
	var mu sync.Mutex
	var seen []string
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		uid := strings.TrimPrefix(authz, "Bearer ")
		mu.Lock()
		seen = append(seen, uid)
		mu.Unlock()
		if uid == "sticky-a" {
			return 403, contentReviewBody, false
		}
		return 200, sseOK, true
	})
	pool := testPoolWith(
		&auth.Auth{UID: "sticky-a", AccessToken: "sticky-a", ExpiresAt: 9999999999},
		&auth.Auth{UID: "sticky-b", AccessToken: "sticky-b", ExpiresAt: 9999999999},
	)
	h := NewHandler(Config{
		Pool:             pool,
		Upstream:         up,
		MaxSoftRotations: 2,
		Session:          session.New(session.Config{TTL: time.Hour, Store: newBindStore(), Available: pool.AvailableUIDs}),
	})
	// 先让会话粘到 sticky-a（首次请求成功不了，但绑定在失败前已建立）。
	body := `{"model":"cn:hy3","stream":true,"metadata":{"conversation_id":"poisoned"},"messages":[{"role":"user","content":"OK"}]}`
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body)))
	if rec.Code != 200 {
		t.Fatalf("换号后应成功: %d %s", rec.Code, rec.Body)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(seen) < 2 {
		t.Fatalf("应至少尝试两个账号，实际 %v", seen)
	}
	if seen[0] == seen[1] {
		t.Fatalf("重试没有真正换号（粘性未解绑）: %v", seen)
	}
}
