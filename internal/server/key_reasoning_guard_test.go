// ═══ 更新日志 ═══
// 2026-09-20：验证重复推理保护按调用密钥独立开关，并在四种协议出口及后续请求中生效。
// 2026-09-20：改用可重发的上游夹具，覆盖「命中后同账号重发一次」的新语义。
package server

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"workbuddy2api/internal/apikeys"
)

func setKeyReasoningGuard(t *testing.T, store *apikeys.Store, id string, enabled bool) {
	t.Helper()
	w := httptest.NewRecorder()
	store.AdminHandler().ServeHTTP(w, httptest.NewRequest("PATCH", "/keys/"+id,
		strings.NewReader(fmt.Sprintf(`{"reasoning_loop_guard":%t}`, enabled))))
	if w.Code != http.StatusOK {
		t.Fatalf("cannot change key guard: HTTP %d", w.Code)
	}
}

func TestKeyReasoningGuardFourExitsAndLiveChanges(t *testing.T) {
	for _, path := range []string{"/v1/chat/completions", "/v1/responses"} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/%t", path, stream), func(t *testing.T) {
				payload := reasoningGuardFinish(reasoningGuardFrame(map[string]any{"reasoning_content": reasoningGuardLines(400)}), "stop")
				h, _, _, _, _ := reasoningGuardFixtureSequence(t, []string{payload}, "cn:deepseek-v4.1-flash")
				store, err := apikeys.Open(filepath.Join(t.TempDir(), "keys.json"), "")
				if err != nil {
					t.Fatal(err)
				}
				on, keyOn, err := store.Create("on", "", nil)
				if err != nil {
					t.Fatal(err)
				}
				off, keyOff, err := store.Create("off", "", nil)
				if err != nil {
					t.Fatal(err)
				}
				h.cfg.APIKeys = store
				h.cfg.Upstream.HTTP.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
					return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}},
						Body: &reasoningGuardBody{Reader: strings.NewReader(payload)}}, nil
				})
				setKeyReasoningGuard(t, store, off.ID, false)
				request := func(key string, guarded bool) {
					r := reasoningGuardRequest(path, "cn:deepseek-v4.1-flash", stream, false)
					r.Header.Set("Authorization", "Bearer "+key)
					w := httptest.NewRecorder()
					h.ServeHTTP(w, r)
					if guarded {
						assertReasoningGuardFailure(t, w, path, stream)
					} else {
						if w.Code != 200 || !strings.Contains(w.Body.String(), "after-guard-marker") || strings.Contains(w.Body.String(), "upstream_reasoning_loop") {
							t.Fatalf("disabled key was interrupted: status=%d", w.Code)
						}
					}
				}
				request(keyOn, true)
				request(keyOff, false)
				setKeyReasoningGuard(t, store, off.ID, true)
				request(keyOff, true)
				setKeyReasoningGuard(t, store, on.ID, false)
				request(keyOn, false)
				if !store.Authenticate(keyOn) || !store.Authenticate(keyOff) {
					t.Fatal("guard toggle changed credential enable state")
				}
			})
		}
	}
}

func TestKeyReasoningGuardOverridesServerDefault(t *testing.T) {
	payload := reasoningGuardFinish(reasoningGuardFrame(map[string]any{"reasoning_content": reasoningGuardLines(400)}), "stop")
	for _, override := range []bool{false, true} {
		t.Run(fmt.Sprintf("override=%t", override), func(t *testing.T) {
			h, _, _, _, _ := reasoningGuardFixtureSequence(t, []string{payload}, "cn:deepseek-v4.1-flash")
			defaultOff := false
			h.cfg.ReasoningLoopGuard = &defaultOff
			store, err := apikeys.Open(filepath.Join(t.TempDir(), "keys.json"), "")
			if err != nil {
				t.Fatal(err)
			}
			info, key, err := store.Create("caller", "", nil)
			if err != nil {
				t.Fatal(err)
			}
			if override {
				setKeyReasoningGuard(t, store, info.ID, true)
			}
			h.cfg.APIKeys = store
			r := reasoningGuardRequest("/v1/chat/completions", "cn:deepseek-v4.1-flash", false, false)
			r.Header.Set("Authorization", "Bearer "+key)
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if override {
				assertReasoningGuardFailure(t, w, "/v1/chat/completions", false)
			} else if w.Code != 200 || !strings.Contains(w.Body.String(), "after-guard-marker") {
				t.Fatal("unset key did not follow the server default")
			}
		})
	}
}
