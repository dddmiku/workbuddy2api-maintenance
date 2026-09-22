// ═══ 更新日志 ═══
// 2026-09-22：补副本主密钥缓存的边界回归：轮换/删除主密钥后不能被缓存掩盖，
//
//	鉴权与复制仍按磁盘真实状态判定。
//
// 2026-09-20：验证完整密钥按需复制、加密持久化、旧密钥补齐及跨实例撤销边界。
package apikeys

import (
	"encoding/json"
	"errors"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func copyKeyResponse(t *testing.T, s *Store, id string) (int, map[string]any) {
	t.Helper()
	r := httptest.NewRequest("POST", "/keys/"+id+"/copy", strings.NewReader("{}"))
	w := httptest.NewRecorder()
	s.AdminHandler().ServeHTTP(w, r)
	var result map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Errorf("copy did not return a JSON result: HTTP %d", w.Code)
	}
	if w.Code == 200 && w.Header().Get("Cache-Control") != "no-store" {
		t.Error("secret response must not be cached")
	}
	return w.Code, result
}

func TestCopyKeySurvivesRestartWithoutPlaintextStorage(t *testing.T) {
	path := filepath.Join(t.TempDir(), "keys.json")
	s, err := Open(path, "")
	if err != nil {
		t.Fatal(err)
	}
	info, secret, err := s.Create("copyable", "", []string{"global:hy3"})
	if err != nil {
		t.Fatal(err)
	}
	s, err = Open(path, "")
	if err != nil {
		t.Fatal(err)
	}
	status, result := copyKeyResponse(t, s, info.ID)
	if status != 200 || result["key"] != secret {
		t.Fatalf("created key cannot be copied after restart: HTTP %d", status)
	}
	listed, _ := json.Marshal(s.List())
	if strings.Contains(string(listed), secret) || strings.Contains(string(listed), "encrypted_key") {
		t.Fatal("key list disclosed secret material")
	}
	for _, name := range []string{path, path + ".enc-key"} {
		raw, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(raw), secret) {
			t.Fatal("plaintext key persisted")
		}
		info, _ := os.Stat(name)
		if runtime.GOOS != "windows" && info.Mode().Perm() != 0600 {
			t.Fatal("secret file permissions are not 0600")
		}
	}
}

func TestLegacyKeyBecomesCopyableOnValidAuthentication(t *testing.T) {
	path := filepath.Join(t.TempDir(), "keys.json")
	const secret = "old-user-key-that-must-not-be-rotated"
	s, err := Open(path, secret)
	if err != nil {
		t.Fatal(err)
	}
	if status, _ := copyKeyResponse(t, s, "legacy"); status != 409 {
		t.Fatalf("uncaptured legacy copy status=%d want 409", status)
	}
	if s.Authenticate("wrong-key") {
		t.Fatal("wrong key accepted")
	}
	if status, _ := copyKeyResponse(t, s, "legacy"); status != 409 {
		t.Fatal("invalid authentication populated a secret")
	}
	if !s.Authenticate(secret) {
		t.Fatal("valid legacy key stopped working")
	}
	status, result := copyKeyResponse(t, s, "legacy")
	if status != 200 || result["key"] != secret {
		t.Fatal("authenticated old key did not become copyable")
	}
	if len(s.List()) != 1 || s.List()[0].ID != "legacy" {
		t.Fatal("legacy identity changed")
	}
}

func TestCopyDoesNotEnableDisabledOrRestoreDeletedKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "keys.json")
	a, err := Open(path, "")
	if err != nil {
		t.Fatal(err)
	}
	info, secret, err := a.Create("client", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	b, err := Open(path, "")
	if err != nil {
		t.Fatal(err)
	}
	disabled := false
	if _, err := b.Update(info.ID, nil, nil, &disabled, nil); err != nil {
		t.Fatal(err)
	}
	status, result := copyKeyResponse(t, a, info.ID)
	if status != 200 || result["key"] != secret || a.Authenticate(secret) {
		t.Fatal("copy changed a disabled key's state")
	}
	if err := b.Delete(info.ID); err != nil {
		t.Fatal(err)
	}
	if status, _ := copyKeyResponse(t, a, info.ID); status != 404 {
		t.Fatal("deleted key could still be copied")
	}
}

func TestMissingEncryptionKeyDoesNotBreakAuthentication(t *testing.T) {
	path := filepath.Join(t.TempDir(), "keys.json")
	s, err := Open(path, "")
	if err != nil {
		t.Fatal(err)
	}
	info, secret, err := s.Create("client", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path + ".enc-key"); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path, "")
	if err != nil {
		t.Fatal(err)
	}
	if !s.Authenticate(secret) {
		t.Fatal("copy storage failure blocked valid authentication")
	}
	if status, result := copyKeyResponse(t, s, info.ID); status != 409 || result["key"] != nil {
		t.Fatal("missing encryption key did not fail copying safely")
	}
	if _, err := os.Stat(path + ".enc-key"); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("missing master key was silently replaced")
	}
}

// TestVaultCacheDoesNotMaskRotatedOrRemovedKey 锁定缓存的两个边界：
//   - 主密钥被换掉后，旧缓存不能继续解密成功（否则会误报「可复制」）；
//   - 主密钥被删掉后，缓存不能让复制继续成功。
//
// 缓存只省 I/O，不改变判定口径：换掉主密钥后 AEAD 认证必然失败。
func TestVaultCacheDoesNotMaskRotatedOrRemovedKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "keys.json")
	s, err := Open(path, "")
	if err != nil {
		t.Fatal(err)
	}
	info, secret, err := s.Create("cached", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	// 先跑一次让缓存填充并确认可复制。
	if status, result := copyKeyResponse(t, s, info.ID); status != 200 || result["key"] != secret {
		t.Fatalf("baseline copy failed: HTTP %d", status)
	}

	// 1) 换掉主密钥：缓存必须失效（AEAD 认证失败），不能继续报「可复制」。
	rotated := make([]byte, 32)
	for i := range rotated {
		rotated[i] = byte(i + 1)
	}
	if err := os.WriteFile(path+".enc-key", rotated, 0600); err != nil {
		t.Fatal(err)
	}
	// 绕过 TTL 直接推进核对时间，模拟 TTL 到期后的重新读盘。
	s.mu.Lock()
	s.vaultCheckedAt = time.Time{}
	s.mu.Unlock()
	if status, result := copyKeyResponse(t, s, info.ID); status != 409 || result["key"] != nil {
		t.Fatalf("rotated master key still decrypted a copy: HTTP %d %v", status, result["key"])
	}
	// 鉴权走摘要，不受副本损坏影响。
	if !s.Authenticate(secret) {
		t.Fatal("rotated copy key broke digest authentication")
	}

	// 2) 删掉主密钥：缓存不能让它继续复制成功。
	if err := os.Remove(path + ".enc-key"); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	s.vaultCheckedAt = time.Time{}
	s.mu.Unlock()
	if status, result := copyKeyResponse(t, s, info.ID); status != 409 || result["key"] != nil {
		t.Fatalf("removed master key still decrypted a copy: HTTP %d", status)
	}
	if !s.Authenticate(secret) {
		t.Fatal("removed copy key broke digest authentication")
	}
}

// TestVaultCacheSurvivesRepeatedAuthWithoutStaleData 验证缓存不会返回陈旧策略：
// 反复鉴权后再改密钥，副本与模型绑定都必须立刻反映最新状态。
func TestVaultCacheSurvivesRepeatedAuthWithoutStaleData(t *testing.T) {
	path := filepath.Join(t.TempDir(), "keys.json")
	s, err := Open(path, "")
	if err != nil {
		t.Fatal(err)
	}
	info, secret, err := s.Create("cached", "", []string{"cn:before"})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 50; i++ {
		if _, ok := s.Resolve(secret); !ok {
			t.Fatal("cached lookup lost a valid key")
		}
	}
	models := []string{"cn:after"}
	if _, err := s.Update(info.ID, nil, nil, nil, &models); err != nil {
		t.Fatal(err)
	}
	got, ok := s.Resolve(secret)
	if !ok || len(got.Models) != 1 || got.Models[0] != "cn:after" {
		t.Fatalf("cached lookup returned stale policy: %+v", got.Models)
	}
	if status, result := copyKeyResponse(t, s, info.ID); status != 200 || result["key"] != secret {
		t.Fatalf("cached lookup broke copying: HTTP %d", status)
	}
}

func TestSecretCaptureWriteFailureDoesNotBlockAuthentication(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "keys.json"), "old-secret-keep-working")
	if err != nil {
		t.Fatal(err)
	}
	s.persist = func(document) error { return errors.New("simulated disk failure") }
	if !s.Authenticate("old-secret-keep-working") {
		t.Fatal("optional secret capture blocked authentication")
	}
	if status, result := copyKeyResponse(t, s, "legacy"); status != 409 || result["key"] != nil {
		t.Fatal("failed persistence exposed uncommitted capture")
	}
}

func TestCiphertextCannotBeSwappedBetweenKeys(t *testing.T) {
	path := filepath.Join(t.TempDir(), "keys.json")
	s, err := Open(path, "")
	if err != nil {
		t.Fatal(err)
	}
	a, keyA, err := s.Create("a", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	b, _, err := s.Create("b", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(path)
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	records := doc["keys"].([]any)
	one, two := records[0].(map[string]any), records[1].(map[string]any)
	one["encrypted_key"], two["encrypted_key"] = two["encrypted_key"], one["encrypted_key"]
	raw, _ = json.Marshal(doc)
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{a.ID, b.ID} {
		if status, result := copyKeyResponse(t, s, id); status != 409 || result["key"] != nil {
			t.Fatal("swapped ciphertext disclosed another key")
		}
	}
	if !s.Authenticate(keyA) {
		t.Fatal("digest authentication was affected by corrupted copy data")
	}
	if status, result := copyKeyResponse(t, s, a.ID); status != 200 || result["key"] != keyA {
		t.Fatal("known valid secret could not repair its own encrypted copy")
	}
}
