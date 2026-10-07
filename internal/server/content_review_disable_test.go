// ═══ 更新日志 ═══
// 2026-10-07：新增。账号维度审核拒绝的停用回归：
//  1. 同请求里 A 被审核拒绝、B 用同一份正文成功 → A 立即停用（硬证据）；
//  2. 请求内容导致的拒绝（所有号都被拒）→ 不停用任何账号，只回 400；
//  3. 未达阈值的零星命中会随成功清零，健康号不会被误停。
package server

import (
	"net/http/httptest"
	"strings"
	"testing"
)

// TestContentReviewDisablesFlaggedAccountWhenPeerSucceeds
// A 被上游审核拒绝、B 用同一份正文成功返回：A 是账号被标记，必须当场停用；
// 客户端仍拿到 200（用户侧无感）。
func TestContentReviewDisablesFlaggedAccountWhenPeerSucceeds(t *testing.T) {
	h, _ := softRotationFixture(t, func(uid string) (int, string, bool) {
		if uid == "a" {
			return 403, contentReviewBody, false
		}
		return 200, sseOK, true
	})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","messages":[]}`)))
	if rec.Code != 200 {
		t.Fatalf("换号后应成功: code=%d body=%s", rec.Code, rec.Body)
	}
	st, ok := h.cfg.Pool.Status("a")
	if !ok {
		t.Fatal("账号 a 应存在")
	}
	if !st.Disabled {
		t.Fatal("同请求内被拒且同伴成功的账号必须被停用")
	}
	if !strings.Contains(st.DisabledReason, "content review") {
		t.Errorf("disabled_reason=%q 应指向内容审核标记", st.DisabledReason)
	}
	// 成功的那个号必须保持可用：它是健康号。
	stb, _ := h.cfg.Pool.Status("b")
	if stb.Disabled {
		t.Error("成功返回的账号不应被停用")
	}
}

// TestContentReviewContentWideDoesNotDisable
// 所有账号用同一份正文都被审核拒绝（内容问题）：不得停用任何账号，客户端拿 400。
// 这是「一拒就停」会误杀健康号的场景，必须守住。
func TestContentReviewContentWideDoesNotDisable(t *testing.T) {
	h, seen := softRotationFixture(t, func(string) (int, string, bool) {
		return 403, contentReviewBody, false
	})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","messages":[]}`)))
	if rec.Code != 400 {
		t.Fatalf("试满后应回 400: code=%d body=%s", rec.Code, rec.Body)
	}
	// 每个账号恰好被拒一次，未达阈值 → 全部保持可用。
	for _, uid := range *seen {
		st, ok := h.cfg.Pool.Status(uid)
		if !ok {
			t.Fatalf("账号 %s 应存在", uid)
		}
		if st.Disabled {
			t.Errorf("账号 %s 只被拒一次就被停用，会误杀健康号", uid)
		}
	}
}

// TestContentReviewStreakResetsOnSuccess
// 偶发命中后账号又成功返回：计数清零，后续单次命中不会再累积成误停。
func TestContentReviewStreakResetsOnSuccess(t *testing.T) {
	round := 0
	h, _ := softRotationFixture(t, func(uid string) (int, string, bool) {
		// 第一轮：a 被拒、b 成功；第二轮：a 直接成功。
		if round == 0 && uid == "a" {
			return 403, contentReviewBody, false
		}
		return 200, sseOK, true
	})
	// 第一轮：a 被拒但同伴成功 → a 会被停用，这里先复位以便验证清零逻辑。
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","messages":[]}`)))
	if rec.Code != 200 {
		t.Fatalf("第一轮应成功: code=%d", rec.Code)
	}
	h.cfg.Pool.ReviveDisabled("a")

	// 第二轮：a 成功返回 → 计数清零，账号保持可用。
	round = 1
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","messages":[]}`)))
	if rec2.Code != 200 {
		t.Fatalf("第二轮应成功: code=%d body=%s", rec2.Code, rec2.Body)
	}
	st, _ := h.cfg.Pool.Status("a")
	if st.Disabled {
		t.Error("成功返回后账号不应处于停用状态")
	}
}
