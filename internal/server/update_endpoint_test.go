// ═══ 更新日志 ═══
// 2026-09-17：锁定热更新端点的边界：只走本机管理通道、未启用时报未启用、
//
//	/healthz 透出当前版本，供部署脚本核对切换是否生效。
package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/hotupdate"
	"workbuddy2api/internal/version"
)

func updateTestHandler(t *testing.T, manager *hotupdate.Manager) *Handler {
	t.Helper()
	return NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: newFakeUpstream(t, func(string) (int, string, bool) { return http.StatusOK, sseOK, true }),
		APIKey:   "legacy-key",
		Update:   manager,
	})
}

func TestUpdateEndpointsAreInternalOnly(t *testing.T) {
	handler := updateTestHandler(t, hotupdate.NewManager(hotupdate.Options{Enabled: true}))
	for _, item := range []struct{ method, path string }{
		{http.MethodGet, "/update"},
		{http.MethodPost, "/update/check"},
		{http.MethodPost, "/update/apply"},
	} {
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(item.method, item.path, strings.NewReader("{}"))
		request.Header.Set("Authorization", "Bearer legacy-key")
		handler.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusUnauthorized {
			t.Fatalf("%s %s status=%d want 401 (管理通道之外的调用密钥不能碰热更新)", item.method, item.path, recorder.Code)
		}
	}
}

func TestUpdateStatusWithoutManagerReportsDisabled(t *testing.T) {
	handler := updateTestHandler(t, nil)
	recorder := httptest.NewRecorder()
	handler.InternalHandler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/update", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body)
	}
	body := recorder.Body.String()
	for _, want := range []string{`"ok":false`, `"enabled":false`, "update.enabled"} {
		if !strings.Contains(body, want) {
			t.Fatalf("disabled manager payload missing %q: %s", want, body)
		}
	}
}

func TestUpdateStatusCarriesVersionAndHandoverFlags(t *testing.T) {
	handler := updateTestHandler(t, hotupdate.NewManager(hotupdate.Options{Enabled: true, Dir: t.TempDir()}))
	recorder := httptest.NewRecorder()
	handler.InternalHandler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/update", nil))
	body := recorder.Body.String()
	// 不改全局 version.Version：它被后台 goroutine 读，改了会触发数据竞争。
	for _, want := range []string{`"ok":true`, `"current":"` + version.Version + `"`, `"state":"idle"`,
		`"inherited_fd":false`, hotupdate.DefaultRepo} {
		if !strings.Contains(body, want) {
			t.Fatalf("/update payload missing %q: %s", want, body)
		}
	}
}

// TestUpdateApplyWithoutPriorCheckStartsAnUpdate 未先点「检查更新」时，触发动作要自己去查
// 远端版本（否则管理台的「立即更新」会先回一句莫名其妙的"已经是最新版本"）。
func TestUpdateApplyWithoutPriorCheckStartsAnUpdate(t *testing.T) {
	// 后台 goroutine 会去查远端：指向不存在的仓库，让它在测试里立刻失败，
	// 既不真的碰 GitHub，也不会长时间留一个读取全局状态的 goroutine。
	manager := hotupdate.NewManager(hotupdate.Options{Enabled: true, Dir: t.TempDir(), Repo: "invalid/repo"})
	handler := updateTestHandler(t, manager)
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/update/apply", strings.NewReader("{}"))
	handler.InternalHandler().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body)
	}
	if !strings.Contains(recorder.Body.String(), `"ok":true`) {
		t.Fatalf("apply must be accepted and let the manager query the release: %s", recorder.Body)
	}
}

func TestUpdateApplyRejectsBadTagPayload(t *testing.T) {
	handler := updateTestHandler(t, hotupdate.NewManager(hotupdate.Options{Enabled: true, Dir: t.TempDir()}))
	// 载荷一旦带了内容就必须是合法的小 JSON：否则一次手滑的请求会静默升到最新版。
	cases := []string{strings.Repeat("x", 1<<13), "{not json"}
	names := []string{"oversize", "broken-json"}
	for index, payload := range cases {
		withSubtest(t, names[index], func(t *testing.T) {
			recorder := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodPost, "/update/apply", strings.NewReader(payload))
			handler.InternalHandler().ServeHTTP(recorder, request)
			if recorder.Code != http.StatusOK {
				t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body)
			}
			if !strings.Contains(recorder.Body.String(), `"ok":false`) {
				t.Fatalf("非法载荷必须被拒绝: %s", recorder.Body)
			}
		})
	}
}

func withSubtest(t *testing.T, name string, fn func(*testing.T)) {
	t.Helper()
	t.Run(name, fn)
}

func TestHealthzReportsVersion(t *testing.T) {
	handler := updateTestHandler(t, nil)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("healthz status=%d", recorder.Code)
	}
	// 部署脚本靠这两个字段核对切换是否生效，所以必须真的透出编译期注入的值。
	body := recorder.Body.String()
	for _, want := range []string{`"version":"` + version.Version + `"`,
		`"commit":"` + version.Commit + `"`, ServiceName} {
		if !strings.Contains(body, want) {
			t.Fatalf("healthz payload missing %q: %s", want, body)
		}
	}
}

// 关掉热更新后，apply 必须如实拒绝：此前会回「已开始热更新」而实际什么都没发生。
func TestUpdateApplyRefusesWhenDisabled(t *testing.T) {
	handler := updateTestHandler(t, hotupdate.NewManager(hotupdate.Options{Enabled: false}))
	recorder := httptest.NewRecorder()
	handler.InternalHandler().ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/update/apply", strings.NewReader("{}")))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body)
	}
	body := recorder.Body.String()
	if !strings.Contains(body, `"ok":false`) || !strings.Contains(body, "未启用") {
		t.Fatalf("disabled apply was not refused honestly: %s", body)
	}
	if strings.Contains(body, "已开始热更新") {
		t.Fatalf("disabled apply claimed to have started: %s", body)
	}
}

// 人工复活被停用账号：仅管理通道，清除 disabled 后可立即被选中。
func TestReviveAccountEndpoint(t *testing.T) {
	p := testPoolWith(&auth.Auth{UID: "revive-me", AccessToken: "at", ExpiresAt: 9999999999})
	p.Disable("revive-me", "test disable")
	handler := NewHandler(Config{Pool: p, Upstream: newFakeUpstream(t, func(string) (int, string, bool) { return http.StatusOK, sseOK, true }), APIKey: "legacy-key"})

	// 调用密钥（非本机管理通道）不能复活账号。
	recorder := httptest.NewRecorder()
	forged := httptest.NewRequest(http.MethodPost, "/accounts/revive", strings.NewReader(`{"uid":"revive-me"}`))
	forged.Header.Set("Authorization", "Bearer legacy-key")
	handler.ServeHTTP(recorder, forged)
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("外部调用复活接口应 401，实际 %d", recorder.Code)
	}

	recorder = httptest.NewRecorder()
	handler.InternalHandler().ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/accounts/revive", strings.NewReader(`{"uid":"revive-me"}`)))
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), `"ok":true`) {
		t.Fatalf("复活失败: %d %s", recorder.Code, recorder.Body)
	}
	if state, _ := p.Status("revive-me"); state.Disabled {
		t.Fatalf("账号仍处于禁用: %+v", state)
	}

	recorder = httptest.NewRecorder()
	handler.InternalHandler().ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/accounts/revive", strings.NewReader(`{"uid":"missing"}`)))
	if !strings.Contains(recorder.Body.String(), "账号不存在") {
		t.Fatalf("未知 uid 应如实报错: %s", recorder.Body)
	}
}
