// ═══ 更新日志 ═══
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
