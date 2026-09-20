// ═══ 更新日志 ═══
// 2026-09-20：覆盖密钥有效期的默认值、创建/修改、到期拒绝与非法输入边界。
package apikeys

import (
	"encoding/json"
	"errors"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func createKey(t *testing.T, s *Store, body string) (int, map[string]any) {
	t.Helper()
	w := httptest.NewRecorder()
	s.AdminHandler().ServeHTTP(w, httptest.NewRequest("POST", "/keys", strings.NewReader(body)))
	var result map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &result)
	return w.Code, result
}

func patchKey(t *testing.T, s *Store, id, body string) (int, map[string]any) {
	t.Helper()
	w := httptest.NewRecorder()
	s.AdminHandler().ServeHTTP(w, httptest.NewRequest("PATCH", "/keys/"+id, strings.NewReader(body)))
	var result map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &result)
	return w.Code, result
}

// TestKeyExpiryDefaultsToUnlimited 既有密钥与新密钥的默认都是无限制。
func TestKeyExpiryDefaultsToUnlimited(t *testing.T) {
	path := filepath.Join(t.TempDir(), "keys.json")
	s, err := Open(path, "legacy-key")
	if err != nil {
		t.Fatal(err)
	}
	legacy := s.List()
	if len(legacy) != 1 || legacy[0].ExpiresAt != nil {
		t.Fatalf("existing key must stay unlimited: %+v", legacy)
	}
	code, created := createKey(t, s, `{"name":"fresh"}`)
	if code != 201 {
		t.Fatalf("create=%d", code)
	}
	entry, _ := created["entry"].(map[string]any)
	if entry["expires_at"] != nil {
		t.Fatalf("new key must default to unlimited: %v", entry["expires_at"])
	}
	key, _ := created["key"].(string)
	if _, ok := s.Resolve(key); !ok {
		t.Fatal("unlimited key was rejected")
	}
}

// TestKeyExpiryRoundTripAndEnforcement 有效期能保存、重启后仍生效，到期即拒绝鉴权。
func TestKeyExpiryRoundTripAndEnforcement(t *testing.T) {
	path := filepath.Join(t.TempDir(), "keys.json")
	s, err := Open(path, "")
	if err != nil {
		t.Fatal(err)
	}
	soon := time.Now().UTC().Add(30 * 24 * time.Hour).Truncate(time.Second)
	code, created := createKey(t, s, `{"name":"timed","expires_at":"`+soon.Format(time.RFC3339)+`"}`)
	if code != 201 {
		t.Fatalf("create=%d body=%v", code, created)
	}
	key, _ := created["key"].(string)
	entry, _ := created["entry"].(map[string]any)
	if entry["expires_at"] != soon.Format(time.RFC3339) {
		t.Fatalf("expiry not returned: %v", entry["expires_at"])
	}
	reopened, err := Open(path, "")
	if err != nil {
		t.Fatal(err)
	}
	info, ok := reopened.Resolve(key)
	if !ok || info.ExpiresAt == nil || !info.ExpiresAt.Equal(soon) {
		t.Fatalf("expiry did not survive restart: %+v ok=%t", info.ExpiresAt, ok)
	}

	// 直接把到期时间改到过去，模拟时间流逝；鉴权必须拒绝且状态可区分。
	past := time.Now().UTC().Add(-time.Hour)
	reopened.mu.Lock()
	reopened.keys[0].ExpiresAt = &past
	reopened.mu.Unlock()
	if reopened.Authenticate(key) {
		t.Fatal("expired key was accepted")
	}
	if _, status := reopened.Lookup(key); status != StatusExpired {
		t.Fatalf("expired key reported status=%v", status)
	}
}

// TestKeyExpiryUpdateAndClear 修改有效期与显式清空为无限制。
func TestKeyExpiryUpdateAndClear(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "keys.json"), "")
	if err != nil {
		t.Fatal(err)
	}
	code, created := createKey(t, s, `{"name":"client"}`)
	if code != 201 {
		t.Fatalf("create=%d", code)
	}
	entry, _ := created["entry"].(map[string]any)
	id, _ := entry["id"].(string)
	key, _ := created["key"].(string)

	later := time.Now().UTC().Add(90 * 24 * time.Hour).Truncate(time.Second)
	if code, body := patchKey(t, s, id, `{"expires_at":"`+later.Format(time.RFC3339)+`"}`); code != 200 {
		t.Fatalf("set expiry=%d %v", code, body)
	}
	if info, ok := s.Resolve(key); !ok || info.ExpiresAt == nil {
		t.Fatal("expiry update did not take effect")
	}
	// 只改名称时不能顺手清掉有效期。
	if code, body := patchKey(t, s, id, `{"name":"renamed"}`); code != 200 {
		t.Fatalf("rename=%d %v", code, body)
	}
	if info, _ := s.Resolve(key); info.ExpiresAt == nil {
		t.Fatal("unrelated update cleared the expiry")
	}
	// 显式 null 表示改回无限制。
	if code, body := patchKey(t, s, id, `{"expires_at":null}`); code != 200 {
		t.Fatalf("clear expiry=%d %v", code, body)
	}
	if info, _ := s.Resolve(key); info.ExpiresAt != nil {
		t.Fatal("explicit null did not restore unlimited")
	}
}

// TestKeyExpiryRejectsInvalidInput 过去时间、超长年限、错误格式与错误类型都必须拒绝。
func TestKeyExpiryRejectsInvalidInput(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "keys.json"), "")
	if err != nil {
		t.Fatal(err)
	}
	code, created := createKey(t, s, `{"name":"client"}`)
	if code != 201 {
		t.Fatalf("create=%d", code)
	}
	entry, _ := created["entry"].(map[string]any)
	id, _ := entry["id"].(string)

	past := time.Now().UTC().Add(-time.Minute).Format(time.RFC3339)
	far := time.Now().UTC().AddDate(20, 0, 0).Format(time.RFC3339)
	for _, tc := range []struct{ name, body string }{
		{"create-past", `{"name":"x","expires_at":"` + past + `"}`},
		{"create-far", `{"name":"x","expires_at":"` + far + `"}`},
		{"create-garbage", `{"name":"x","expires_at":"tomorrow"}`},
		{"create-wrong-type", `{"name":"x","expires_at":123}`},
		{"update-past", `{"expires_at":"` + past + `"}`},
		{"update-far", `{"expires_at":"` + far + `"}`},
		{"update-garbage", `{"expires_at":"not-a-time"}`},
		{"update-wrong-type", `{"expires_at":[]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var code int
			if strings.HasPrefix(tc.name, "create") {
				code, _ = createKey(t, s, tc.body)
			} else {
				code, _ = patchKey(t, s, id, tc.body)
			}
			if code != 400 {
				t.Fatalf("invalid expiry accepted: HTTP %d", code)
			}
		})
	}
	if _, _, err := s.Create("x", "", nil, Options{ExpiresAt: &time.Time{}, ExpiresAtSet: true}); !errors.Is(err, ErrInvalidExpiry) {
		t.Fatalf("store accepted zero expiry: %v", err)
	}
}
