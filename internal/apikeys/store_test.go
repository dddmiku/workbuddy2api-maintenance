// ═══ 更新日志 ═══
// 2026-09-22：模型绑定写入侧要求完整模型名（带 cn:/global: 前缀），裸名返回
//
//	ErrBindingRealm；读取旧文件仍放行，避免存量密钥库整体打不开。
//
// 2026-09-18：只在支持 POSIX 权限位的平台断言 0600，Windows 继续执行完整密钥生命周期回归。
package apikeys

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
)

// TestModelBindingValidationAndPersistence 覆盖模型白名单的校验、更新、清空与重载。
func TestModelBindingValidationAndPersistence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "api_keys.json")
	s, err := Open(path, "")
	if err != nil {
		t.Fatal(err)
	}
	// 先断言裸名被拒：这一把不产生可用记录，返回的 info/key 直接丢弃。
	if _, _, err := s.Create("bound", "", []string{"cn:deepseek-v4.1-flash", "glm-5.2"}); !errors.Is(err, ErrBindingRealm) {
		t.Fatalf("bare binding accepted on create: err=%v", err)
	}
	info, key, err := s.Create("bound", "", []string{"cn:deepseek-v4.1-flash", "global:glm-5.2"})
	if err != nil {
		t.Fatal(err)
	}
	resolved, ok := s.Resolve(key)
	if !ok || len(resolved.Models) != 2 || resolved.Models[0] != "cn:deepseek-v4.1-flash" {
		t.Fatalf("models not stored: %+v", resolved.Models)
	}
	for _, bad := range [][]string{{""}, {"a b"}, {strings.Repeat("x", 65)}, {"dup", "dup"}, {"bad\nname"}} {
		if _, _, err := s.Create("x", "", bad); !errors.Is(err, ErrInvalidModels) {
			t.Errorf("invalid models accepted: %q (err=%v)", bad, err)
		}
	}
	too := make([]string, MaxBoundModels+1)
	for i := range too {
		too[i] = "cn:m" + string(rune('a'+i%26)) + string(rune('a'+i/26))
	}
	if _, _, err := s.Create("x", "", too); !errors.Is(err, ErrInvalidModels) {
		t.Error("oversize model list accepted")
	}
	// 裸名同样不能在更新时写进去。
	if _, err := s.Update(info.ID, nil, nil, nil, &[]string{"glm-5.2"}); !errors.Is(err, ErrBindingRealm) {
		t.Errorf("bare binding accepted on update: err=%v", err)
	}
	updated := []string{"global:glm-5.2"}
	if _, err := s.Update(info.ID, nil, nil, nil, &updated); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(path, "")
	if err != nil {
		t.Fatal(err)
	}
	entry, ok := reopened.Resolve(key)
	if !ok || len(entry.Models) != 1 || entry.Models[0] != "global:glm-5.2" {
		t.Fatalf("binding not persisted: %+v", entry.Models)
	}
	empty := []string{}
	if _, err := reopened.Update(info.ID, nil, nil, nil, &empty); err != nil {
		t.Fatal(err)
	}
	cleared, _ := reopened.Resolve(key)
	if len(cleared.Models) != 0 {
		t.Fatalf("binding not cleared: %+v", cleared.Models)
	}
}

// TestLegacyBareBindingStillLoads 锁定读取侧的宽松：历史文件里存过裸名绑定
// 时整个密钥库必须照常打开，否则写入侧一收紧，存量密钥会全部鉴权失败——
// 那是把「一条绑定要改」放大成「所有人断网」。裸名在鉴权时只匹配裸名请求，
// 不会顺带放行 cn: 请求。
func TestLegacyBareBindingStillLoads(t *testing.T) {
	path := filepath.Join(t.TempDir(), "api_keys.json")
	s, err := Open(path, "")
	if err != nil {
		t.Fatal(err)
	}
	_, key, err := s.Create("legacy", "", []string{"cn:deepseek-v4.1-flash"})
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
	for _, item := range doc["keys"].([]any) {
		entry := item.(map[string]any)
		if entry["name"] == "legacy" {
			entry["models"] = []string{"deepseek-v4.1-flash"}
		}
	}
	encoded, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(path, "")
	if err != nil {
		t.Fatalf("legacy bare binding made the store unreadable: %v", err)
	}
	entry, ok := reopened.Resolve(key)
	if !ok {
		t.Fatal("legacy key stopped authenticating")
	}
	if len(entry.Models) != 1 || entry.Models[0] != "deepseek-v4.1-flash" {
		t.Fatalf("legacy binding changed on load: %+v", entry.Models)
	}
	// 无关字段仍可修改：否则管理员没法把这条旧绑定改对。
	note := "fixed"
	if _, err := reopened.Update(entry.ID, nil, &note, nil, nil); err != nil {
		t.Fatalf("unrelated update rejected on a legacy bare binding: %v", err)
	}
}

func TestKeyLifecycleAndRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "api_keys.json")
	legacy := "old-existing-secret-keep-working"
	s, err := Open(path, legacy)
	if err != nil {
		t.Fatal(err)
	}
	if !s.Authenticate(legacy) || s.Authenticate("") {
		t.Fatal("legacy migration failed")
	}
	info, key, err := s.Create("测试客户端", "工作电脑", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(key, "wbk_") || len(key) != 47 || !s.Authenticate(key) {
		t.Fatal("new key not accepted")
	}
	raw, _ := os.ReadFile(path)
	if strings.Contains(string(raw), key) || strings.Contains(string(raw), legacy) {
		t.Fatal("plaintext key persisted")
	}
	stat, _ := os.Stat(path)
	if runtime.GOOS != "windows" && stat.Mode().Perm() != 0600 {
		t.Fatalf("permissions=%o", stat.Mode().Perm())
	}
	enabled := false
	if _, err = s.Update(info.ID, nil, nil, &enabled, nil); err != nil {
		t.Fatal(err)
	}
	if s.Authenticate(key) {
		t.Fatal("disabled key accepted")
	}
	s, err = Open(path, legacy)
	if err != nil {
		t.Fatal(err)
	}
	if s.Authenticate(key) || !s.Authenticate(legacy) {
		t.Fatal("restart lost enable state")
	}
	enabled = true
	name := "新的名称"
	if _, err = s.Update(info.ID, &name, nil, &enabled, nil); err != nil {
		t.Fatal(err)
	}
	if !s.Authenticate(key) {
		t.Fatal("reenabled key rejected")
	}
	if err = s.Delete(info.ID); err != nil {
		t.Fatal(err)
	}
	if err = s.Delete("legacy"); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path, legacy)
	if err != nil {
		t.Fatal(err)
	}
	if s.Authenticate(key) || s.Authenticate(legacy) || len(s.List()) != 0 {
		t.Fatal("deleted keys reactivated on restart")
	}
}

func TestPersistenceFailureDoesNotPublishMutation(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "keys.json"), "existing-secret-long-enough")
	if err != nil {
		t.Fatal(err)
	}
	s.persist = func(document) error { return errors.New("simulated disk failure") }
	if _, key, err := s.Create("new", "", nil); err == nil || key != "" {
		t.Fatal("failed write returned a usable key")
	}
	enabled := false
	if _, err = s.Update("legacy", nil, nil, &enabled, nil); err == nil {
		t.Fatal("write failure ignored")
	}
	if err = s.Delete("legacy"); err == nil {
		t.Fatal("delete failure ignored")
	}
	if len(s.List()) != 1 || !s.Authenticate("existing-secret-long-enough") {
		t.Fatal("failed write changed active state")
	}
}

func TestCorruptStoreFailsClosed(t *testing.T) {
	for _, data := range []string{`null`, `{}`, `{"version":2,"keys":[]}`, `{"version":1,"keys":[{"id":"legacy","name":"x","sha256":"bad"}]}`} {
		path := filepath.Join(t.TempDir(), "keys.json")
		if err := os.WriteFile(path, []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := Open(path, "legacy-secret"); err == nil {
			t.Fatalf("corrupt store accepted: %s", data)
		}
	}
}

func TestKeyLabelValidation(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "keys.json"), "")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"", strings.Repeat("界", 65), "header\nvalue"} {
		if _, _, err := s.Create(name, "", nil); !errors.Is(err, ErrInvalid) {
			t.Fatalf("name accepted: %q", name)
		}
	}
	if _, _, err := s.Create("合法", strings.Repeat("x", 257), nil); !errors.Is(err, ErrInvalid) {
		t.Fatal("oversize note accepted")
	}
}

func TestConcurrentAuthenticationAndManagement(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "keys.json"), "")
	if err != nil {
		t.Fatal(err)
	}
	info, key, err := s.Create("concurrent", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				s.Authenticate(key)
				s.List()
			}
		}()
	}
	for i := 0; i < 12; i++ {
		enabled := i%2 == 0
		if _, err := s.Update(info.ID, nil, nil, &enabled, nil); err != nil {
			t.Fatal(err)
		}
	}
	wg.Wait()
}
