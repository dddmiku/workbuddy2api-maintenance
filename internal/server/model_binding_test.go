// ═══ 更新日志 ═══
// 2026-09-22：模型绑定改为「realm + 模型名」精确匹配：裸名绑定不再跨域放行
//
//	（此前只比裸名、仅在绑定项自带 ":" 时才校验 realm，裸名绑定会漏到另一个上游域）。
//
// 2026-09-17：锁定密钥模型白名单：越界模型在选号前被拒，模型列表按绑定过滤。
package server

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"workbuddy2api/internal/apikeys"
	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/upstream"
)

func boundKeyHandler(t *testing.T, models []string) (*Handler, string, *int) {
	t.Helper()
	store, err := apikeys.Open(filepath.Join(t.TempDir(), "keys.json"), "legacy-key")
	if err != nil {
		t.Fatal(err)
	}
	calls := new(int)
	up := newFakeUpstream(t, func(string) (int, string, bool) {
		*calls++
		return http.StatusOK, sseOK, true
	})
	handler := NewHandler(Config{
		Pool: testPoolWith(
			&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999},
			// 同时备一个 global 账号：否则跨域请求会先因为「没有该域的可用账号」返回
			// 503，测到的就不是绑定校验本身了。
			&auth.Auth{UID: "g1", AccessToken: "at_gl", Domain: "www.workbuddy.ai", ExpiresAt: 9999999999},
		),
		Upstream:      up,
		APIKey:        "legacy-key",
		APIKeys:       store,
		GlobalEnabled: true,
	})
	_, key, err := store.Create("bound", "", models)
	if err != nil {
		t.Fatal(err)
	}
	return handler, key, calls
}

func TestKeyModelBindingBlocksOtherModels(t *testing.T) {
	handler, key, calls := boundKeyHandler(t, []string{"cn:deepseek-v4.1-flash"})
	invoke := func(model string) *httptest.ResponseRecorder {
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
			strings.NewReader(`{"model":"`+model+`","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
		request.Header.Set("Authorization", "Bearer "+key)
		handler.ServeHTTP(recorder, request)
		return recorder
	}
	if recorder := invoke("cn:deepseek-v4.1-flash"); recorder.Code != http.StatusOK {
		t.Fatalf("bound model rejected: %d %s", recorder.Code, recorder.Body)
	}
	if *calls != 1 {
		t.Fatalf("upstream calls=%d want 1", *calls)
	}
	recorder := invoke("cn:glm-5.2")
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("unbound model accepted: %d %s", recorder.Code, recorder.Body)
	}
	if !assertJSONErrorCode(t, recorder.Body.String(), "model_not_allowed") {
		t.Fatalf("wrong error envelope: %s", recorder.Body)
	}
	if *calls != 1 {
		t.Fatalf("blocked model reached upstream: calls=%d", *calls)
	}
}

// TestKeyModelBindingBareNameStaysInItsRealm 锁定「绑定必须匹配到具体模型」：
// 裸名按既有口径解析为 cn 域，因此放行裸名与 cn: 两种写法（同一个账号池），
// 但不放行 global: 同裸名——那是另一个上游域。此前裸名绑定会跨域放行。
func TestKeyModelBindingBareNameStaysInItsRealm(t *testing.T) {
	handler, key, calls := boundKeyHandler(t, []string{"deepseek-v4.1-flash"})
	for _, model := range []string{"cn:deepseek-v4.1-flash", "deepseek-v4.1-flash"} {
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
			strings.NewReader(`{"model":"`+model+`","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
		request.Header.Set("Authorization", "Bearer "+key)
		handler.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusOK {
			t.Fatalf("cn-side binding rejected %q: %d %s", model, recorder.Code, recorder.Body)
		}
	}
	if *calls != 2 {
		t.Fatalf("upstream calls=%d", *calls)
	}
	// 跨域必须被拒，且不能打到上游。
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"global:deepseek-v4.1-flash","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	request.Header.Set("Authorization", "Bearer "+key)
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("bare binding leaked into another realm: %d %s", recorder.Code, recorder.Body)
	}
	if !assertJSONErrorCode(t, recorder.Body.String(), "model_not_allowed") {
		t.Fatalf("wrong error envelope: %s", recorder.Body)
	}
	if *calls != 2 {
		t.Fatalf("cross-realm request reached upstream: calls=%d", *calls)
	}
}

// TestKeyModelBindingPrefixMustMatchRealm 锁定带前缀绑定的对称性：任一侧的前缀
// 不匹配都拒绝，两个域都要放开就必须各写一条。
func TestKeyModelBindingPrefixMustMatchRealm(t *testing.T) {
	for _, tc := range []struct {
		bound   string
		request string
		allowed bool
	}{
		{"cn:deepseek-v4.1-flash", "cn:deepseek-v4.1-flash", true},
		{"cn:deepseek-v4.1-flash", "global:deepseek-v4.1-flash", false},
		{"global:deepseek-v4.1-flash", "global:deepseek-v4.1-flash", true},
		{"global:deepseek-v4.1-flash", "cn:deepseek-v4.1-flash", false},
		// 裸名绑定 == cn 域绑定。
		{"deepseek-v4.1-flash", "cn:deepseek-v4.1-flash", true},
		{"deepseek-v4.1-flash", "deepseek-v4.1-flash", true},
		{"deepseek-v4.1-flash", "global:deepseek-v4.1-flash", false},
		{"cn:deepseek-v4.1-flash", "deepseek-v4.1-flash", true},
	} {
		t.Run(tc.bound+"<-"+tc.request, func(t *testing.T) {
			handler, key, _ := boundKeyHandler(t, []string{tc.bound})
			recorder := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
				strings.NewReader(`{"model":"`+tc.request+`","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
			request.Header.Set("Authorization", "Bearer "+key)
			handler.ServeHTTP(recorder, request)
			if tc.allowed && recorder.Code != http.StatusOK {
				t.Fatalf("bound=%q request=%q should be allowed: %d %s",
					tc.bound, tc.request, recorder.Code, recorder.Body)
			}
			if !tc.allowed && recorder.Code != http.StatusForbidden {
				t.Fatalf("bound=%q request=%q should be forbidden: %d %s",
					tc.bound, tc.request, recorder.Code, recorder.Body)
			}
		})
	}
}

// TestKeyModelBindingBothRealmsRequiresBothEntries 说明「两个域都放开」的正确写法：
// 必须显式写两条，一条裸名绑定不再同时覆盖两个域。
func TestKeyModelBindingBothRealmsRequiresBothEntries(t *testing.T) {
	handler, key, _ := boundKeyHandler(t, []string{"cn:deepseek-v4.1-flash", "global:deepseek-v4.1-flash"})
	for _, model := range []string{"cn:deepseek-v4.1-flash", "global:deepseek-v4.1-flash"} {
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
			strings.NewReader(`{"model":"`+model+`","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
		request.Header.Set("Authorization", "Bearer "+key)
		handler.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusOK {
			t.Fatalf("explicit two-realm binding rejected %q: %d %s", model, recorder.Code, recorder.Body)
		}
	}
}

func TestKeyModelBindingFiltersModelList(t *testing.T) {
	resetModelsCache()
	t.Cleanup(resetModelsCache)
	dynamicModelsCache.Lock()
	dynamicModelsCache.ids = []upstream.ModelInfo{
		{ID: "deepseek-v4.1-flash", ContextWindow: 131072, MaxTokens: 8192},
		{ID: "glm-5.2", ContextWindow: 131072, MaxTokens: 8192},
	}
	dynamicModelsCache.fetched = time.Now()
	dynamicModelsCache.Unlock()
	handler, key, _ := boundKeyHandler(t, []string{"deepseek-v4.1-flash"})
	request := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	request.Header.Set("Authorization", "Bearer "+key)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("models status=%d", recorder.Code)
	}
	if !strings.Contains(recorder.Body.String(), "deepseek-v4.1-flash") {
		t.Fatalf("bound model missing from list: %s", recorder.Body)
	}
	if strings.Contains(recorder.Body.String(), "glm-5.2") {
		t.Fatalf("unbound model leaked into list: %s", recorder.Body)
	}
}

func TestKeyWithoutBindingKeepsFullAccess(t *testing.T) {
	handler, key, _ := boundKeyHandler(t, nil)
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"cn:glm-5.2","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	request.Header.Set("Authorization", "Bearer "+key)
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("unbound key restricted: %d %s", recorder.Code, recorder.Body)
	}
}
