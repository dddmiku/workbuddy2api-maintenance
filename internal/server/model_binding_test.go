// ═══ 更新日志 ═══
// 2026-09-22：模型绑定改为「完整模型名逐字相等」，不做任何前缀解析或降级匹配：
//
//	绑定 cn:glm-5.2 只放行 cn:glm-5.2，不放行 global:glm-5.2，也不放行裸名 glm-5.2。
//
// 2026-09-17：锁定密钥模型白名单：越界模型在选号前被拒，模型列表按绑定过滤。
package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"workbuddy2api/internal/apikeys"
	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/upstream"
)

// legacyBareBindingHandler 造一把「绑定里存了裸名」的密钥。
//
// 写入路径现在会拒掉裸名，但历史文件里可能已经存过；读取时故意不校验，
// 所以这里直接改盘上的记录再重开，模拟那种存量密钥，用它验证鉴权侧
// 也不会把裸名当成 cn 域去放行 cn: 请求。
func legacyBareBindingHandler(t *testing.T, bareModels []string) (*Handler, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "keys.json")
	store, err := apikeys.Open(path, "legacy-key")
	if err != nil {
		t.Fatal(err)
	}
	_, key, err := store.Create("legacy-bare", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	keys, _ := doc["keys"].([]any)
	// Open 会顺带建一条 legacy 记录（ID "legacy"），只改我们新建的那把。
	var entry map[string]any
	for _, item := range keys {
		candidate, _ := item.(map[string]any)
		if candidate["name"] == "legacy-bare" {
			entry = candidate
			break
		}
	}
	if entry == nil {
		t.Fatalf("created key not found in %d records", len(keys))
	}
	entry["models"] = bareModels
	encoded, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	reopened, err := apikeys.Open(path, "legacy-key")
	if err != nil {
		t.Fatal(err)
	}
	info, ok := reopened.Resolve(key)
	if !ok || len(info.Models) != len(bareModels) {
		t.Fatalf("legacy bare binding was not preserved: %+v", info.Models)
	}
	calls := new(int)
	up := newFakeUpstream(t, func(string) (int, string, bool) {
		*calls++
		return http.StatusOK, sseOK, true
	})
	handler := NewHandler(Config{
		Pool: testPoolWith(
			&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999},
			&auth.Auth{UID: "g1", AccessToken: "at_gl", Domain: "www.workbuddy.ai", ExpiresAt: 9999999999},
		),
		Upstream:      up,
		APIKey:        "legacy-key",
		APIKeys:       reopened,
		GlobalEnabled: true,
	})
	return handler, key
}

func invokeChat(t *testing.T, handler *Handler, key, model string) *httptest.ResponseRecorder {
	t.Helper()
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"`+model+`","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	request.Header.Set("Authorization", "Bearer "+key)
	handler.ServeHTTP(recorder, request)
	return recorder
}

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

// TestKeyModelBindingIsExactFullName 锁定「绑定必须填完整模型名，且逐字匹配」：
// 任何一侧缺前缀、或前缀不同域，都算不命中。此前两版实现都有降级——
// 最初只比裸名（裸名绑定跨域放行），后来按 resolveModel 解析后再比
// （裸名绑定被静默扩成 cn: 域的两个名字）。两者都让实际可用范围大于
// 管理员写下的那一条，所以现在按字面比对。
func TestKeyModelBindingIsExactFullName(t *testing.T) {
	for _, tc := range []struct {
		bound   string
		request string
		allowed bool
	}{
		{"cn:deepseek-v4.1-flash", "cn:deepseek-v4.1-flash", true},
		{"cn:deepseek-v4.1-flash", "global:deepseek-v4.1-flash", false},
		{"cn:deepseek-v4.1-flash", "deepseek-v4.1-flash", false},
		{"global:deepseek-v4.1-flash", "global:deepseek-v4.1-flash", true},
		{"global:deepseek-v4.1-flash", "cn:deepseek-v4.1-flash", false},
		{"global:deepseek-v4.1-flash", "deepseek-v4.1-flash", false},
		// 裸名绑定只匹配裸名请求，不因为裸名归 cn 域就顺带放行 cn: 请求。
		{"deepseek-v4.1-flash", "deepseek-v4.1-flash", true},
		{"deepseek-v4.1-flash", "cn:deepseek-v4.1-flash", false},
		{"deepseek-v4.1-flash", "global:deepseek-v4.1-flash", false},
		// 前缀相同但模型名不同，不能命中。
		{"cn:deepseek-v4.1-flash", "cn:glm-5.2", false},
	} {
		t.Run(tc.bound+"<-"+tc.request, func(t *testing.T) {
			// 裸名绑定已经写不进去了，用历史文件里的存量密钥来验证鉴权侧的行为。
			var handler *Handler
			var key string
			if strings.Contains(tc.bound, ":") {
				handler, key, _ = boundKeyHandler(t, []string{tc.bound})
			} else {
				handler, key = legacyBareBindingHandler(t, []string{tc.bound})
			}
			recorder := invokeChat(t, handler, key, tc.request)
			if tc.allowed && recorder.Code != http.StatusOK {
				t.Fatalf("bound=%q request=%q should be allowed: %d %s",
					tc.bound, tc.request, recorder.Code, recorder.Body)
			}
			if !tc.allowed && recorder.Code != http.StatusForbidden {
				t.Fatalf("bound=%q request=%q should be forbidden: %d %s",
					tc.bound, tc.request, recorder.Code, recorder.Body)
			}
			if !tc.allowed && !assertJSONErrorCode(t, recorder.Body.String(), "model_not_allowed") {
				t.Fatalf("wrong error envelope: %s", recorder.Body)
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
	// 上游模型对象给的是裸 ID，modelList 统一加 "cn:" 前缀后对外；绑定必须写
	// 这个对外名字，所以下面断言的是带前缀的形式。
	dynamicModelsCache.ids = []upstream.ModelInfo{
		{ID: "deepseek-v4.1-flash", ContextWindow: 131072, MaxTokens: 8192},
		{ID: "glm-5.2", ContextWindow: 131072, MaxTokens: 8192},
	}
	dynamicModelsCache.fetched = time.Now()
	dynamicModelsCache.Unlock()
	handler, key, _ := boundKeyHandler(t, []string{"cn:deepseek-v4.1-flash"})
	request := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	request.Header.Set("Authorization", "Bearer "+key)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("models status=%d", recorder.Code)
	}
	if !strings.Contains(recorder.Body.String(), "cn:deepseek-v4.1-flash") {
		t.Fatalf("bound model missing from list: %s", recorder.Body)
	}
	for _, leaked := range []string{"cn:glm-5.2"} {
		if strings.Contains(recorder.Body.String(), leaked) {
			t.Fatalf("unbound model %q leaked into list: %s", leaked, recorder.Body)
		}
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
