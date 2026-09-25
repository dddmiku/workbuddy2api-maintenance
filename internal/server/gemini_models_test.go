// ═══ 更新日志 ═══
// 2026-09-25：验证 Gemini 发现共用模型权限，分页可继续且未知上下文窗口不编造。
package server

import (
	"encoding/json"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestGeminiModelDiscoveryUsesPermissionsAndRealLimits(t *testing.T) {
	seedDiscoveryModels(t)
	h, key, calls := boundKeyHandler(t, []string{"cn:known/model"})
	h.cfg.GlobalEnabled = false
	r := httptest.NewRequest("GET", "/v1beta/models?pageSize=200", nil)
	r.Header.Set("Authorization", "Bearer "+key)
	w := httptest.NewRecorder()
	gw := newGeminiWriter(w)
	h.withAuth(h.geminiModels)(gw, r)
	gw.finish()
	if w.Code != 200 || *calls != 0 || strings.Contains(w.Body.String(), "unknown-limits") {
		t.Fatalf("discovery did not use calling-key permissions: %d %s", w.Code, w.Body.String())
	}
	for _, expected := range []string{"models/cn:known/model", "\"inputTokenLimit\":65536", "\"outputTokenLimit\":4096", "generateContent"} {
		if !strings.Contains(w.Body.String(), expected) {
			t.Fatalf("discovery missing %s: %s", expected, w.Body.String())
		}
	}
}

func TestGeminiModelDiscoveryPaginationKeepsUnknownLimitsUnknown(t *testing.T) {
	seedDiscoveryModels(t)
	h, key, _ := boundKeyHandler(t, nil)
	h.cfg.GlobalEnabled = false
	token := ""
	seen := map[string]bool{}
	for page := 0; page < 3; page++ {
		r := httptest.NewRequest("GET", "/v1beta/models?pageSize=1&pageToken="+url.QueryEscape(token), nil)
		r.Header.Set("Authorization", "Bearer "+key)
		w := httptest.NewRecorder()
		gw := newGeminiWriter(w)
		h.withAuth(h.geminiModels)(gw, r)
		gw.finish()
		var body map[string]any
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &body) != nil {
			t.Fatalf("invalid model page: %d %s", w.Code, w.Body.String())
		}
		for _, raw := range body["models"].([]any) {
			model := raw.(map[string]any)
			name := model["name"].(string)
			if seen[name] {
				t.Fatalf("repeated model %s", name)
			}
			seen[name] = true
			if strings.Contains(name, "unknown-limits") && (model["inputTokenLimit"] != nil || model["outputTokenLimit"] != nil) {
				t.Fatalf("invented model limits: %v", model)
			}
		}
		token, _ = body["nextPageToken"].(string)
		if token == "" {
			break
		}
	}
	if len(seen) != 2 {
		t.Fatalf("pagination lost models: %v", seen)
	}
}
