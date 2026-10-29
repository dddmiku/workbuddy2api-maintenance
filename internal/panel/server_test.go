// ═══ 更新日志 ═══
// 2026-10-13：初版——锁住 Go 面板后端与 panel/app.py 的**行为等价**。
//
// 为什么这组测试必须存在：Go 版要接手已在跑的部署，前端（index.html / *.js）完全复用，
// 它按状态码、按中文文案、按 JSON 键名分支。任何一处差异都会表现成「按钮没反应」
// 或「提示文不对」这类很难定位的现象。参照行为取自 panel/test_*.py 的断言。
//
// 测试全部在**进程内**跑 httptest，不打网络、不起 docker、不读真实配置。
package panel

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"
)

// ── 测试替身 ──────────────────────────────────────────────────────────

// stubRunner 记录被执行的 argv 并返回预设输出。
type stubRunner struct {
	mu        sync.Mutex
	calls     [][]string
	replies   map[string]ExecResult
	credit    []byte
	creditErr error
}

func (s *stubRunner) Run(_ context.Context, args []string, _ time.Duration, _ map[string]string) ExecResult {
	s.mu.Lock()
	s.calls = append(s.calls, append([]string(nil), args...))
	s.mu.Unlock()
	key := strings.Join(args, " ")
	if result, ok := s.replies[key]; ok {
		return result
	}
	return ExecResult{Code: 0}
}

func (s *stubRunner) RunCredit(_ context.Context, _ map[string]string) ([]byte, error) {
	if s.creditErr != nil {
		return nil, s.creditErr
	}
	return s.credit, nil
}

func (s *stubRunner) recorded() [][]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([][]string(nil), s.calls...)
}

// stubTransport 是管理 socket 的假实现：按 endpoint 返回预设结果并记录调用。
type stubTransport struct {
	mu       sync.Mutex
	requests []stubRequest
	replies  map[string]stubReply
}

type stubRequest struct {
	Method   string
	Endpoint string
	Body     string
}

type stubReply struct {
	Code int
	Body string
}

func (s *stubTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	var raw []byte
	if req.Body != nil {
		raw, _ = readAllLimited(req.Body)
	}
	s.mu.Lock()
	s.requests = append(s.requests, stubRequest{Method: req.Method, Endpoint: req.URL.Path + queryOf(req.URL.RawQuery), Body: string(raw)})
	reply, ok := s.replies[req.Method+" "+req.URL.Path]
	s.mu.Unlock()
	if !ok {
		reply = stubReply{Code: 200, Body: `{"ok":true}`}
	}
	return &http.Response{
		StatusCode: reply.Code,
		Body:       newReadCloser(reply.Body),
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Request:    req,
	}, nil
}

func (s *stubTransport) recorded() []stubRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]stubRequest(nil), s.requests...)
}

// count 统计某个 "METHOD /path" 被调过几次（断言「必须触发重载」这类要求用）。
func (s *stubTransport) count(key string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, req := range s.requests {
		if req.Method+" "+req.Endpoint == key {
			n++
		}
	}
	return n
}

// ── 测试装置 ──────────────────────────────────────────────────────────

// harness 是一套隔离的面板后端：临时目录 + 假时钟 + 假执行器 + 假管理通道。
type harness struct {
	t         *testing.T
	server    *Server
	dir       string
	runner    *stubRunner
	transport *stubTransport
	clock     *harnessClock
}

type harnessClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *harnessClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *harnessClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	dir := t.TempDir()
	adminDir := filepath.Join(dir, "panel-data")
	authDir := filepath.Join(dir, "auths")
	trashDir := filepath.Join(dir, "auths-trash")
	for _, d := range []string{adminDir, authDir, trashDir} {
		if err := os.MkdirAll(d, 0700); err != nil {
			t.Fatal(err)
		}
	}
	configPath := filepath.Join(dir, "config.json")
	if err := os.WriteFile(configPath, []byte("{\n  \"api_keys_file\": \"./data/api_keys.json\",\n  \"schedule\": {\n    \"checkin_enabled\": true\n  }\n}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	runner := &stubRunner{replies: map[string]ExecResult{}}
	transport := &stubTransport{replies: map[string]stubReply{}}
	clock := &harnessClock{now: time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)}

	assets := fstest.MapFS{
		"index.html":       &fstest.MapFile{Data: []byte("<html>panel</html>")},
		"login.html":       &fstest.MapFile{Data: []byte("<html>login</html><span id=\"hint\">会话有效期 30 天</span>")},
		"vendor/qrcode.js": &fstest.MapFile{Data: []byte("// qrcode")},
	}
	server, err := New(Options{
		AdminDir:   adminDir,
		AuthDir:    authDir,
		TrashDir:   trashDir,
		ConfigPath: configPath,
		BaseDir:    dir,
		RuntimeDir: filepath.Join(dir, "runtime"),
		Container:  "workbuddy2api",
		Runtime:    "native",
		Assets:     fs.FS(assets),
		Now:        clock.Now,
		Version:    "2.5.0",
	})
	if err != nil {
		t.Fatal(err)
	}
	server.runner = runner
	server.credits.runner = runner
	server.gateway.Management.Transport = transport
	server.gateway.Management.SocketPath = filepath.Join(dir, "api_keys.sock")
	server.gateway.HTTPClient = &http.Client{Transport: transport}
	return &harness{t: t, server: server, dir: dir, runner: runner, transport: transport, clock: clock}
}

// writeCredentials 写一份可用的 credentials.json。
func (h *harness) writeCredentials() Credentials {
	h.t.Helper()
	hash, err := HashPassword("secret-password")
	if err != nil {
		h.t.Fatal(err)
	}
	doc := Credentials{
		"version": 1, "username": "wbadmin", "password": hash,
		"sessionKey": strings.Repeat("ab", 32), "updatedAt": h.clock.Now().Unix(),
	}
	store := &CredentialStore{Path: filepath.Join(h.dir, "panel-data", "credentials.json")}
	if err := store.Save(doc); err != nil {
		h.t.Fatal(err)
	}
	return doc
}

// token 签一个有效会话令牌。
//
// 必须用**假时钟**（IssueSessionAt + h.clock.Now()），不能用 IssueSession：
// 后者读 time.Now()，令牌的 Expires 会以真实时间为基准，而服务器校验走的是
// h.clock。两个基准错位时，「推进假时钟」算出来的剩余量与令牌真实剩余量无关，
// 续期判据永远对不上——表现为「临近过期时必须滑动续期」这类断言必然失败，
// 而产品代码其实是对的（2026-10-13 支持行空板时跑全量测试发现，该失败在
// HEAD 上已存在）。
func (h *harness) token(doc Credentials) string {
	h.t.Helper()
	token, err := IssueSessionAt(doc, "", SessionTTL, h.clock.Now())
	if err != nil {
		h.t.Fatal(err)
	}
	return token
}

// do 发一个请求并返回响应。
//
// Content-Length 走 header 显式设置：面板的框架校验读的就是 header 里的原始文本，
// 而不是 net/http 归一化后的 r.ContentLength——测试必须能造出「header 里是畸形值」
// 这种真实场景。
func (h *harness) do(method, path, body string, headers map[string]string) *httptest.ResponseRecorder {
	h.t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Host = "127.0.0.1:7864"
	req.RemoteAddr = "127.0.0.1:12345"
	req.ContentLength = int64(len(body))
	if method != http.MethodGet && method != http.MethodHead {
		req.Header.Set("Content-Type", "application/json")
	}
	for key, value := range headers {
		if value == "" {
			req.Header.Del(key)
			continue
		}
		req.Header.Set(key, value)
	}
	// 真实客户端总会带 Content-Length；Go 的服务端会把它保留在 Header 里
	// （面板的框架校验读的就是 Header 里的原始文本）。测试必须复现这一点，
	// 否则每个请求都会在「请求长度无效」处被拒——那不是被测代码的问题。
	raw := headers["Content-Length"]
	if _, present := headers["Content-Length"]; !present {
		raw = strconv.Itoa(len(body))
	}
	req.Header.Set("Content-Length", raw)
	if parsed, err := strconv.ParseInt(raw, 10, 64); err == nil {
		req.ContentLength = parsed
	}
	recorder := httptest.NewRecorder()
	h.server.ServeHTTP(recorder, req)
	return recorder
}

// postJSON 发一个带会话与闸门的写请求。
func (h *harness) postJSON(doc Credentials, path string, body map[string]any) *httptest.ResponseRecorder {
	h.t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		h.t.Fatal(err)
	}
	return h.do(http.MethodPost, path, string(raw), map[string]string{
		"Cookie":          CookieName + "=" + h.token(doc),
		"X-Admin-Request": "1",
	})
}

// decode 解析响应体。
func decode(t *testing.T, recorder *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &out); err != nil {
		t.Fatalf("响应不是 JSON 对象: %v\n%s", err, recorder.Body.String())
	}
	return out
}

func messageOf(t *testing.T, recorder *httptest.ResponseRecorder) string {
	t.Helper()
	return jsonString(decode(t, recorder)["message"])
}

// ── C1/C2/C3/C4：闸门与来源校验 ───────────────────────────────────────

func TestGatesRequireSessionOriginAndAdminHeader(t *testing.T) {
	// 每个写接口各给一个**能通过字段校验**的请求体。顺序很重要：面板与 Python 版
	// 一样是「先验请求体、再验会话」，所以拿 `{}` 去打 /api/account/delete 会在
	// 「账号标识不正确」处就返回 400，根本走不到会话闸门——那不是闸门失效。
	validBodies := map[string]string{
		"/api/service/restart":         `{}`,
		"/api/account/delete":          `{"uid":"abc12345"}`,
		"/api/account/toggle":          `{"uid":"abc12345","disabled":true}`,
		"/api/task/toggle":             `{"key":"checkin","enabled":true}`,
		"/api/task/run":                `{"key":"checkin"}`,
		"/api/features/reasoning-loop": `{"stop_only":true}`,
		"/api/login/start":             `{"realm":"cn"}`,
		"/api/login/poll":              `{"realm":"cn","login_id":"0123456789abcdef0123456789abcdef"}`,
		"/api/auth/password":           `{"current":"x","username":"wbadmin","password":"yyyyyyyy","confirm":"yyyyyyyy"}`,
		"/api/update/check":            `{}`,
		"/api/update/apply":            `{}`,
	}
	h := newHarness(t)
	doc := h.writeCredentials()
	for route, body := range validBodies {
		t.Run("无会话"+route, func(t *testing.T) {
			recorder := h.do(http.MethodPost, route, body, map[string]string{"X-Admin-Request": "1"})
			if recorder.Code != 401 {
				t.Fatalf("应 401，实际 %d：%s", recorder.Code, recorder.Body.String())
			}
			if !strings.Contains(recorder.Body.String(), "登录已失效") {
				t.Fatalf("401 必须是可解析的 JSON 并带原因：%s", recorder.Body.String())
			}
		})
		t.Run("异源"+route, func(t *testing.T) {
			recorder := h.do(http.MethodPost, route, body, map[string]string{
				"Cookie": CookieName + "=" + h.token(doc), "X-Admin-Request": "1",
				"Origin": "https://untrusted.invalid"})
			if recorder.Code != 403 {
				t.Fatalf("应 403，实际 %d", recorder.Code)
			}
			if got := messageOf(t, recorder); got != "请求来源无效，请从管理页面重新操作" {
				t.Fatalf("403 文案不对：%q", got)
			}
		})
		t.Run("缺管理标记"+route, func(t *testing.T) {
			recorder := h.do(http.MethodPost, route, body, map[string]string{
				"Cookie": CookieName + "=" + h.token(doc), "X-Admin-Request": ""})
			if recorder.Code != 403 {
				t.Fatalf("应 403，实际 %d", recorder.Code)
			}
		})
	}
}

// TestValidationRunsBeforeSessionGate 锁住闸门顺序（与 Python 版一致）。
//
// 顺序本身是可观测行为：body 不合法时面板回 400 而不是 401。反过来（先验会话）
// 会让「登录态下填错表单」与「未登录」给出同一种错误，前端无法区分该提示用户
// 重新登录还是改表单。
func TestValidationRunsBeforeSessionGate(t *testing.T) {
	h := newHarness(t)
	recorder := h.do(http.MethodPost, "/api/account/delete", `{"uid":"bad uid"}`,
		map[string]string{"X-Admin-Request": "1"})
	if recorder.Code != 400 {
		t.Fatalf("body 不合法应 400（先验 body 再验会话），实际 %d", recorder.Code)
	}
	if got := messageOf(t, recorder); got != "账号标识不正确" {
		t.Fatalf("文案不对：%q", got)
	}
}

func TestOriginComparisonIsExactHostMatch(t *testing.T) {
	h := newHarness(t)
	doc := h.writeCredentials()
	cookie := CookieName + "=" + h.token(doc)
	cases := []struct {
		origin string
		want   int
	}{
		{"", 200},
		{"http://127.0.0.1:7864", 200},
		{"https://127.0.0.1:7864", 200},
		{"http://127.0.0.1:9999", 403},
		{"https://evil.example", 403},
		{"http://user@127.0.0.1:7864", 403},
		{"not a url", 403},
	}
	for _, tc := range cases {
		t.Run("origin="+tc.origin, func(t *testing.T) {
			recorder := h.do(http.MethodPost, "/api/service/restart", "{}", map[string]string{
				"Cookie": cookie, "X-Admin-Request": "1", "Origin": tc.origin})
			if recorder.Code != tc.want {
				t.Fatalf("Origin %q：应 %d，实际 %d（%s）", tc.origin, tc.want, recorder.Code, recorder.Body.String())
			}
		})
	}
}

// ── C5/C6：请求体框架校验 ─────────────────────────────────────────────

func TestBodyFramingValidation(t *testing.T) {
	h := newHarness(t)
	doc := h.writeCredentials()
	cookie := CookieName + "=" + h.token(doc)
	base := map[string]string{"Cookie": cookie, "X-Admin-Request": "1", "Content-Type": "application/json"}

	cases := []struct {
		name    string
		body    string
		headers map[string]string
		want    int
		message string
	}{
		{"正常", `{}`, nil, 200, ""},
		{"非 JSON 类型", `{}`, map[string]string{"Content-Type": "text/plain"}, 415, "请使用 JSON 格式提交"},
		{"空体", "", map[string]string{"Content-Length": "0"}, 400, "请求体不能为空"},
		{"超限", `{}`, map[string]string{"Content-Length": "9999999"}, 413, "请求体超过大小限制"},
		{"不是对象_数组", `[]`, nil, 400, "请求体必须是 JSON 对象"},
		{"不是对象_null", `null`, nil, 400, "请求体必须是 JSON 对象"},
		{"不是对象_数字", `123`, nil, 400, "请求体必须是 JSON 对象"},
		{"非法 JSON", `not json`, nil, 400, "请求体不是完整的 JSON"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			headers := map[string]string{}
			for k, v := range base {
				headers[k] = v
			}
			for k, v := range tc.headers {
				headers[k] = v
			}
			recorder := h.do(http.MethodPost, "/api/service/restart", tc.body, headers)
			if recorder.Code != tc.want {
				t.Fatalf("应 %d，实际 %d：%s", tc.want, recorder.Code, recorder.Body.String())
			}
			if tc.message != "" {
				if got := messageOf(t, recorder); got != tc.message {
					t.Fatalf("文案不对：%q，期望 %q", got, tc.message)
				}
			}
		})
	}
}

func TestContentLengthHeaderMustBeAsciiDigits(t *testing.T) {
	// 这一条锁住 Python 侧踩过的坑：latin-1 数字字节（0xB2 → '²'）在 Python 的
	// str.isdigit() 下为 True、int() 却抛 ValueError，最终变成 500 并回显内部细节。
	// Go 的 net/http 在更早的一层就把畸形 Content-Length 拒成 400，所以这里锁的是
	// 「最终结果仍是 400 且不是 500」这个可观测行为。
	cases := []string{"invalid", "-1", "+2", "2.0"}
	for _, value := range cases {
		t.Run(value, func(t *testing.T) {
			values := []string{value}
			if !contentLengthOK(values) {
				return // 我们的校验会拒掉它 → 400，正确
			}
			t.Fatalf("Content-Length %q 必须被拒", value)
		})
	}
	for _, ok := range []string{"0", "1", "2", "1234567890"} {
		if !contentLengthOK([]string{ok}) {
			t.Fatalf("Content-Length %q 应通过", ok)
		}
	}
	if contentLengthOK([]string{"1", "2"}) {
		t.Fatal("重复的 Content-Length 必须被拒")
	}
	if contentLengthOK([]string{"12345678901"}) {
		t.Fatal("超过 10 位必须被拒")
	}
}

// ── C9/C10/C11/C12：登录与限流 ────────────────────────────────────────

func TestLoginThrottleResponses(t *testing.T) {
	h := newHarness(t)
	h.writeCredentials()

	// 全站节流：灌满 20 次失败后，登录必须回 429 且只回一个响应。
	for i := 0; i < LoginGlobalMaxFails; i++ {
		h.server.throttle.Failed("203.0.113.9")
	}
	recorder := h.do(http.MethodPost, "/api/auth/login", `{"username":"wbadmin","password":"x"}`,
		map[string]string{"X-Admin-Request": "1"})
	if recorder.Code != 429 {
		t.Fatalf("应 429，实际 %d", recorder.Code)
	}
	retryAfter := recorder.Header().Get("Retry-After")
	if retryAfter == "" {
		t.Fatal("429 必须带 Retry-After")
	}
	if got := messageOf(t, recorder); !strings.Contains(got, "登录尝试过于频繁，请 "+retryAfter+" 秒后再试") {
		t.Fatalf("文案与 Retry-After 不一致：%q（header=%s）", got, retryAfter)
	}
	// 只回一个响应：这里锁的是「header 与 body 都来自同一次 _json 调用」。
	if strings.Count(recorder.Body.String(), "429") != 0 {
		t.Fatalf("响应体里不该再出现 429：%s", recorder.Body.String())
	}
}

func TestLoginBlockedByPerIPBucket(t *testing.T) {
	h := newHarness(t)
	h.writeCredentials()
	for i := 0; i < LoginMaxFails; i++ {
		h.server.throttle.Failed("198.51.100.7")
	}
	recorder := h.do(http.MethodPost, "/api/auth/login", `{"username":"wbadmin","password":"x"}`,
		map[string]string{"X-Admin-Request": "1", "X-Real-IP": "198.51.100.7"})
	if recorder.Code != 429 {
		t.Fatalf("应 429，实际 %d", recorder.Code)
	}
	if got := messageOf(t, recorder); got != "失败次数过多，请 5 分钟后再试" {
		t.Fatalf("文案不对：%q", got)
	}
}

func TestLoginConsumesBudgetBeforeVerifying(t *testing.T) {
	// 并发登录必须「先占预算再验证」：否则两个并发请求会同时通过验证、只记一次失败。
	h := newHarness(t)
	h.writeCredentials()
	before := h.server.throttle.countFor("127.0.0.1")
	recorder := h.do(http.MethodPost, "/api/auth/login", `{"username":"wbadmin","password":"wrong"}`,
		map[string]string{"X-Admin-Request": "1"})
	if recorder.Code != 401 {
		t.Fatalf("应 401，实际 %d", recorder.Code)
	}
	after := h.server.throttle.countFor("127.0.0.1")
	if after != before+1 {
		t.Fatalf("失败的登录必须已记账：%d → %d", before, after)
	}
	// 剩余次数提示：LOGIN_MAX_FAILS - (hits+1)，hits 是本次之前的计数。
	if got := messageOf(t, recorder); got != "用户名或密码错误，还可尝试 5 次" {
		t.Fatalf("剩余次数文案不对：%q", got)
	}
}

func TestLoginSucceedsAndSetsCookie(t *testing.T) {
	h := newHarness(t)
	doc := h.writeCredentials()
	recorder := h.do(http.MethodPost, "/api/auth/login",
		`{"username":"wbadmin","password":"secret-password"}`, map[string]string{"X-Admin-Request": "1"})
	if recorder.Code != 200 {
		t.Fatalf("应 200，实际 %d：%s", recorder.Code, recorder.Body.String())
	}
	body := decode(t, recorder)
	if jsonBool(body["ok"], false) != true {
		t.Fatalf("ok 应为 true：%v", body)
	}
	cookies := recorder.Result().Cookies()
	found := false
	for _, cookie := range cookies {
		if cookie.Name == CookieName && cookie.Value != "" && cookie.Path == "/" {
			found = true
		}
	}
	if !found {
		t.Fatalf("登录必须种下 Path=/ 的会话 cookie：%v", cookies)
	}
	// 种下的 cookie 必须能通过 /api/session。
	// 注意：登录会同时种两条同名 cookie（Path=/ 的有效值 + Path=/admin/ 的清除副本），
	// 取值时必须跳过空值那条，否则会把清除副本当成会话。
	token := ""
	for _, cookie := range cookies {
		if cookie.Name == CookieName && cookie.Value != "" {
			token = cookie.Value
		}
	}
	if token == "" {
		t.Fatalf("没有拿到有效的会话 cookie：%v", cookies)
	}
	session := h.do(http.MethodGet, "/api/session", "", map[string]string{"Cookie": CookieName + "=" + token})
	if session.Code != 200 {
		t.Fatalf("/api/session 应 200，实际 %d", session.Code)
	}
	if got := jsonString(decode(t, session)["username"]); got != doc.Username() {
		t.Fatalf("会话用户名不对：%q", got)
	}
}

func TestLoginRejectsWhenCredentialsChangedUnderneath(t *testing.T) {
	// C12：验证通过后凭证被改过 → 401，且不得签发会话。
	h := newHarness(t)
	doc := h.writeCredentials()
	original := h.server.creds.Load
	// 第一次 Load 返回旧凭证，第二次返回改过的——模拟并发改密。
	calls := 0
	h.server.credsLoadHook = func() (Credentials, error) {
		calls++
		if calls == 1 {
			return original()
		}
		changed := Credentials{}
		for k, v := range doc {
			changed[k] = v
		}
		changed["password"] = "pbkdf2_sha256$200000$AAECAwQFBgcICQoLDA0ODw==$f2A+BZ5F0J+/XtK6mfeaoGhmyQTtH5XVbdPAY7HnBP4="
		return changed, nil
	}
	defer func() { h.server.credsLoadHook = nil }()

	recorder := h.do(http.MethodPost, "/api/auth/login",
		`{"username":"wbadmin","password":"secret-password"}`, map[string]string{"X-Admin-Request": "1"})
	if recorder.Code != 401 {
		t.Fatalf("应 401，实际 %d：%s", recorder.Code, recorder.Body.String())
	}
	if got := messageOf(t, recorder); got != "登录凭证已更改，请使用新密码重新登录" {
		t.Fatalf("文案不对：%q", got)
	}
	for _, cookie := range recorder.Result().Cookies() {
		if cookie.Name == CookieName && cookie.Value != "" {
			t.Fatal("凭证已变时不得种下会话 cookie")
		}
	}
}

func TestLogoutClearsCookiesAndRevokesSession(t *testing.T) {
	h := newHarness(t)
	doc := h.writeCredentials()
	token := h.token(doc)
	recorder := h.do(http.MethodPost, "/api/auth/logout", "{}", map[string]string{
		"Cookie": CookieName + "=" + token, "X-Admin-Request": "1"})
	if recorder.Code != 200 {
		t.Fatalf("应 200，实际 %d", recorder.Code)
	}
	// 只能有清除用的 Set-Cookie，不能有新的会话。
	for _, cookie := range recorder.Result().Cookies() {
		if cookie.Name == CookieName && cookie.Value != "" {
			t.Fatalf("登出不得种下新会话：%v", cookie)
		}
	}
	// 撤销必须落盘：模拟面板重启（重新读文件）后旧令牌仍失效。
	reloaded, err := h.server.creds.Load()
	if err != nil {
		t.Fatal(err)
	}
	if ReadSession(reloaded, token) != nil {
		t.Fatal("登出后旧令牌必须失效（且撤销记录必须已落盘）")
	}
	if _, ok := reloaded["revokedSessions"].(map[string]any); !ok {
		t.Fatalf("撤销记录必须写进 credentials.json：%v", reloaded)
	}
}

func TestRenewedSessionRevokedByLogout(t *testing.T) {
	// C17：续期沿用同一个 nonce，所以按 nonce 撤销天然覆盖续期前后两份令牌。
	h := newHarness(t)
	doc := h.writeCredentials()
	token := h.token(doc)
	payload := ReadSession(doc, token)
	if payload == nil {
		t.Fatal("令牌应有效")
	}
	// 把时钟推到期临界，触发一次滑动续期。
	//
	// 令牌 Expires = 签发时刻 + ttl（由假时钟签发，见 h.token）。续期判据是
	// 剩余 < ttl/3，所以推进量必须**大于** ttl - ttl/3；这里多推 1 小时留出余量，
	// 得到剩余 ttl/3 - 1h。
	//
	// 这条断言长期失败过，原因不在算术而在时钟基准：h.token 原先用 IssueSession
	// （真实 time.Now()）签发，而服务器校验走 h.clock，两个基准错位让推进量算不出
	// 正确剩余。修好基准后这里就正常了（2026-10-13 支持行空板时跑全量测试发现，
	// 该失败在 HEAD 上已存在，与本次改动无关）。
	h.clock.Advance(SessionTTL - renewThreshold + time.Hour)
	recorder := h.do(http.MethodGet, "/api/session", "", map[string]string{"Cookie": CookieName + "=" + token})
	if recorder.Code != 200 {
		t.Fatalf("应 200，实际 %d", recorder.Code)
	}
	renewed := ""
	for _, cookie := range recorder.Result().Cookies() {
		if cookie.Name == CookieName && cookie.Value != "" {
			renewed = cookie.Value
		}
	}
	if renewed == "" {
		t.Fatal("临近过期时必须滑动续期")
	}
	// 续期后的令牌与原来的 nonce 相同。
	if got := ReadSession(doc, renewed); got == nil || got.Nonce != payload.Nonce {
		t.Fatal("续期必须沿用同一个 nonce（否则登出会留下复活副本）")
	}
	// 登出（用续期后的令牌）后，两份都失效。
	h.do(http.MethodPost, "/api/auth/logout", "{}", map[string]string{
		"Cookie": CookieName + "=" + renewed, "X-Admin-Request": "1"})
	reloaded, _ := h.server.creds.Load()
	if ReadSession(reloaded, token) != nil {
		t.Fatal("原令牌在登出后必须失效")
	}
	if ReadSession(reloaded, renewed) != nil {
		t.Fatal("续期令牌在登出后必须失效")
	}
}

func TestSessionRenewalSignsWithCurrentCredentials(t *testing.T) {
	// C14：换密后旧会话（含续期副本）必须失效——续期用的是当下的 sessionKey。
	h := newHarness(t)
	doc := h.writeCredentials()
	token := h.token(doc)
	rotated := Credentials{"username": "wbadmin", "password": doc.Password(), "sessionKey": strings.Repeat("cd", 32)}
	if err := h.server.creds.Save(rotated); err != nil {
		t.Fatal(err)
	}
	if ReadSession(rotated, token) != nil {
		t.Fatal("换 sessionKey 后旧令牌必须失效")
	}
	// 用旧令牌访问 /api/session 会被判无效（进而 401）。
	recorder := h.do(http.MethodGet, "/api/session", "", map[string]string{"Cookie": CookieName + "=" + token})
	if recorder.Code != 401 {
		t.Fatalf("换密钥后旧 cookie 应 401，实际 %d", recorder.Code)
	}
}

// ── C13：改密 ─────────────────────────────────────────────────────────

func TestPasswordChangeValidationMessages(t *testing.T) {
	h := newHarness(t)
	doc := h.writeCredentials()
	cases := []struct {
		name    string
		body    map[string]any
		want    int
		message string
	}{
		{"当前密码错", map[string]any{"current": "nope", "username": "wbadmin", "password": "newpassword", "confirm": "newpassword"}, 400, "当前密码不正确"},
		{"新密码太短", map[string]any{"current": "secret-password", "username": "wbadmin", "password": "short", "confirm": "short"}, 400, "新密码至少 8 位"},
		{"两次不一致", map[string]any{"current": "secret-password", "username": "wbadmin", "password": "newpassword", "confirm": "otherpassword"}, 400, "两次输入的新密码不一致"},
		{"与当前相同", map[string]any{"current": "secret-password", "username": "wbadmin", "password": "secret-password", "confirm": "secret-password"}, 400, "新密码不能与当前密码相同"},
		{"用户名非法", map[string]any{"current": "secret-password", "username": "ab", "password": "newpassword", "confirm": "newpassword"}, 400, "用户名需为 3-32 位字母、数字、下划线、点或连字符"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			recorder := h.postJSON(doc, "/api/auth/password", tc.body)
			if recorder.Code != tc.want {
				t.Fatalf("应 %d，实际 %d：%s", tc.want, recorder.Code, recorder.Body.String())
			}
			if got := messageOf(t, recorder); got != tc.message {
				t.Fatalf("文案不对：%q，期望 %q", got, tc.message)
			}
		})
	}
}

func TestPasswordChangeRotatesSessionKey(t *testing.T) {
	h := newHarness(t)
	doc := h.writeCredentials()
	recorder := h.postJSON(doc, "/api/auth/password", map[string]any{
		"current": "secret-password", "username": "wbadmin",
		"password": "brand-new-password", "confirm": "brand-new-password"})
	if recorder.Code != 200 {
		t.Fatalf("应 200，实际 %d：%s", recorder.Code, recorder.Body.String())
	}
	fresh, err := h.server.creds.Load()
	if err != nil {
		t.Fatal(err)
	}
	if fresh["sessionKey"] == doc["sessionKey"] {
		t.Fatal("换密必须换 sessionKey（否则旧 cookie 还能用）")
	}
	if !VerifyPassword("brand-new-password", fresh.Password()) {
		t.Fatal("新口令必须可校验")
	}
	if got := jsonString(decode(t, recorder)["message"]); got != "账号信息已更新，其他设备上的登录已失效" {
		t.Fatalf("文案不对：%q", got)
	}
}

// ── C36/C37：账号启停与删除 ───────────────────────────────────────────

func TestAccountToggleDetectsDuplicatePair(t *testing.T) {
	h := newHarness(t)
	doc := h.writeCredentials()
	authDir := h.server.opts.AuthDir
	writeAuthFixture(t, authDir, "workbuddy-abc12345.json", "abc12345")
	writeAuthFixture(t, authDir, "workbuddy-abc12345.json.disabled", "abc12345")

	recorder := h.postJSON(doc, "/api/account/toggle", map[string]any{"uid": "abc12345", "disabled": true})
	if recorder.Code != 200 {
		t.Fatalf("应 200（业务失败也走 200），实际 %d", recorder.Code)
	}
	body := decode(t, recorder)
	if jsonBool(body["ok"], true) {
		t.Fatal("存在同名两份凭据时必须报失败")
	}
	if got := jsonString(body["message"]); got != "存在同名的启用与禁用凭据，已保留两份文件，请先核对重复账号" {
		t.Fatalf("文案不对：%q", got)
	}
	// 两份文件都必须在原地。
	for _, name := range []string{"workbuddy-abc12345.json", "workbuddy-abc12345.json.disabled"} {
		if _, err := os.Stat(filepath.Join(authDir, name)); err != nil {
			t.Fatalf("文件 %s 不应被移动：%v", name, err)
		}
	}
	if len(h.runner.recorded()) != 0 {
		t.Fatalf("冲突时不得重启容器：%v", h.runner.recorded())
	}
}

func TestAccountToggleRenamesAndReloads(t *testing.T) {
	h := newHarness(t)
	doc := h.writeCredentials()
	writeAuthFixture(t, h.server.opts.AuthDir, "workbuddy-abc12345.json", "abc12345")
	// 2026-10-15 改：账号启停不再重启容器，改走进程内重载（POST /runtime/reload）。
	// 重启一次几十秒还打断在途请求，而 auth 文件的改动只需网关重扫目录。
	h.transport.replies["POST /runtime/reload"] = stubReply{Code: 200, Body: `{"ok":true}`}

	recorder := h.postJSON(doc, "/api/account/toggle", map[string]any{"uid": "abc12345", "disabled": true})
	body := decode(t, recorder)
	if !jsonBool(body["ok"], false) {
		t.Fatalf("应成功：%v", body)
	}
	if _, err := os.Stat(filepath.Join(h.server.opts.AuthDir, "workbuddy-abc12345.json.disabled")); err != nil {
		t.Fatalf("停用应是改名加 .disabled：%v", err)
	}
	if len(h.runner.recorded()) != 0 {
		t.Fatalf("账号启停不得重启容器（应走进程内重载）：%v", h.runner.recorded())
	}
	// 重载必须真的发出去，否则改动不生效（这正是用户抱怨的「要重启才生效」）。
	if h.transport.count("POST /runtime/reload") == 0 {
		t.Fatalf("改动账号后必须触发进程内重载：%v", h.transport.recorded())
	}
	if got := jsonString(body["message"]); !strings.Contains(got, "立即生效") {
		t.Fatalf("文案应说明立即生效：%q", got)
	}
}

// TestTaskToggleAppliesWithoutRestart 任务开关改完**立即生效**，不重启容器。
//
// 2026-10-15 用户反馈「任务启停都要重启网关才行，这合理吗」。旧实现只写
// config.json，而排程器在启动时把开关抄进了自己的 Config——不重启就不生效。
// 现在写完文件后调网关的 POST /tasks/toggle（原地改运行期覆盖 + 叫醒排程器）。
//
// 判据有两面：**必须**发出 /tasks/toggle（否则开关不生效），且**不得**重启容器
// （重启一次几十秒还打断在途请求，与「改一个布尔值」的代价完全不成比例）。
func TestTaskToggleAppliesWithoutRestart(t *testing.T) {
	h := newHarness(t)
	doc := h.writeCredentials()
	h.transport.replies["POST /tasks/toggle"] = stubReply{Code: 200, Body: `{"ok":true}`}

	recorder := h.postJSON(doc, "/api/task/toggle", map[string]any{"key": "checkin", "enabled": false})
	body := decode(t, recorder)
	if !jsonBool(body["ok"], false) {
		t.Fatalf("应成功：%v", body)
	}
	if h.transport.count("POST /tasks/toggle") == 0 {
		t.Fatalf("必须调网关原地生效：%v", h.transport.recorded())
	}
	if len(h.runner.recorded()) != 0 {
		t.Fatalf("任务开关不得重启容器：%v", h.runner.recorded())
	}
	if got := jsonString(body["message"]); !strings.Contains(got, "立即生效") {
		t.Fatalf("文案应说明立即生效：%q", got)
	}
	// config.json 也必须真的写对了——原地生效是加速器，文件才是真值。
	raw, err := os.ReadFile(filepath.Join(h.dir, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"checkin_enabled": false`) {
		t.Fatalf("config.json 应已写入新开关：%s", raw)
	}
}

// TestTaskToggleFallsBackToNoticeWhenGatewayRejects 管理通道不可用时如实降级。
//
// 文件已经写对了（那才是真值），但原地生效没做成。此时**不能**谎报「立即生效」，
// 也不能把整个操作判失败——要明说「重启后生效」，让用户知道文件改对了。
func TestTaskToggleFallsBackToNoticeWhenGatewayRejects(t *testing.T) {
	h := newHarness(t)
	doc := h.writeCredentials()
	h.transport.replies["POST /tasks/toggle"] = stubReply{
		Code: 400, Body: `{"error":{"message":"未知任务：checkin"}}`}

	recorder := h.postJSON(doc, "/api/task/toggle", map[string]any{"key": "checkin", "enabled": false})
	body := decode(t, recorder)
	// 文件改对了 → 操作本身成功；但文案必须如实说明要靠重启。
	if !jsonBool(body["ok"], false) {
		t.Fatalf("文件已改对，操作应成功：%v", body)
	}
	got := jsonString(body["message"])
	if strings.Contains(got, "立即生效") {
		t.Fatalf("原地生效失败时不得谎报立即生效：%q", got)
	}
	if !strings.Contains(got, "重启网关后生效") {
		t.Fatalf("应提示重启后生效：%q", got)
	}
}

func TestAccountToggleNoChangeSkipsRestart(t *testing.T) {
	h := newHarness(t)
	doc := h.writeCredentials()
	writeAuthFixture(t, h.server.opts.AuthDir, "workbuddy-abc12345.json.disabled", "abc12345")
	recorder := h.postJSON(doc, "/api/account/toggle", map[string]any{"uid": "abc12345", "disabled": true})
	body := decode(t, recorder)
	if got := jsonString(body["message"]); got != "状态未变化" {
		t.Fatalf("文案不对：%q", got)
	}
	if len(h.runner.recorded()) != 0 {
		t.Fatalf("状态未变化时不得重启：%v", h.runner.recorded())
	}
}

func TestAccountDeleteUsesUniqueTrashNames(t *testing.T) {
	h := newHarness(t)
	doc := h.writeCredentials()
	writeAuthFixture(t, h.server.opts.AuthDir, "workbuddy-aaa11111.json", "aaa11111")
	writeAuthFixture(t, h.server.opts.AuthDir, "workbuddy-bbb22222.json", "bbb22222")

	first := decode(t, h.postJSON(doc, "/api/account/delete", map[string]any{"uid": "aaa11111"}))
	second := decode(t, h.postJSON(doc, "/api/account/delete", map[string]any{"uid": "bbb22222"}))
	one, two := jsonString(first["trashed"]), jsonString(second["trashed"])
	if one == "" || two == "" || one == two {
		t.Fatalf("同一秒内两次删除的回收件名必须不同：%q / %q", one, two)
	}
	// 名字形如 <file>.<YYYYmmdd-HHMMSS>.<12 hex>。
	for _, name := range []string{one, two} {
		parts := strings.Split(name, ".")
		if len(parts) != 4 || len(parts[3]) != 12 {
			t.Fatalf("回收件名格式不对：%q", name)
		}
	}
	entries, err := os.ReadDir(h.server.opts.TrashDir)
	if err != nil || len(entries) != 2 {
		t.Fatalf("两份回收件都应保留：%v %v", entries, err)
	}
}

// ── C52/C53/C54/C55/C56/C57：请求体字段校验 ──────────────────────────

func TestActionBodyValidation(t *testing.T) {
	h := newHarness(t)
	doc := h.writeCredentials()
	cases := []struct {
		name    string
		path    string
		body    map[string]any
		want    int
		message string
	}{
		{"多余字段", "/api/features/reasoning-loop", map[string]any{"stop_only": true, "extra": 1}, 400, "包含不支持的字段"},
		{"字符串字段类型错", "/api/account/toggle", map[string]any{"uid": 123, "disabled": true}, 400, "字段格式不正确：uid"},
		{"开关必须是布尔_字符串", "/api/account/toggle", map[string]any{"uid": "abc12345", "disabled": "false"}, 400, "开关值必须明确指定为 true 或 false"},
		{"开关必须是布尔_数字", "/api/account/toggle", map[string]any{"uid": "abc12345", "disabled": 1}, 400, "开关值必须明确指定为 true 或 false"},
		{"开关缺失", "/api/task/toggle", map[string]any{"key": "checkin"}, 400, "开关值必须明确指定为 true 或 false"},
		{"uid 非法", "/api/account/delete", map[string]any{"uid": "bad uid!"}, 400, "账号标识不正确"},
		{"uid 超长", "/api/account/delete", map[string]any{"uid": strings.Repeat("a", 81)}, 400, "账号标识不正确"},
		{"任务标识非法", "/api/task/run", map[string]any{"key": "nope"}, 400, "任务标识不正确"},
		{"任务标识非字符串", "/api/task/run", map[string]any{"key": []any{}}, 400, "字段格式不正确：key"},
		{"登录流程标识非法", "/api/login/poll", map[string]any{"realm": "cn", "login_id": "zz"}, 400, "登录流程标识无效，请重新生成链接"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			recorder := h.postJSON(doc, tc.path, tc.body)
			if recorder.Code != tc.want {
				t.Fatalf("应 %d，实际 %d：%s", tc.want, recorder.Code, recorder.Body.String())
			}
			if got := messageOf(t, recorder); got != tc.message {
				t.Fatalf("文案不对：%q，期望 %q", got, tc.message)
			}
		})
	}
}

// ── C39–C45：运维只读通道与密钥 ───────────────────────────────────────

func TestOpsGetQueryValidation(t *testing.T) {
	h := newHarness(t)
	doc := h.writeCredentials()
	cookie := CookieName + "=" + h.token(doc)
	cases := []struct {
		name    string
		path    string
		want    int
		message string
	}{
		{"限流接口不接受参数", "/api/key-limits?limit=1", 400, "限流状态接口不接受筛选参数"},
		{"筛选项接口不接受参数", "/api/requests/facets?x=1", 400, "筛选项接口不接受参数"},
		{"重复参数", "/api/requests?limit=1&limit=2", 400, "包含重复或不支持的筛选参数"},
		{"未知参数", "/api/requests?secret=1", 400, "包含重复或不支持的筛选参数"},
		{"limit 超范围", "/api/requests?limit=101", 400, "分页参数无效"},
		{"limit 为零", "/api/requests?limit=0", 400, "分页参数无效"},
		{"limit 非数字", "/api/requests?limit=abc", 400, "分页参数无效"},
		{"offset 为负", "/api/requests?offset=-1", 400, "分页参数无效"},
		{"状态非法", "/api/requests?status=bogus", 400, "请求状态无效"},
		{"请求标识含斜杠", "/api/requests/..%2Fkeys", 400, "请求标识不正确"},
		{"请求标识带查询串", "/api/requests/abc123?x=1", 400, "请求标识不正确"},
		{"筛选条件过长", "/api/requests?" + strings.Repeat("a", 2100), 400, "筛选条件过长"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			recorder := h.do(http.MethodGet, tc.path, "", map[string]string{"Cookie": cookie})
			if recorder.Code != tc.want {
				t.Fatalf("应 %d，实际 %d：%s", tc.want, recorder.Code, recorder.Body.String())
			}
			if got := messageOf(t, recorder); got != tc.message {
				t.Fatalf("文案不对：%q，期望 %q", got, tc.message)
			}
			if len(h.transport.recorded()) != 0 {
				t.Fatalf("被拒的请求不得转发给网关：%v", h.transport.recorded())
			}
		})
	}
}

func TestOpsGetForwardsValidatedQuery(t *testing.T) {
	h := newHarness(t)
	doc := h.writeCredentials()
	h.transport.replies["GET /requests"] = stubReply{Code: 200, Body: `{"ok":true,"items":[]}`}
	recorder := h.do(http.MethodGet, "/api/requests?model=cn%3Adeep&limit=10", "",
		map[string]string{"Cookie": CookieName + "=" + h.token(doc)})
	if recorder.Code != 200 {
		t.Fatalf("应 200，实际 %d", recorder.Code)
	}
	calls := h.transport.recorded()
	if len(calls) != 1 || calls[0].Method != "GET" {
		t.Fatalf("应恰好一次 GET 转发：%v", calls)
	}
	if !strings.HasPrefix(calls[0].Endpoint, "/requests?") {
		t.Fatalf("应转发到 /requests 并带上校验过的查询串：%q", calls[0].Endpoint)
	}
}

func TestOpsGetPassesThroughUpstreamStatus(t *testing.T) {
	h := newHarness(t)
	doc := h.writeCredentials()
	h.transport.replies["GET /key-limits"] = stubReply{Code: 503, Body: `{"ok":false,"message":"上游不可用"}`}
	recorder := h.do(http.MethodGet, "/api/key-limits", "", map[string]string{"Cookie": CookieName + "=" + h.token(doc)})
	if recorder.Code != 503 {
		t.Fatalf("上游状态码必须原样透传，实际 %d", recorder.Code)
	}
}

func TestKeysPostValidatesLimits(t *testing.T) {
	h := newHarness(t)
	doc := h.writeCredentials()
	const limitsMessage = "限流需为整数：每分钟 0–60000 次、并发 0–256、等待 0–30 秒；排队需先设置并发上限"
	bad := []any{
		true, []any{}, 1,
		map[string]any{"typo": 1},
		map[string]any{"requests_per_minute": -1},
		map[string]any{"requests_per_minute": 60001},
		map[string]any{"max_concurrent": 257},
		map[string]any{"queue_timeout_seconds": 31},
		map[string]any{"queue_timeout_seconds": 1},
		map[string]any{"requests_per_minute": true},
		map[string]any{"max_concurrent": nil},
		map[string]any{"max_concurrent": "2"},
		map[string]any{"max_concurrent": 2.5},
	}
	for index, value := range bad {
		t.Run(fmt.Sprintf("非法限流_%d", index), func(t *testing.T) {
			recorder := h.postJSON(doc, "/api/keys", map[string]any{"name": "k", "limits": value})
			if recorder.Code != 400 {
				t.Fatalf("应 400，实际 %d：%s", recorder.Code, recorder.Body.String())
			}
			if got := messageOf(t, recorder); got != limitsMessage {
				t.Fatalf("文案不对：%q", got)
			}
		})
	}
	// 合法值必须放行（含 null = 恢复无限制）。
	good := []any{
		nil,
		map[string]any{"requests_per_minute": 0, "max_concurrent": 0, "queue_timeout_seconds": 0},
		map[string]any{"requests_per_minute": 60000, "max_concurrent": 256, "queue_timeout_seconds": 30},
		map[string]any{"max_concurrent": 2, "queue_timeout_seconds": 5},
	}
	h.transport.replies["POST /keys"] = stubReply{Code: 201, Body: `{"ok":true,"entry":{},"key":"sk-x"}`}
	for index, value := range good {
		t.Run(fmt.Sprintf("合法限流_%d", index), func(t *testing.T) {
			recorder := h.postJSON(doc, "/api/keys", map[string]any{"name": "k", "limits": value})
			if recorder.Code != 201 {
				t.Fatalf("应 201，实际 %d：%s", recorder.Code, recorder.Body.String())
			}
		})
	}
}

func TestKeysUpdateStripsIDAndValidates(t *testing.T) {
	h := newHarness(t)
	doc := h.writeCredentials()
	h.transport.replies["PATCH /keys/key_0123456789abcdef01234567"] = stubReply{Code: 200, Body: `{"ok":true,"entry":{"id":"key_0123456789abcdef01234567"}}`}
	keyID := "key_0123456789abcdef01234567"

	t.Run("非法标识", func(t *testing.T) {
		recorder := h.postJSON(doc, "/api/keys/update", map[string]any{"id": "bogus", "name": "x"})
		if recorder.Code != 400 || messageOf(t, recorder) != "密钥标识不正确" {
			t.Fatalf("应 400 密钥标识不正确，实际 %d %s", recorder.Code, recorder.Body.String())
		}
	})
	t.Run("没有有效修改字段", func(t *testing.T) {
		recorder := h.postJSON(doc, "/api/keys/update", map[string]any{"id": keyID})
		if recorder.Code != 400 || messageOf(t, recorder) != "没有有效的修改字段" {
			t.Fatalf("应 400 没有有效的修改字段，实际 %d %s", recorder.Code, recorder.Body.String())
		}
	})
	t.Run("合法修改会剥掉 id", func(t *testing.T) {
		recorder := h.postJSON(doc, "/api/keys/update", map[string]any{"id": keyID, "name": "新名字"})
		if recorder.Code != 200 {
			t.Fatalf("应 200，实际 %d：%s", recorder.Code, recorder.Body.String())
		}
		calls := h.transport.recorded()
		last := calls[len(calls)-1]
		if last.Method != "PATCH" || last.Endpoint != "/keys/"+keyID {
			t.Fatalf("应 PATCH /keys/<id>：%v", last)
		}
		if strings.Contains(last.Body, `"id"`) {
			t.Fatalf("转发体里不得带 id：%s", last.Body)
		}
	})
	t.Run("limits null 显式清空", func(t *testing.T) {
		recorder := h.postJSON(doc, "/api/keys/update", map[string]any{"id": keyID, "limits": nil})
		if recorder.Code != 200 {
			t.Fatalf("应 200，实际 %d：%s", recorder.Code, recorder.Body.String())
		}
		calls := h.transport.recorded()
		last := calls[len(calls)-1]
		if !strings.Contains(last.Body, `"limits":null`) {
			t.Fatalf("limits:null 必须显式转发（表示清空）：%s", last.Body)
		}
	})
	t.Run("非法模型绑定", func(t *testing.T) {
		recorder := h.postJSON(doc, "/api/keys/update", map[string]any{"id": keyID, "models": []any{"deep"}})
		if recorder.Code != 400 {
			t.Fatalf("裸模型名必须被拒，实际 %d：%s", recorder.Code, recorder.Body.String())
		}
		if got := messageOf(t, recorder); got != "模型绑定必须选完整模型名（cn: / global: / trae: 开头），请从模型列表添加" {
			t.Fatalf("文案不对：%q", got)
		}
	})
}

func TestKeysPostRejectsForeignOrigin(t *testing.T) {
	h := newHarness(t)
	doc := h.writeCredentials()
	recorder := h.do(http.MethodPost, "/api/keys", `{"name":"k"}`, map[string]string{
		"Cookie": CookieName + "=" + h.token(doc), "X-Admin-Request": "1",
		"Origin": "http://127.0.0.1:1"})
	if recorder.Code != 403 {
		t.Fatalf("应 403，实际 %d", recorder.Code)
	}
	if len(h.transport.recorded()) != 0 {
		t.Fatal("异源请求不得转发给网关")
	}
}

// ── C46–C50：更新通道 ─────────────────────────────────────────────────

func TestUpdateChannel(t *testing.T) {
	h := newHarness(t)
	doc := h.writeCredentials()
	cookie := CookieName + "=" + h.token(doc)
	h.transport.replies["GET /update"] = stubReply{Code: 200, Body: `{"ok":true,"status":{"current":"v1"}}`}
	h.transport.replies["POST /update/apply"] = stubReply{Code: 200, Body: `{"ok":true,"applied":true}`}

	t.Run("读状态无会话", func(t *testing.T) {
		recorder := h.do(http.MethodGet, "/api/update", "", nil)
		if recorder.Code != 401 {
			t.Fatalf("应 401，实际 %d", recorder.Code)
		}
		if len(h.transport.recorded()) != 0 {
			t.Fatal("无会话不得调用网关")
		}
	})
	t.Run("读状态", func(t *testing.T) {
		recorder := h.do(http.MethodGet, "/api/update", "", map[string]string{"Cookie": cookie})
		if recorder.Code != 200 || !strings.Contains(recorder.Body.String(), `"current":"v1"`) {
			t.Fatalf("应原样透传 /update：%d %s", recorder.Code, recorder.Body.String())
		}
	})
	t.Run("标签含路径穿越", func(t *testing.T) {
		recorder := h.postJSON(doc, "/api/update/apply", map[string]any{"tag": "../etc/passwd"})
		if recorder.Code != 400 || messageOf(t, recorder) != "版本号格式不正确" {
			t.Fatalf("应 400 版本号格式不正确，实际 %d %s", recorder.Code, recorder.Body.String())
		}
	})
	t.Run("标签过长", func(t *testing.T) {
		recorder := h.postJSON(doc, "/api/update/apply", map[string]any{"tag": strings.Repeat("a", 65)})
		if recorder.Code != 400 {
			t.Fatalf("应 400，实际 %d", recorder.Code)
		}
	})
	t.Run("多余字段", func(t *testing.T) {
		recorder := h.postJSON(doc, "/api/update/apply", map[string]any{"force": true})
		if recorder.Code != 400 || messageOf(t, recorder) != "包含不支持的字段" {
			t.Fatalf("应 400 包含不支持的字段，实际 %d %s", recorder.Code, recorder.Body.String())
		}
	})
	t.Run("空体转发空体", func(t *testing.T) {
		before := len(h.transport.recorded())
		recorder := h.postJSON(doc, "/api/update/check", map[string]any{})
		if recorder.Code != 200 {
			t.Fatalf("应 200，实际 %d", recorder.Code)
		}
		calls := h.transport.recorded()[before:]
		if len(calls) != 1 || calls[0].Endpoint != "/update/check" {
			t.Fatalf("应 POST /update/check：%v", calls)
		}
	})
	t.Run("带标签转发", func(t *testing.T) {
		before := len(h.transport.recorded())
		recorder := h.postJSON(doc, "/api/update/apply", map[string]any{"tag": "v1.3.1"})
		if recorder.Code != 200 {
			t.Fatalf("应 200，实际 %d", recorder.Code)
		}
		calls := h.transport.recorded()[before:]
		if len(calls) != 1 || !strings.Contains(calls[0].Body, `"tag":"v1.3.1"`) {
			t.Fatalf("应带上 tag：%v", calls)
		}
	})
}

// ── C58/C59：重复推理保护热切换 ───────────────────────────────────────

func TestReasoningLoopToggle(t *testing.T) {
	h := newHarness(t)
	doc := h.writeCredentials()
	h.transport.replies["POST /features/reasoning-loop"] = stubReply{Code: 200, Body: `{"ok":true,"stop_only":true}`}
	recorder := h.postJSON(doc, "/api/features/reasoning-loop", map[string]any{"stop_only": true})
	if recorder.Code != 200 {
		t.Fatalf("应 200，实际 %d", recorder.Code)
	}
	body := decode(t, recorder)
	if !jsonBool(body["ok"], false) || jsonBool(body["stop_only"], false) != true {
		t.Fatalf("回包不对：%v", body)
	}
	if got := jsonString(body["message"]); got != "已切换为「命中即停止」" {
		t.Fatalf("文案不对：%q", got)
	}
	calls := h.transport.recorded()
	last := calls[len(calls)-1]
	if last.Method != "POST" || last.Endpoint != "/features/reasoning-loop" {
		t.Fatalf("应 POST /features/reasoning-loop：%v", last)
	}
	if !strings.Contains(last.Body, `"stop_only":true`) {
		t.Fatalf("转发体不对：%s", last.Body)
	}
}

func TestReasoningLoopToggleReportsGatewayFailure(t *testing.T) {
	h := newHarness(t)
	doc := h.writeCredentials()
	h.transport.replies["POST /features/reasoning-loop"] = stubReply{Code: 200, Body: `{"ok":false}`}
	recorder := h.postJSON(doc, "/api/features/reasoning-loop", map[string]any{"stop_only": false})
	if recorder.Code != 200 {
		t.Fatalf("失败也必须走 200 + ok:false，实际 %d", recorder.Code)
	}
	body := decode(t, recorder)
	if jsonBool(body["ok"], true) {
		t.Fatal("网关没成功时不得报成功")
	}
	if got := jsonString(body["message"]); got != "网关未响应（旧版本网关请先升级）" {
		t.Fatalf("文案不对：%q", got)
	}
}

// ── C31–C34：账号文件枚举 ─────────────────────────────────────────────

func TestListAuthFilesCoversWideGlob(t *testing.T) {
	h := newHarness(t)
	authDir := h.server.opts.AuthDir
	writeAuthFixture(t, authDir, "workbuddy-aaa11111.json", "aaa11111")
	writeAuthFixture(t, authDir, "workbuddy-bbb22222.json.disabled", "bbb22222")
	// 无连字符命名必须可见（网关会加载它们，面板看不到就管不了）。
	if err := os.WriteFile(filepath.Join(authDir, "workbuddy_new.json"), []byte(`{"uid":"newuid01"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(authDir, "workbuddy.json"), []byte(`{}`), 0600); err != nil {
		t.Fatal(err)
	}
	// 不匹配的文件必须被忽略。
	for _, name := range []string{"notes.txt", "other.json", "workbuddy-broken.txt"} {
		if err := os.WriteFile(filepath.Join(authDir, name), []byte("x"), 0600); err != nil {
			t.Fatal(err)
		}
	}

	entries := h.server.store.List()
	byFile := map[string]AuthEntry{}
	for _, entry := range entries {
		byFile[entry.File] = entry
	}
	for _, name := range []string{"workbuddy-aaa11111.json", "workbuddy-bbb22222.json.disabled",
		"workbuddy_new.json", "workbuddy.json"} {
		if _, ok := byFile[name]; !ok {
			t.Fatalf("%s 必须被列出，实际 %v", name, byFile)
		}
	}
	if len(entries) != 4 {
		t.Fatalf("应恰好列出 4 条，实际 %d：%v", len(entries), byFile)
	}
	if !byFile["workbuddy-bbb22222.json.disabled"].Disabled {
		t.Fatal(".disabled 必须标记为已停用")
	}
	// uid 优先取内容：workbuddy_new.json 的内容里是 newuid01，不能回落到文件名。
	if got := byFile["workbuddy_new.json"].UID; got != "newuid01" {
		t.Fatalf("uid 必须优先取文件内容，实际 %q", got)
	}
	// workbuddy.json 内容里没有 uid → 回落到文件名中间段（去掉前导连字符）。
	if got := byFile["workbuddy.json"].UID; got != "" {
		t.Fatalf("空文件名段应回落成空串，实际 %q", got)
	}
	// 排序必须稳定（按文件名）。
	for i := 1; i < len(entries); i++ {
		if entries[i-1].File > entries[i].File {
			t.Fatalf("必须按文件名排序：%v", entries)
		}
	}
}

func TestListAuthFilesFallsBackToFilenameWhenCorrupt(t *testing.T) {
	h := newHarness(t)
	authDir := h.server.opts.AuthDir
	if err := os.WriteFile(filepath.Join(authDir, "workbuddy-f0f0f0f0.json"), []byte("{not json"), 0600); err != nil {
		t.Fatal(err)
	}
	entries := h.server.store.List()
	if len(entries) != 1 || entries[0].UID != "f0f0f0f0" {
		t.Fatalf("内容损坏时必须回落到文件名中间段：%v", entries)
	}
	if found := h.server.store.Find("f0f0f0f0"); found == nil {
		t.Fatal("回落后的 uid 必须仍能按 uid 找到（否则这类账号管不了）")
	}
}

func TestAuthSummaryBothDocumentShapes(t *testing.T) {
	h := newHarness(t)
	authDir := h.server.opts.AuthDir
	nested := `{"account":{"uid":"nested01","nickname":"昵称A","enterpriseId":"ent-1","mobile":"157******41"},` +
		`"auth":{"accessToken":"t","expiresAt":1800000000,"domain":"codebuddy.cn","realm":"cn"}}`
	flat := `{"uid":"flat0001","accessToken":"t","realm":"global","domain":"workbuddy.ai","expiresAt":"1800000001"}`
	if err := os.WriteFile(filepath.Join(authDir, "workbuddy-nested01.json"), []byte(nested), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(authDir, "workbuddy-flat0001.json"), []byte(flat), 0600); err != nil {
		t.Fatal(err)
	}
	byUID := map[string]AuthSummary{}
	for _, entry := range h.server.store.List() {
		byUID[entry.UID] = Summary(entry)
	}
	a := byUID["nested01"]
	if a.Nickname != "昵称A" || a.EnterpriseID != "ent-1" || a.Realm != "cn" ||
		a.Domain != "codebuddy.cn" || a.ExpiresAt != 1800000000 || !a.Parsable {
		t.Fatalf("嵌套形摘要不对：%+v", a)
	}
	// 手机号来自 account.mobile（Trae 加号时由 GetUserInfo 写入，掩码形态）。
	// 前端拿它替换 uid 显示，让运维能对上「这是谁的号」。
	if a.Mobile != "157******41" {
		t.Fatalf("mobile 未读出：%+v", a)
	}
	b := byUID["flat0001"]
	if b.Nickname != "flat0001" || b.Realm != "global" || b.ExpiresAt != 1800000001 || !b.Parsable {
		t.Fatalf("扁平形摘要不对（nickname 应回落到 uid）：%+v", b)
	}
	// 没有 mobile 字段时留空串——前端据此回落到 uid 显示。
	if b.Mobile != "" {
		t.Fatalf("无 mobile 时应留空：%+v", b)
	}
	// realm 缺失时留空串，由前端兜底成 cn——硬填会让「未标注域」与「明确 cn」无法区分。
	if err := os.WriteFile(filepath.Join(authDir, "workbuddy-norealm.json"), []byte(`{"uid":"norealm1"}`), 0600); err != nil {
		t.Fatal(err)
	}
	for _, entry := range h.server.store.List() {
		if entry.UID == "norealm1" {
			if got := Summary(entry).Realm; got != "" {
				t.Fatalf("缺 realm 必须留空串，实际 %q", got)
			}
		}
	}
}

// ── C35：auth 文件落盘 ────────────────────────────────────────────────

func TestWriteAuthFileValidatesAndLands(t *testing.T) {
	h := newHarness(t)
	s := h.server
	if _, err := s.writeAuthFile(map[string]any{"uid": "bad uid", "access_token": "t"}); err == nil {
		t.Fatal("非法 uid 必须报错")
	}
	if _, err := s.writeAuthFile(map[string]any{"uid": "good1234"}); err == nil {
		t.Fatal("缺 access_token 必须报错")
	}
	summary, err := s.writeAuthFile(map[string]any{
		"uid": "good1234", "access_token": "at", "refresh_token": "rt",
		"expires_in": float64(86400), "nickname": "昵称", "realm": "cn", "domain": "codebuddy.cn",
	})
	if err != nil {
		t.Fatal(err)
	}
	if summary["uid"] != "good1234" || summary["realm"] != "cn" {
		t.Fatalf("摘要不对：%v", summary)
	}
	path := filepath.Join(s.opts.AuthDir, "workbuddy-good1234.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("落盘必须是合法 JSON：%v", err)
	}
	account := jsonObject(doc["account"])
	auth := jsonObject(doc["auth"])
	if jsonString(account["uid"]) != "good1234" || jsonString(auth["accessToken"]) != "at" {
		t.Fatalf("落盘内容不对：%s", raw)
	}
	// 网关按内容里的 uid 索引，所以面板必须能按同一个 uid 找到它。
	if found := s.store.Find("good1234"); found == nil {
		t.Fatal("落盘后必须能按 uid 找到")
	}
}

func TestWriteAuthFileClearsStaleDisabledCopy(t *testing.T) {
	// 重新加同一个号时，旧的 .disabled 副本必须清掉，否则之后启停/删除会撞上
	// 「存在同名凭据」的冲突，账号卡死。
	h := newHarness(t)
	s := h.server
	stale := filepath.Join(s.opts.AuthDir, "workbuddy-good1234.json.disabled")
	if err := os.WriteFile(stale, []byte(`{}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.writeAuthFile(map[string]any{"uid": "good1234", "access_token": "at"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stale); err == nil {
		t.Fatal("重新加号后必须清掉同名的 .disabled 副本")
	}
}

// ── C61/C62：build_state ──────────────────────────────────────────────

func TestBuildStateShape(t *testing.T) {
	h := newHarness(t)
	doc := h.writeCredentials()
	_ = doc
	writeAuthFixture(t, h.server.opts.AuthDir, "workbuddy-aaa11111.json", "aaa11111")
	h.runner.credit = []byte(`{"accounts":[{"uid":"aaa11111","remain":100,"size":200,"used":100,` +
		`"expiring":10,"expiry":{"7d":10},"expiring_window_hours":72,"ok":true}],"total":{"remain":100}}`)
	h.transport.replies["GET /status"] = stubReply{Code: 200, Body: `{"total":1,"healthy":1,"cooling":0,"disabled":0,` +
		`"accounts":[{"uid":"aaa11111","cooling":true,"cool_remaining_sec":42,"cool_kind":"soft",` +
		`"reason":"限流","success_count":3,"err_total":4,"in_flight":1,"breaker_fails":0,` +
		`"credits_expiring":15}]}`}
	h.transport.replies["GET /healthz"] = stubReply{Code: 200, Body: `{"service":"workbuddy2api","version":"v9.9.9"}`}
	h.transport.replies["GET /inspect"] = stubReply{Code: 200, Body: `{"ok":true}`}

	payload := h.server.state.Build(context.Background(), true)
	if payload.Service != "wb2api-admin" {
		t.Fatalf("service 不对：%q", payload.Service)
	}
	// version 必须报**网关**版本，panelVersion 才是面板自身。
	if payload.Version != "v9.9.9" || payload.PanelVersion != PanelVersion {
		t.Fatalf("版本字段不对：version=%q panelVersion=%q", payload.Version, payload.PanelVersion)
	}
	if payload.APIKey != "" {
		t.Fatal("apiKey 必须恒为空串（不回显调用密钥）")
	}
	if !payload.APIKeysManaged {
		t.Fatal("apiKeysManaged 应为 true")
	}
	if len(payload.Accounts) != 1 {
		t.Fatalf("应有 1 个账号：%+v", payload.Accounts)
	}
	account := payload.Accounts[0]
	if !account.Credits.Supported {
		t.Fatal("credits.supported 必须为 true")
	}
	// expiring 取 ./credit 与 /status 的较大者（宁可多提醒，不可漏）。
	if account.Credits.Expiring != 15 {
		t.Fatalf("expiring 应取较大者 15，实际 %d", account.Credits.Expiring)
	}
	if !account.Pool.InPool || account.Pool.CoolRemaining != float64(42) ||
		account.Pool.CoolKind != "soft" || account.Pool.Reason != "限流" {
		t.Fatalf("冷却台账字段不对：%+v", account.Pool)
	}
	if account.Pool.Healthy {
		t.Fatal("冷却中的账号不得算健康")
	}

	// 序列化后的键名必须与 Python 版一致（前端按这些键取值）。
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{`"coolRemaining"`, `"coolKind"`, `"reason"`, `"successCount"`,
		`"errTotal"`, `"inPool"`, `"healthy"`, `"disabledReason"`, `"inFlight"`, `"breakerFails"`,
		`"lastSuccess"`, `"lastErr"`, `"credits"`, `"supported"`, `"expiringWindowHours"`,
		`"panelVersion"`, `"creditPending"`, `"pendingRestart"`, `"apiKeysManaged"`, `"containerRunning"`} {
		if !strings.Contains(string(raw), key) {
			t.Fatalf("状态响应缺少键 %s：%s", key, raw)
		}
	}
}

func TestBuildStatePendingRestartWhenDisabledButInPool(t *testing.T) {
	h := newHarness(t)
	writeAuthFixture(t, h.server.opts.AuthDir, "workbuddy-aaa11111.json.disabled", "aaa11111")
	h.runner.credit = []byte(`{"accounts":[]}`)
	h.transport.replies["GET /status"] = stubReply{Code: 200, Body: `{"accounts":[{"uid":"aaa11111"}]}`}
	payload := h.server.state.Build(context.Background(), false)
	if !payload.PendingRestart {
		t.Fatal("文件已停用但账号还在池里 → 必须提示需要重启")
	}
}

// ── 静态资源与入口 ────────────────────────────────────────────────────

func TestStaticRoutes(t *testing.T) {
	h := newHarness(t)
	doc := h.writeCredentials()
	cookie := CookieName + "=" + h.token(doc)

	t.Run("未登录访问根路径重定向到登录页", func(t *testing.T) {
		recorder := h.do(http.MethodGet, "/", "", nil)
		if recorder.Code != 302 || recorder.Header().Get("Location") != "login" {
			t.Fatalf("应 302 → login，实际 %d %q", recorder.Code, recorder.Header().Get("Location"))
		}
	})
	t.Run("登录页", func(t *testing.T) {
		recorder := h.do(http.MethodGet, "/login", "", nil)
		if recorder.Code != 200 || !strings.Contains(recorder.Body.String(), "login") {
			t.Fatalf("应返回 login.html：%d %s", recorder.Code, recorder.Body.String())
		}
	})
	t.Run("已登录访问登录页重定向", func(t *testing.T) {
		recorder := h.do(http.MethodGet, "/login", "", map[string]string{"Cookie": cookie})
		if recorder.Code != 302 {
			t.Fatalf("应 302，实际 %d", recorder.Code)
		}
	})
	t.Run("首页", func(t *testing.T) {
		recorder := h.do(http.MethodGet, "/", "", map[string]string{"Cookie": cookie})
		if recorder.Code != 200 || !strings.Contains(recorder.Body.String(), "panel") {
			t.Fatalf("应返回 index.html：%d %s", recorder.Code, recorder.Body.String())
		}
	})
	t.Run("vendor 静态资源需要会话", func(t *testing.T) {
		recorder := h.do(http.MethodGet, "/vendor/qrcode.js", "", map[string]string{"Cookie": cookie})
		if recorder.Code != 200 {
			t.Fatalf("应 200，实际 %d", recorder.Code)
		}
		if got := recorder.Header().Get("Content-Type"); !strings.HasPrefix(got, "application/javascript") {
			t.Fatalf("Content-Type 不对：%q", got)
		}
	})
	t.Run("未登录的 vendor 请求被重定向而不是 404", func(t *testing.T) {
		// 与 Python 版同序：vendor 在会话检查之后。未登录时落到重定向分支。
		recorder := h.do(http.MethodGet, "/vendor/qrcode.js", "", nil)
		if recorder.Code != 302 {
			t.Fatalf("应 302，实际 %d", recorder.Code)
		}
	})
	t.Run("vendor 路径穿越被挡", func(t *testing.T) {
		recorder := h.do(http.MethodGet, "/vendor/../../etc/passwd", "", map[string]string{"Cookie": cookie})
		if recorder.Code == 200 {
			t.Fatal("vendor 下不得读到目录外的文件")
		}
	})
	t.Run("未知接口 404", func(t *testing.T) {
		recorder := h.do(http.MethodGet, "/api/nope", "", map[string]string{"Cookie": cookie})
		if recorder.Code != 404 {
			t.Fatalf("应 404，实际 %d", recorder.Code)
		}
	})
	t.Run("未登录访问 API 得 401", func(t *testing.T) {
		recorder := h.do(http.MethodGet, "/api/state", "", nil)
		if recorder.Code != 401 {
			t.Fatalf("应 401，实际 %d", recorder.Code)
		}
		if !strings.Contains(recorder.Body.String(), "登录已失效") {
			t.Fatalf("401 必须可解析：%s", recorder.Body.String())
		}
	})
}

func TestSecurityHeadersOnEveryResponse(t *testing.T) {
	h := newHarness(t)
	recorder := h.do(http.MethodGet, "/login", "", nil)
	for name, want := range map[string]string{
		"Cache-Control":          "no-store",
		"X-Content-Type-Options": "nosniff",
		"Referrer-Policy":        "same-origin",
		"X-Frame-Options":        "SAMEORIGIN",
	} {
		if got := recorder.Header().Get(name); got != want {
			t.Fatalf("%s 应为 %q，实际 %q", name, want, got)
		}
	}
	if got := recorder.Header().Get("Content-Security-Policy"); !strings.Contains(got, "frame-ancestors 'self'") {
		t.Fatalf("CSP 不对：%q", got)
	}
}

func TestAdminAllowlistRejectsForeignClient(t *testing.T) {
	h := newHarness(t)
	// 白名单只放行 203.0.113.0/24；请求来自 127.0.0.1（回环恒放行）。
	h.server.throttle = NewThrottle("", "203.0.113.0/24")
	allowed := h.do(http.MethodGet, "/login", "", nil)
	if allowed.Code != 200 {
		t.Fatalf("回环必须始终放行，实际 %d", allowed.Code)
	}
	// 非白名单来源（模拟直连对端是公网地址）。
	req := httptest.NewRequest(http.MethodGet, "/login", nil)
	req.RemoteAddr = "198.51.100.7:1234"
	recorder := httptest.NewRecorder()
	h.server.ServeHTTP(recorder, req)
	if recorder.Code != 403 {
		t.Fatalf("应 403，实际 %d", recorder.Code)
	}
	if got := messageOf(t, recorder); got != "当前来源不在管理入口白名单内" {
		t.Fatalf("文案不对：%q", got)
	}
}

func TestHealthEndpointOnlyInNativeMode(t *testing.T) {
	h := newHarness(t)
	recorder := h.do(http.MethodGet, "/__health", "", nil)
	if recorder.Code != 200 {
		t.Fatalf("原生模式下 /__health 应 200，实际 %d", recorder.Code)
	}
	body := decode(t, recorder)
	if jsonString(body["version"]) != "2.5.0" {
		t.Fatalf("版本不对：%v", body)
	}
	// 非原生模式（容器）下该路径不对外。
	other, err := New(Options{AdminDir: h.dir, AuthDir: h.dir, ConfigPath: filepath.Join(h.dir, "config.json"),
		BaseDir: h.dir, Assets: fs.FS(fstest.MapFS{})})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/__health", nil)
	req.RemoteAddr = "127.0.0.1:1"
	recorder2 := httptest.NewRecorder()
	other.ServeHTTP(recorder2, req)
	if recorder2.Code == 200 {
		t.Fatal("容器模式下 /__health 不该由面板响应")
	}
}

// ── Trae 回调页 ───────────────────────────────────────────────────────

func TestTraeCallbackPageIsSelfContained(t *testing.T) {
	h := newHarness(t)
	// 无会话、无同源头也必须能进来（浏览器从 Trae 授权页跳回来时什么都没有）。
	h.transport.replies["POST /runtime/reload"] = stubReply{Code: 200, Body: `{"ok":true}`}
	// 与 panel/test_trae_login.py 的 fixture 同形：userJwt 带 Token/RefreshToken，
	// userInfo 是**另一个顶层参数**（uid 与昵称从它取，不是从 userJwt 里嵌的那份）。
	userJwt := `{"Token":"jwt-token","RefreshToken":"refresh-token","ClientID":"ono9krqynydwx5"}`
	userInfo := `{"UserID":"trae0001","ScreenName":"昵称","Region":"CN"}`
	query := "userJwt=" + url.QueryEscape(userJwt) + "&userInfo=" + url.QueryEscape(userInfo)
	recorder := h.do(http.MethodGet, "/trae/callback?"+query, "", nil)
	if recorder.Code != 200 {
		t.Fatalf("应 200，实际 %d：%s", recorder.Code, recorder.Body.String())
	}
	// 页面必须自包含（不能引用面板的静态资源，因为这条路径没有会话）。
	if !strings.Contains(recorder.Body.String(), "<!doctype html>") {
		t.Fatalf("应是自包含 HTML：%s", recorder.Body.String())
	}
	// 绝不能回显凭据内容。
	if strings.Contains(recorder.Body.String(), "jwt-token") {
		t.Fatal("回调页不得回显任何凭据内容")
	}
	// 落盘的账号必须能被网关扫到（文件名匹配 workbuddy*.json）。
	entries := h.server.store.List()
	found := false
	for _, entry := range entries {
		if entry.UID == "trae0001" {
			found = true
		}
	}
	if !found {
		t.Fatalf("Trae 账号必须落盘成网关可见的文件：%v", entries)
	}
}

func TestTraeCallbackRejectsBadInput(t *testing.T) {
	h := newHarness(t)
	recorder := h.do(http.MethodGet, "/trae/callback", "", nil)
	if recorder.Code != 400 {
		t.Fatalf("空回调应 400，实际 %d", recorder.Code)
	}
	if !strings.Contains(recorder.Body.String(), "没有收到内容") {
		t.Fatalf("必须给出可读原因：%s", recorder.Body.String())
	}
}

// ── 辅助 ──────────────────────────────────────────────────────────────

// writeAuthFixture 写一个最小的嵌套形 auth 文件。
func writeAuthFixture(t *testing.T, dir, name, uid string) {
	t.Helper()
	doc := fmt.Sprintf(`{"account":{"uid":%q,"nickname":"昵称"},"auth":{"accessToken":"t","expiresAt":1800000000,"realm":"cn"}}`, uid)
	if err := os.WriteFile(filepath.Join(dir, name), []byte(doc), 0600); err != nil {
		t.Fatal(err)
	}
}

// readAllLimited 读完一个请求体（测试替身用）。
func readAllLimited(body io.Reader) ([]byte, error) { return io.ReadAll(body) }

// newReadCloser 把字符串包成 ReadCloser。
func newReadCloser(text string) io.ReadCloser { return io.NopCloser(strings.NewReader(text)) }

// queryOf 还原查询串（测试替身用）。
func queryOf(raw string) string {
	if raw == "" {
		return ""
	}
	return "?" + raw
}

// countFor 是测试专用的计数读取（同包 _test 文件里声明方法，不进生产二进制）。
func (t *Throttle) countFor(ip string) int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.fails[bucketKey(ip)])
}

// ── C4/C7/C8：被拒绝的请求必须可读、且迟到请求体不得把连接钉住 ──────────
//
// 这组测试必须走**真实 TCP**，不能用 httptest.NewRequest：要验证的正是
// 「响应先发出、请求体后到」这种分帧行为，而内存里的假请求没有连接可谈。

// rawReject 发一个「先发头、拿到响应后再补发请求体」的请求。
func rawReject(t *testing.T, addr, path string, headers map[string]string) (string, string, string) {
	t.Helper()
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := conn.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	var head strings.Builder
	head.WriteString("POST " + path + " HTTP/1.1\r\nHost: " + addr + "\r\n")
	for name, value := range headers {
		head.WriteString(name + ": " + value + "\r\n")
	}
	head.WriteString("\r\n")
	if _, err := conn.Write([]byte(head.String())); err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(conn)
	status, err := reader.ReadString('\n')
	if err != nil {
		t.Fatalf("没收到响应行（响应必须在请求体之前发出）：%v", err)
	}
	// 现在才把请求体补上——服务端应当已经回了拒绝响应并准备关连接。
	_, _ = conn.Write([]byte("{}"))
	rest := &strings.Builder{}
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			break
		}
		if strings.TrimSpace(line) == "" {
			break
		}
		rest.WriteString(line)
	}
	body := make([]byte, 0, 256)
	buf := make([]byte, 256)
	for {
		n, err := reader.Read(buf)
		body = append(body, buf[:n]...)
		if err != nil {
			break
		}
	}
	return strings.TrimSpace(status), rest.String(), string(body)
}

func TestRejectedRequestsStayReadableWithLateBody(t *testing.T) {
	h := newHarness(t)
	server := httptest.NewServer(h.server)
	defer server.Close()
	addr := strings.TrimPrefix(server.URL, "http://")

	cases := []struct {
		name    string
		path    string
		headers map[string]string
		want    string
	}{
		{"异源", "/api/login/start", map[string]string{
			"Content-Type": "application/json", "Content-Length": "2",
			"Cookie": CookieName + "=" + h.token(h.writeCredentials()), "X-Admin-Request": "1",
			"Origin": "https://untrusted.invalid"}, "403"},
		{"缺管理标记", "/api/login/start", map[string]string{
			"Content-Type": "application/json", "Content-Length": "2",
			"Cookie": CookieName + "=" + h.token(h.writeCredentials()), "X-Admin-Request": ""}, "403"},
		{"无会话的密钥接口", "/api/keys/copy", map[string]string{
			"Content-Type": "application/json", "Content-Length": "2", "X-Admin-Request": "1"}, "401"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			status, headers, body := rawReject(t, addr, tc.path, tc.headers)
			if !strings.Contains(status, tc.want) {
				t.Fatalf("应 %s，实际 %q", tc.want, status)
			}
			// 必须显式声明关连接：不声明的话客户端会以为还能复用这条连接，
			// 而我们刚刚把它的请求体丢掉了。
			if !strings.Contains(strings.ToLower(headers), "connection: close") {
				t.Fatalf("拒绝响应必须带 Connection: close：%q", headers)
			}
			if strings.TrimSpace(body) == "" {
				t.Fatal("拒绝响应必须有可解析的 JSON 体（前端靠它显示原因）")
			}
			if !strings.Contains(body, `"message"`) {
				t.Fatalf("响应体必须带 message：%q", body)
			}
			// 绝不能出现第二个响应（那正是 Python 侧修掉的 429+500 那个回归）。
			if strings.Count(body, "HTTP/1.1") != 0 {
				t.Fatalf("不得有第二个响应：%q", body)
			}
		})
	}
}

func TestLateBodyDrainIsBounded(t *testing.T) {
	// 排空必须有硬上限与硬时限：一个声明 1e9 字节的请求体不能把处理协程钉住。
	//
	// 可观测的判据是「服务端在有限时间内把连接关掉」——如果排空写成
	// io.Copy(io.Discard, r.Body)，它会一直读到客户端把 1e9 字节发完（永远发不完），
	// 连接永不关闭，本测试就会超时失败。
	h := newHarness(t)
	server := httptest.NewServer(h.server)
	defer server.Close()
	addr := strings.TrimPrefix(server.URL, "http://")

	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(15 * time.Second))
	request := "POST /api/keys/copy HTTP/1.1" + crlf + "Host: " + addr + crlf +
		"Content-Type: application/json" + crlf + "Content-Length: 1000000000" + crlf +
		"X-Admin-Request: 1" + crlf + crlf
	if _, err := conn.Write([]byte(request)); err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(conn)
	if _, err := reader.ReadString('\n'); err != nil {
		t.Fatalf("没收到响应行：%v", err)
	}
	// 持续灌数据，直到服务端关掉连接。
	chunk := make([]byte, 32*1024)
	started := time.Now()
	for {
		if _, err := conn.Write(chunk); err != nil {
			break // 服务端关了连接 —— 正是期望
		}
		if time.Since(started) > 6*time.Second {
			t.Fatal("服务端没有在有限时间内关闭连接（排空无上限？）")
		}
	}
	if elapsed := time.Since(started); elapsed > 5*time.Second {
		t.Fatalf("关闭连接太慢：%s", elapsed)
	}
}

// crlf 是 HTTP 头的行分隔符（写成常量以免在测试里到处转义）。
const crlf = "\r\n"

// TestLongSessionTTLIsHonored 安卓端把会话设成长期有效时，签发与续期都必须照办。
//
// 背景（2026-10-13）：手机上的面板是 App 里 WebView 长期挂着的界面，每次打开都要
// 重新输密码是纯粹的折磨，而手机本身已有锁屏与生物识别。用户明确要求「手机端登录
// 有效期设为永久」。这里锁住的是**整条链**都取同一个值——只改签发不改续期，
// 会出现「登录时给长期令牌、一次续期后又缩回 30 天」，症状是「用几天又掉登录」。
func TestLongSessionTTLIsHonored(t *testing.T) {
	h := newHarness(t)
	h.server.opts.SessionTTL = LongSessionTTL
	doc := h.writeCredentials()

	// 1) 登录返回的 ttl 必须是长期值。
	recorder := h.postJSON(doc, "/api/auth/login", map[string]any{
		"username": "wbadmin", "password": "secret-password"})
	if recorder.Code != 200 {
		t.Fatalf("登录应 200，实际 %d：%s", recorder.Code, recorder.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if ttl, _ := jsonNumber(body["ttl"]); int64(ttl) != int64(LongSessionTTL/time.Second) {
		t.Fatalf("登录返回的 ttl = %v，期望 %d（长期）", body["ttl"], int64(LongSessionTTL/time.Second))
	}

	token := ""
	for _, cookie := range recorder.Result().Cookies() {
		if cookie.Name == CookieName && cookie.Value != "" {
			token = cookie.Value
		}
	}
	if token == "" {
		t.Fatal("登录必须下发会话 cookie")
	}
	// 2) 令牌本身在远超 30 天后仍然有效。
	payload := ReadSessionAt(doc, token, h.clock.Now().Add(SessionTTL+24*time.Hour))
	if payload == nil {
		t.Fatal("长期会话在 30 天之后必须仍然有效（否则安卓端还是会掉登录）")
	}
	// 3) 把时钟推到「30 天已过、但长期 TTL 未到」时请求一次，触发的续期不能缩回 30 天。
	h.clock.Advance(SessionTTL + 24*time.Hour)
	recorder = h.do(http.MethodGet, "/api/session", "", map[string]string{"Cookie": CookieName + "=" + token})
	if recorder.Code != 200 {
		t.Fatalf("长期会话在 30 天后应仍然 200，实际 %d", recorder.Code)
	}
	var sessionBody map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &sessionBody); err != nil {
		t.Fatal(err)
	}
	if ttl, _ := jsonNumber(sessionBody["ttl"]); int64(ttl) != int64(LongSessionTTL/time.Second) {
		t.Fatalf("/api/session 报的 ttl = %v，期望 %d（长期）", sessionBody["ttl"], int64(LongSessionTTL/time.Second))
	}
}

// TestContainerRunningAccepts503FromHealthz 零账号时 /healthz 返回 503，但那必须
// 判成「网关运行中」。
//
// 真机事故（2026-10-13）：用户刚装好安卓 App、还没加任何账号，面板首屏就显示
// 「网关容器未运行 / 服务当前不可用」。根因是读 /healthz 的路径把 HTTP 状态码当
// 成了前置闸——code >= 400 直接丢掉响应体，而判据恰恰要读响应体里的 service 字段。
// 一个账号都没有时 /healthz 正是 503 + {"service":"workbuddy2api","healthy":0}，
// 于是「网关活着、只是池里还没号」被显示成「网关没运行」。
//
// 注意：/healthz 的 503 是**设计如此**（池里没有可服务的号），不是错误。
func TestContainerRunningAccepts503FromHealthz(t *testing.T) {
	h := newHarness(t)
	h.transport.replies["GET /healthz"] = stubReply{Code: 503,
		Body: `{"commit":"x","healthy":0,"service":"workbuddy2api","total":0,"version":"v2.16.0"}`}
	payload := h.server.state.Build(context.Background(), false)
	if !payload.ContainerRun {
		t.Fatal("零账号时 /healthz 返回 503，仍须判为网关运行中（否则面板显示「网关容器未运行」）")
	}
}

// TestContainerRunningFalseWhenHealthzIsNotGateway /healthz 回了对象但不是本网关
// 时（同端口上跑着别的东西），必须判成未运行——否则面板会对着别人的服务操作。
func TestContainerRunningFalseWhenHealthzIsNotGateway(t *testing.T) {
	h := newHarness(t)
	h.transport.replies["GET /healthz"] = stubReply{Code: 200, Body: `{"service":"something-else"}`}
	payload := h.server.state.Build(context.Background(), false)
	if payload.ContainerRun {
		t.Fatal("service 字段不是 workbuddy2api 时必须判为未运行")
	}
}

// TestLoginPageSessionHintFollowsTTL 登录页那句「会话有效期 …」必须反映本次运行
// 的真实 TTL。
//
// 曾经它是页面里写死的「30 天」，而安卓端签发的是长期会话——登录页于是在对用户
// 说假话，而且是关于「多久要重登一次」这种用户会当成事实的提示。
func TestLoginPageSessionHintFollowsTTL(t *testing.T) {
	h := newHarness(t)
	// 默认（桌面/容器）：30 天。
	body := h.do(http.MethodGet, "/login", "", nil).Body.String()
	if !strings.Contains(body, "会话有效期 30 天") {
		t.Fatalf("默认应显示 30 天，实际：%s", firstLine(body))
	}
	// 页面上不得出现任何模板残留（注入是替换现成文案，不是填占位符）。
	if strings.Contains(body, "{{") {
		t.Fatal("登录页出现模板残留")
	}

	// 安卓：长期有效。
	h2 := newHarness(t)
	h2.server.opts.SessionTTL = LongSessionTTL
	body2 := h2.do(http.MethodGet, "/login", "", nil).Body.String()
	if !strings.Contains(body2, "会话长期有效") {
		t.Fatalf("安卓应显示长期有效，实际：%s", firstLine(body2))
	}
	if strings.Contains(body2, "30 天") {
		t.Fatal("安卓上不得再出现 30 天")
	}
}

func firstLine(s string) string {
	if idx := strings.Index(s, "hint"); idx >= 0 && idx+80 < len(s) {
		return s[idx : idx+80]
	}
	if len(s) > 120 {
		return s[:120]
	}
	return s
}

// TestContainerRunningAgreesWithState 两个判据必须给出同一个答案。
//
// 真机事故（2026-10-13）：安卓上面板状态显示「网关运行中」、/api/state 里
// containerRunning=true，但点「添加账号」报「容器未运行，先启动服务」——功能直接
// 不可用。根因是同一件事有两套实现：state.go 读 /healthz 的 service 字段，而
// actions.go 走 runner 的 inspect 分支（原生模式下依赖管理通道），两者在安卓上
// 结论相反。
//
// 这条测试同时锁住两个入口，避免它们再次分叉。
func TestContainerRunningAgreesWithState(t *testing.T) {
	for _, tc := range []struct {
		name    string
		code    int
		body    string
		running bool
	}{
		{"零账号时 503 仍算运行中", 503, `{"service":"workbuddy2api","healthy":0,"total":0}`, true},
		{"正常 200", 200, `{"service":"workbuddy2api","healthy":5,"total":5}`, true},
		{"不是本网关", 200, `{"service":"something-else"}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			h.transport.replies["GET /healthz"] = stubReply{Code: tc.code, Body: tc.body}
			payload := h.server.state.Build(context.Background(), false)
			viaState := payload.ContainerRun
			viaAction := h.server.containerRunning(context.Background())
			if viaState != tc.running {
				t.Fatalf("/api/state 判为 %v，期望 %v", viaState, tc.running)
			}
			if viaAction != viaState {
				t.Fatalf("两个判据不一致：添加账号等操作用 %v，状态显示用 %v", viaAction, viaState)
			}
		})
	}
}

// ── 安卓自动登录（App 用初始凭据换会话 cookie，不再让用户手抄） ──────────
//
// App 的自动登录走的是与浏览器登录**同一个**接口，但请求方是
// HttpURLConnection（MainActivity.performAutoLogin）。它与浏览器的差别只有两点：
//   - 不带 Origin 头（浏览器一定会带）；
//   - 自己读 Set-Cookie，而不是靠浏览器自动存。
//
// 这两点都必须成立，否则安卓端会静默回落到「显示凭据让用户手抄」。
func TestAutoLoginWorksWithoutOriginHeader(t *testing.T) {
	h := newHarness(t)
	h.writeCredentials()
	// 显式不带 Origin：originOK 对空 Origin 必须放行，否则 App 永远登不进去。
	recorder := h.do(http.MethodPost, "/api/auth/login",
		`{"username":"wbadmin","password":"secret-password"}`,
		map[string]string{"X-Admin-Request": "1"})
	if recorder.Code != 200 {
		t.Fatalf("不带 Origin 的登录应 200，实际 %d：%s", recorder.Code, recorder.Body.String())
	}
	// Set-Cookie 必须是 App 能直接解析的裸 `name=value` 前缀（MainActivity 截到
	// 第一个分号）。若把值写成带引号或有前缀的形式，App 那边解析会失败。
	headers := recorder.Header().Values("Set-Cookie")
	if len(headers) == 0 {
		t.Fatal("登录必须返回 Set-Cookie，否则 App 拿不到会话")
	}
	valid := ""
	for _, header := range headers {
		if !strings.HasPrefix(header, CookieName+"=") {
			continue
		}
		value := strings.SplitN(header, ";", 2)[0]
		if strings.TrimPrefix(value, CookieName+"=") != "" {
			valid = value
		}
	}
	if valid == "" {
		t.Fatalf("Set-Cookie 里没有可用的会话值：%v", headers)
	}
	// App 会原样把这串塞回 Cookie 头，必须能通过会话校验。
	session := h.do(http.MethodGet, "/api/session", "", map[string]string{"Cookie": valid})
	if session.Code != 200 {
		t.Fatalf("App 回填的 cookie 必须有效，实际 %d", session.Code)
	}
}
