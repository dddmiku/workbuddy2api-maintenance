// ═══ 更新日志 ═══
// 2026-09-30：锁定筛选项接口：下拉必须列全部密钥与模型，且不被 /requests/{id} 抢路由。
package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"workbuddy2api/internal/requestlog"
)

// facetsFixture 建一个带若干条已完成记录的请求明细 store。
func facetsFixture(t *testing.T, records []requestlog.Record) *Handler {
	t.Helper()
	store, err := requestlog.Open(filepath.Join(t.TempDir(), "requests.jsonl"), requestlog.Options{})
	if err != nil {
		t.Fatal(err)
	}
	for _, record := range records {
		if err := store.Append(record); err != nil {
			t.Fatal(err)
		}
	}
	h := NewHandler(Config{Requests: store})
	t.Cleanup(func() { _ = store.Close() })
	return h
}

func facetsRecord(id, keyID, keyName, model string) requestlog.Record {
	now := time.Now().UTC()
	return requestlog.Record{
		RequestID: id, Protocol: requestlog.ProtocolMessages, Model: model,
		KeyID: keyID, KeyName: keyName, StartedAt: now, FinishedAt: now,
		Status: requestlog.StatusSuccess, HTTPStatus: 200, AttemptCount: 1,
	}
}

func facetsGet(t *testing.T, h *Handler, path string) (int, map[string]any) {
	t.Helper()
	w := httptest.NewRecorder()
	// 走 InternalHandler：这些接口只允许本机管理通道，直连 mux 会被 401 挡住。
	h.InternalHandler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("响应不是 JSON: %s", w.Body.String())
	}
	return w.Code, body
}

// TestRequestFacetsListsAllKeysAndModels 下拉必须列出**全部**出现过的密钥与模型，
// 而不是只列当前这一页的。2026-09-30 用户反馈「箭头点开什么都没有」——
// 因为旧实现只从当前页 20 条里凑，首次加载前更是空的。
func TestRequestFacetsListsAllKeysAndModels(t *testing.T) {
	h := facetsFixture(t, []requestlog.Record{
		facetsRecord("req_1", "key_a", "热情", "global:deepseek-v4.1-flash"),
		facetsRecord("req_2", "key_b", "风少爷", "cn:hy3"),
		facetsRecord("req_3", "key_a", "热情", "global:deepseek-v4.1-flash"),
	})
	code, body := facetsGet(t, h, "/requests/facets")
	if code != http.StatusOK || body["ok"] != true {
		t.Fatalf("code=%d body=%v", code, body)
	}
	keys, _ := body["keys"].([]any)
	models, _ := body["models"].([]any)
	if len(keys) != 2 {
		t.Fatalf("应列出 2 个去重密钥，实际 %d: %v", len(keys), keys)
	}
	if len(models) != 2 {
		t.Fatalf("应列出 2 个去重模型，实际 %d: %v", len(models), models)
	}
	first, _ := keys[0].(map[string]any)
	if first["id"] != "key_a" || first["name"] != "热情" {
		t.Fatalf("密钥应带展示名且新在前: %v", keys)
	}
	if body["truncated"] != false {
		t.Fatalf("未截断时不应标记: %v", body)
	}
}

// TestRequestFacetsRouteIsNotShadowed facets 不能被 /requests/{requestID} 抢走。
func TestRequestFacetsRouteIsNotShadowed(t *testing.T) {
	h := facetsFixture(t, []requestlog.Record{facetsRecord("req_1", "key_a", "A", "m")})
	code, body := facetsGet(t, h, "/requests/facets")
	if code != http.StatusOK {
		t.Fatalf("facets 被当成请求 ID 处理了: code=%d body=%v", code, body)
	}
	if _, isList := body["keys"]; !isList {
		t.Fatalf("返回的不是筛选项结构: %v", body)
	}
}

// TestRequestFacetsRejectsQuery 该接口不接受参数，避免被当成筛选通道。
func TestRequestFacetsRejectsQuery(t *testing.T) {
	h := facetsFixture(t, []requestlog.Record{facetsRecord("req_1", "key_a", "A", "m")})
	code, _ := facetsGet(t, h, "/requests/facets?limit=1")
	if code != http.StatusBadRequest {
		t.Fatalf("带参数应被拒: code=%d", code)
	}
}
