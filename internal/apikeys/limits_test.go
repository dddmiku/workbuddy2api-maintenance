// ═══ 更新日志 ═══
// 2026-09-25：从真实密钥管理入口验证限流持久化、深拷贝、缺省保留和显式清空，拒绝会被静默转为不限的非法数值。
package apikeys

import (
	"encoding/json"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"workbuddy2api/internal/keylimit"
)

func TestLimitsAdminLifecycle(t *testing.T) {
	path := filepath.Join(t.TempDir(), "keys.json")
	store, err := Open(path, "")
	if err != nil {
		t.Fatal(err)
	}
	call := func(method, path, body string, status int) *httptest.ResponseRecorder {
		t.Helper()
		response := httptest.NewRecorder()
		store.AdminHandler().ServeHTTP(response, httptest.NewRequest(method, path, strings.NewReader(body)))
		if response.Code != status {
			t.Fatalf("%s %s: status=%d body=%s", method, path, response.Code, response.Body.String())
		}
		return response
	}
	result := call("POST", "/keys", `{"name":"fixture","limits":{"requests_per_minute":30,"max_concurrent":2,"queue_timeout_seconds":5}}`, 201)
	var created struct {
		Key   string `json:"key"`
		Entry Info   `json:"entry"`
	}
	if err := json.Unmarshal(result.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if created.Entry.Limits == nil || created.Entry.Limits.RequestsPerMinute != 30 {
		t.Fatal("create lost limits")
	}
	call("PATCH", "/keys/"+created.Entry.ID, `{"name":"renamed"}`, 200)
	value, status := store.Lookup(created.Key)
	if status != StatusActive || value.Limits == nil || value.Limits.QueueTimeoutSeconds != 5 {
		t.Fatal("unrelated edit cleared limits")
	}
	reopened, err := Open(path, "")
	if err != nil {
		t.Fatal(err)
	}
	if value, _ := reopened.Lookup(created.Key); value.Limits == nil || value.Limits.MaxConcurrent != 2 {
		t.Fatal("limits were not persisted")
	}
	call("PATCH", "/keys/"+created.Entry.ID, `{"limits":null}`, 200)
	if value, _ := store.Lookup(created.Key); value.Limits != nil {
		t.Fatal("explicit null did not clear limits")
	}
	call("PATCH", "/keys/"+created.Entry.ID, `{"limits":{"requests_per_minute":1}}`, 200)
	call("PATCH", "/keys/"+created.Entry.ID, `{"limits":{"requests_per_minute":0,"max_concurrent":0,"queue_timeout_seconds":0}}`, 200)
	if value, _ := store.Lookup(created.Key); value.Limits != nil {
		t.Fatal("all-zero policy did not clear limits")
	}
}

func TestLimitsDefensiveCopies(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "keys.json"), "")
	if err != nil {
		t.Fatal(err)
	}
	policy := &keylimit.Policy{RequestsPerMinute: 60, MaxConcurrent: 3}
	created, secret, err := store.Create("fixture", "", nil, Options{Limits: policy, LimitsSet: true})
	if err != nil {
		t.Fatal(err)
	}
	policy.MaxConcurrent = 100
	created.Limits.MaxConcurrent = 100
	value, _ := store.Lookup(secret)
	if value.Limits.MaxConcurrent != 3 {
		t.Fatal("caller mutated stored policy")
	}
	value.Limits.MaxConcurrent = 101
	listed := store.List()
	listed[0].Limits.MaxConcurrent = 102
	value, _ = store.Lookup(secret)
	if value.Limits.MaxConcurrent != 3 {
		t.Fatal("read API leaked policy pointer")
	}
}

func TestLimitsRejectInvalidValues(t *testing.T) {
	for _, limits := range []string{
		`true`, `[]`, `"unlimited"`, `1`, `{"max_concurrent":-1}`, `{"max_concurrent":257}`,
		`{"requests_per_minute":60001}`, `{"queue_timeout_seconds":31}`, `{"queue_timeout_seconds":1}`,
		`{"max_concurrent":null}`, `{"requests_per_minute":true}`, `{"max_concurrent":1.5}`,
		`{"max_concurrent":2,"unexpected":1}`, `{"max_concurrent":1,"max_concurrent":0}`,
	} {
		t.Run(limits, func(t *testing.T) {
			store, err := Open(filepath.Join(t.TempDir(), "keys.json"), "")
			if err != nil {
				t.Fatal(err)
			}
			response := httptest.NewRecorder()
			store.AdminHandler().ServeHTTP(response, httptest.NewRequest("POST", "/keys", strings.NewReader(`{"name":"fixture","limits":`+limits+`}`)))
			if response.Code != 400 {
				t.Fatalf("invalid policy silently accepted: status=%d", response.Code)
			}
			if len(store.List()) != 0 {
				t.Fatal("invalid policy created a key")
			}
		})
	}
}
