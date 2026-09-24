// ═══ 更新日志 ═══
// 2026-09-24：多密钥部署的账号详情和排程只允许管理通道访问，普通调用密钥不能执行全账号操作。
package server

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"workbuddy2api/internal/apikeys"
	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/scheduler"
)

type boundaryTasks struct{ calls int }

func (f *boundaryTasks) TaskSnapshot() []scheduler.TaskInfo {
	f.calls++
	return []scheduler.TaskInfo{{Key: "checkin", Name: "fixture"}}
}

func (f *boundaryTasks) TriggerTask(string) error { f.calls++; return nil }
func (f *boundaryTasks) TaskLog(string) ([]string, error) {
	f.calls++
	return []string{"private-management-log"}, nil
}

func TestManagedKeysCannotAccessAccountAdministration(t *testing.T) {
	store, err := apikeys.Open(filepath.Join(t.TempDir(), "keys.json"), "fixture-legacy")
	if err != nil {
		t.Fatal(err)
	}
	_, key, err := store.Create("client", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	tasks := &boundaryTasks{}
	h := NewHandler(Config{APIKeys: store, APIKey: "fixture-legacy", Tasks: tasks,
		Pool: testPoolWith(&auth.Auth{UID: "private-account", AccessToken: "fixture", ExpiresAt: 9999999999})})
	for _, route := range []struct {
		method, path string
		internalCode int
	}{
		{"GET", "/status", 200}, {"GET", "/tasks", 200},
		{"POST", "/tasks/checkin/run", 202}, {"GET", "/tasks/checkin/log", 200},
	} {
		t.Run(route.method+route.path, func(t *testing.T) {
			for _, token := range []string{"", key, "fixture-legacy"} {
				req := httptest.NewRequest(route.method, route.path, nil)
				req.Header.Set("Authorization", "Bearer "+token)
				req.Header.Set("X-Admin-Request", "1")
				req.Header.Set("X-Forwarded-For", "127.0.0.1")
				before := tasks.calls
				rec := httptest.NewRecorder()
				h.ServeHTTP(rec, req)
				if rec.Code != http.StatusUnauthorized || tasks.calls != before ||
					strings.Contains(rec.Body.String(), "private-account") || strings.Contains(rec.Body.String(), "private-management-log") {
					t.Errorf("public request reached administration: status=%d task_calls=%d", rec.Code, tasks.calls-before)
				}
			}
			rec := httptest.NewRecorder()
			h.InternalHandler().ServeHTTP(rec, httptest.NewRequest(route.method, route.path, nil))
			if rec.Code != route.internalCode {
				t.Fatalf("management channel unavailable: status=%d", rec.Code)
			}
		})
	}
}
