// ═══ 更新日志 ═══
// 2026-10-13：初版——Go 版面板后端的 HTTP 层，路由与响应口径与 panel/app.py 逐项对齐。
//
// 为什么要有 Go 版：安卓 App 与 Windows 桌面版需要一个单二进制，装 CPython 代价太大。
// 前端（index.html / *.js / vendor/qrcode.js）完全复用，只换后端，所以这里的**每一个
// 状态码、每一句中文提示、每一个 JSON 键名**都必须和 Python 版一致——前端会直接
// 把它们显示给用户，也会按它们分支。
//
// ═══ 安全边界（三层，顺序不能变） ═══
//
//  1. **入口白名单**（可选 CIDR）：最先判，未配置则全放行、回环始终放行。
//  2. **同源 + 管理标记**：所有写操作必须同时满足「Origin 为空或与 Host 完全一致」
//     和 `X-Admin-Request: 1`。缺一即 403，且响应要**可解析**（前端靠 message 显示原因）。
//  3. **会话**：读操作要有效 cookie，写操作在闸门之后单独再验一次。
//
// 被拒绝的请求还要**排空请求体**再关连接：直接回 403 就关闭会让客户端在写大 body 时
// 撞上 RST，报出与真实原因无关的网络错误。排空有硬上限（64 KiB）与硬时限（0.1 秒），
// 不能让一个慢速攻击者用「永远发不完的 body」把处理协程钉住。
package panel

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	panelassets "workbuddy2api/panel"
)

// Options 是面板后端的全部外部依赖。全部显式传入（没有包级全局）：
// 面板要被塞进网关进程、桌面启动器、安卓 App 三种宿主里，全局状态会让它们互相污染，
// 也让测试无法并行。
type Options struct {
	// AdminDir 存 credentials.json / initial-password.txt。
	AdminDir string
	// AuthDir 是账号文件目录（网关的 auths）。
	AuthDir string
	// TrashDir 是删除账号的回收目录。
	TrashDir string
	// ConfigPath 是网关 config.json（任务开关、密钥路径、expiring_soon 都读它）。
	ConfigPath string
	// BaseDir 是网关运行目录（解析相对路径、映射 /app/ 前缀）。
	BaseDir string
	// RuntimeDir 是辅助程序所在目录（login / credit / fission_bin）。
	RuntimeDir string
	// GatewayURL 是网关 HTTP 地址，管理 socket 不可用时的回落路径。
	GatewayURL string
	// AdminPrefix 是面板的挂载前缀（默认 /admin），用于拼 Trae 公网回调地址。
	AdminPrefix string
	// Container 是网关容器名（docker 模式）。
	Container string
	// Runtime 为 "native" 时走原生执行通道（桌面 / 安卓）。
	Runtime string
	// TrustedProxies / AdminAllowCIDRs 是限流与入口白名单的配置（逗号分隔 CIDR）。
	TrustedProxies  string
	AdminAllowCIDRs string
	// Assets 提供 index.html / login.html / vendor/*。空则用内嵌的 panel.Files。
	Assets fs.FS
	// LogPath 是网关日志文件（原生模式下 /api/logs 读它）。
	LogPath string
	// AdminHandler 是网关的管理接口（同进程）。给了它就**不经 socket/TCP**，
	// 直接在进程内分发——Windows 上 net.Dial("unix", …) 不可用（实测 invalid
	// argument），走 socket 会让密钥/用量/热更新页全挂。空则回落到 socket 路径。
	AdminHandler http.Handler
	// HelperPaths 显式指定辅助程序路径（名字 → 绝对路径）。安卓端用它指向
	// APK 的 nativeLibraryDir；桌面/容器留空即按运行目录拼名。
	HelperPaths map[string]string
	// UpdateDir 是热更新下载目录（与网关自身用的是同一个）。面板据此显示
	// 「当前版本来自更新目录」这类信息。
	UpdateDir string
	// SessionTTL 是登录会话的有效期；0 表示用默认的 SessionTTL（30 天）。
	//
	// 安卓端会把它设成极长（见 cmd/server 的 Android 分支）：手机上的面板是 WebView
	// 里长期挂着的界面，用户每次打开 App 都要重新输一遍密码是纯粹的折磨，而手机
	// 本身已经有锁屏与生物识别这道门。桌面与容器仍用 30 天——它们面对的是浏览器，
	// 会话有效期是那里唯一的一道门。
	SessionTTL time.Duration
	// Version 是面板自身版本号；空则用 PanelVersion。
	Version string
	// Now 可注入时钟（测试用）。
	Now func() time.Time
}

// Server 是面板的 HTTP 处理器。
type Server struct {
	opts      Options
	assets    fs.FS
	store     *AccountStore
	creds     *CredentialStore
	gateway   *GatewayClient
	credits   *CreditCache
	runner    Runner
	throttle  *Throttle
	state     *StateBuilder
	version   string
	now       func() time.Time
	adminBase string

	// lock 串行化「改文件 + 重启」这一类复合操作（对应 Python 版的 document_lock）。
	//
	// 为什么需要它：account_toggle 要「改名 → 重启 → 读健康」，account_delete 要
	// 「移文件 → 重启」。两个并发请求会交错成「A 改名、B 改名、A 重启、B 重启」，
	// 于是 A 的重启把 B 的改动也带上了、B 又重启一次——两次重启之间的窗口里，面板
	// 报的状态与网关实际加载的账号对不上。持锁让每次操作是原子的。
	//
	// 它**不**保护只读路径（List/Summary/Find）：读操作拿到的可能是一瞬间的中间态，
	// 但那份中间态本身是一致的（rename 是原子的），而让列表也排队会拖慢首屏。
	lock sync.Mutex

	// credsLoadHook 只被测试用来模拟「读凭证 → 写会话」之间凭证被别人改掉。
	// 生产路径恒为 nil（走 creds.Load）。
	credsLoadHook func() (Credentials, error)
}

// loadCredentials 读凭证；测试可通过 credsLoadHook 注入并发改写。
func (s *Server) loadCredentials() (Credentials, error) {
	if s.credsLoadHook != nil {
		return s.credsLoadHook()
	}
	return s.creds.Load()
}

// New 构造面板处理器。
func New(opts Options) (*Server, error) {
	assets := opts.Assets
	if assets == nil {
		assets = defaultAssets()
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	version := strings.TrimSpace(opts.Version)
	if version == "" {
		version = PanelVersion
	}
	adminBase := strings.TrimRight(strings.TrimSpace(opts.AdminPrefix), "/")
	if adminBase == "" {
		adminBase = "/admin"
	}
	if !strings.HasPrefix(adminBase, "/") {
		adminBase = "/" + adminBase
	}

	socket, err := ResolveSocketPath(opts.ConfigPath, opts.BaseDir)
	if err != nil {
		// 配置读不到不该拦启动：面板仍要能显示「配置有问题」，只是管理通道暂时不可用。
		log.Printf("WARN: [panel] 读取网关配置失败，管理通道暂不可用: %v", err)
		socket = ""
	}
	management := &ManagementClient{Handler: opts.AdminHandler, SocketPath: socket, BaseDir: opts.BaseDir}
	runner := NewRunner(opts.Runtime, opts.Container, opts.RuntimeDir, opts.AuthDir, opts.AdminDir, opts.BaseDir, management, opts.HelperPaths)

	server := &Server{
		opts:      opts,
		assets:    assets,
		store:     &AccountStore{AuthDir: opts.AuthDir, TrashDir: opts.TrashDir},
		creds:     &CredentialStore{Path: credPath(opts.AdminDir)},
		gateway:   &GatewayClient{Management: management, BaseURL: normalizeBaseURL(opts.GatewayURL), APIKey: LoadAPIKey(opts.ConfigPath)},
		runner:    runner,
		throttle:  NewThrottle(trustedProxiesOr(opts.TrustedProxies), opts.AdminAllowCIDRs),
		version:   version,
		now:       opts.Now,
		adminBase: adminBase,
	}
	server.credits = NewCreditCache(runner, opts.ConfigPath, 0, 0)
	server.credits.now = opts.Now
	server.state = &StateBuilder{Store: server.store, Gateway: server.gateway, Credits: server.credits,
		Runner: runner, Container: opts.Container}

	// 启动时就把凭证准备好（Python 版的 main() 也这么做）。
	//
	// 为什么要在这里主动 Load 一次：它同时是「首次运行生成 credentials.json +
	// initial-password.txt」的触发点。不主动触发的话，那两个文件要等到第一次
	// 有人提交登录表单才出现——而 README 让用户去 panel-data/initial-password.txt
	// 取初始密码，那时文件还不存在，用户会以为包坏了。
	//
	// 凭证损坏时只记日志不返回错误：面板自己会把它显示成「登录凭证文件无效」，
	// 这比让整个面板起不来更有用（至少运维能看到原因）。
	if _, err := server.creds.Load(); err != nil {
		log.Printf("WARN: [panel] 登录凭证不可用：%v", err)
	}
	return server, nil
}

// trustedProxiesOr 在未配置可信代理时回落到默认网段（回环 + 私网 + ULA）。
//
// 空串不能解释成「没有任何可信代理」：文档推荐的部署是「nginx 在同一台机器或同一个
// Compose 网络里转发到面板」，那种形态下对端必然是回环或容器网段。若把空串当成
// 「谁都不信」，转发头一律忽略，限流会按 nginx 的地址分桶——所有人共用一个桶，
// 一个攻击者就能把整个面板的登录封掉。Python 版同样是「空 = 用默认值」。
func trustedProxiesOr(configured string) string {
	if strings.TrimSpace(configured) == "" {
		return TrustedProxiesRaw
	}
	return configured
}

// defaultAssets 返回内嵌的静态资源。
//
// 资源来自 panel 包（index.html / login.html / vendor/qrcode.js）——它们必须与
// panelruntime 解出来的那套是**同一份文件**，否则容器版与原生版会跑出两个前端。
func defaultAssets() fs.FS { return panelassets.FrontendFiles() }

// credPath 返回 credentials.json 路径。
func credPath(adminDir string) string { return joinPath(adminDir, "credentials.json") }

// ── 路由 ──────────────────────────────────────────────────────────────

// ServeHTTP 按 Python 版的 do_GET / do_POST 分支顺序分发。
//
// 顺序本身是语义的一部分：/trae/callback 必须在会话检查**之前**（浏览器从 Trae 授权页
// 跳回来时没有管理会话 cookie，走到后面会被重定向到登录页、把凭据参数丢掉）；
// /api/requests/facets 必须排在 /api/requests/{id} 之前（否则会被当成 id="facets" 转发）。
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !s.throttle.AdminClientAllowed(r) {
		writeJSON(w, http.StatusForbidden, map[string]any{"ok": false, "message": "当前来源不在管理入口白名单内"})
		return
	}
	path := s.stripPrefix(r.URL.Path)
	if path == "" {
		path = "/"
	}
	if path == "/__health" && s.opts.Runtime == "native" {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "version": s.version})
		return
	}
	if r.Method == http.MethodGet || r.Method == http.MethodHead {
		s.serveGet(w, r, path)
		return
	}
	if r.Method == http.MethodPost {
		s.servePost(w, r, path)
		return
	}
	writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
}

// stripPrefix 剥掉面板的挂载前缀，把路径归一成「面板内部路径」。
//
// 为什么必须剥：网关把面板挂在 `<prefix>/`（默认 /admin）下，路由分发后交给面板的
// 是**完整路径**（/admin/login）。而面板内部的分支是按内部路径写的（/login、/api/state）
// ——Python 版之所以没这个问题，是因为它躲在网关的反向代理后面，由代理先剥了前缀。
// 不剥的表现是「登录页反复重定向」：/admin/login 匹配不上 /login 分支，落到
// 「未登录 → 重定向到 login」，而重定向目标又被加上前缀，于是自己指回自己。
func (s *Server) stripPrefix(path string) string {
	base := s.adminBase
	if base == "" || base == "/" {
		return path
	}
	if path == base {
		return "/"
	}
	if strings.HasPrefix(path, base+"/") {
		return path[len(base):]
	}
	// 不在前缀下的路径原样返回：交给后面的 404 分支，而不是硬塞进面板内部路由。
	return path
}

func (s *Server) serveGet(w http.ResponseWriter, r *http.Request, path string) {
	ctx := r.Context()
	session := s.session(w, r, true)

	// Trae 授权回调：只收凭据并落盘，不做任何需要鉴权的操作。这条路径经 nginx 暴露
	// 在公网（Trae 只能回调到公网地址），因此**不返回任何凭据内容**。
	if path == "/trae/callback" {
		s.traeCallback(w, r)
		return
	}
	if path == "/login" {
		if session != nil {
			redirect(w, "./")
			return
		}
		body, err := readAsset(s.assets, "login.html")
		if err != nil {
			writeHTML(w, http.StatusInternalServerError, "登录页缺失: "+err.Error())
			return
		}
		writeHTML(w, http.StatusOK, body)
		return
	}
	if session == nil {
		if strings.HasPrefix(path, "/api/") {
			writeJSON(w, http.StatusUnauthorized, map[string]any{"ok": false, "error": "unauthorized",
				"message": "登录已失效，请重新登录"})
			return
		}
		redirect(w, "login")
		return
	}

	switch {
	case path == "/" || path == "/index.html":
		body, err := readAsset(s.assets, "index.html")
		if err != nil {
			writeHTML(w, http.StatusInternalServerError, "面板前端缺失: "+err.Error())
			return
		}
		writeHTML(w, http.StatusOK, body)
		return
	case strings.HasPrefix(path, "/vendor/"):
		// 放在会话检查之后（与 Python 版同序）：vendor 下是面板前端资源，只有
		// 登录后的 index.html 会引用它。未登录时落到上面的重定向分支即可。
		name := "vendor/" + baseName(path)
		body, err := readAsset(s.assets, name)
		if err != nil {
			writeJSON(w, http.StatusNotFound, map[string]any{"error": "not found"})
			return
		}
		writeBytes(w, http.StatusOK, []byte(body), "application/javascript; charset=utf-8")
		return
	case path == "/api/state":
		force := r.URL.Query().Get("refresh_credit") == "1"
		payload := s.state.Build(ctx, force)
		writeJSON(w, http.StatusOK, payload)
		return
	case path == "/api/session":
		doc, err := s.loadCredentials()
		if err != nil {
			writeJSON(w, http.StatusOK, map[string]any{"ok": false, "message": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":        true,
			"username":  doc.Username(),
			"expiresAt": session.Expires,
			// 报**本次运行实际用的** TTL，而不是那个 30 天的常量：安卓端把它设成
			// 长期（见 Options.SessionTTL），报常量会让界面显示一个与事实不符的
			// 「30 天后过期」。
			"ttl":       int64(s.sessionTTL() / time.Second),
			"inherited": jsonBool(doc["inherited"], false),
		})
		return
	case path == "/api/keys":
		s.keysWithUsage(w, r)
		return
	case path == "/api/key-limits" || path == "/api/requests" || strings.HasPrefix(path, "/api/requests/"):
		s.opsGet(w, r, path)
		return
	case path == "/api/usage":
		// 用量账本只经本机管理通道读取：面板能看到全量，普通调用密钥看不到。
		s.gatewayAdmin(w, r, http.MethodGet, "/usage", nil)
		return
	case path == "/api/update":
		s.gatewayAdmin(w, r, http.MethodGet, "/update", nil)
		return
	case path == "/api/features/reasoning-loop":
		s.gatewayAdmin(w, r, http.MethodGet, "/features/reasoning-loop", nil)
		return
	case path == "/api/models":
		writeJSON(w, http.StatusOK, modelsPayload(s.gateway.Get(ctx, "/v1/models")))
		return
	case path == "/api/models/refresh":
		s.modelsRefresh(w, r)
		return
	case path == "/api/tasks":
		if payload := s.gateway.Get(ctx, "/tasks"); payload != nil {
			writeJSON(w, http.StatusOK, payload)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"available": false, "tasks": []any{},
			"error": "连不上网关或网关未接入排程"})
		return
	case path == "/api/task/log":
		s.taskLog(w, r)
		return
	case path == "/api/logs":
		s.logs(w, r)
		return
	}
	writeJSON(w, http.StatusNotFound, map[string]any{"error": "not found"})
}

func (s *Server) servePost(w http.ResponseWriter, r *http.Request, path string) {
	// 密钥管理有自己的一整套闸门（它要在会话之前拿到 body 才能判 8192 上限），
	// 与 Python 版一致地单独分流。
	switch path {
	case "/api/keys", "/api/keys/update", "/api/keys/delete", "/api/keys/copy":
		s.keysPost(w, r, path)
		return
	}
	if !s.originOK(r) || r.Header.Get("X-Admin-Request") != "1" {
		s.rejectUnreadBody(w, r, http.StatusForbidden,
			map[string]any{"ok": false, "message": "请求来源无效，请从管理页面重新操作"})
		return
	}
	limit := int64(65536)
	if strings.HasPrefix(path, "/api/update/") {
		limit = 8192
	}
	body, ok := s.readBody(w, r, limit)
	if !ok {
		return
	}
	if message := validateActionBody(path, body); message != "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "message": message})
		return
	}
	if path == "/api/auth/login" {
		s.authLogin(w, r, body)
		return
	}
	if path == "/api/auth/logout" {
		s.authLogout(w, r)
		return
	}

	session := s.session(w, r, true)
	if session == nil {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"ok": false, "error": "unauthorized",
			"message": "登录已失效，请重新登录"})
		return
	}

	switch path {
	case "/api/auth/password":
		s.authPassword(w, r, body, session)
	case "/api/login/start":
		writeJSON(w, http.StatusOK, s.loginStart(r.Context(), body))
	case "/api/login/poll":
		writeJSON(w, http.StatusOK, s.loginPoll(r.Context(), body))
	case "/api/trae/login/start":
		writeJSON(w, http.StatusOK, s.traeLoginStart())
	case "/api/trae/login/finish":
		writeJSON(w, http.StatusOK, s.traeLoginFinish(r.Context(), body))
	case "/api/account/toggle":
		writeJSON(w, http.StatusOK, s.accountToggle(r.Context(), body))
	case "/api/account/revive":
		writeJSON(w, http.StatusOK, s.accountRevive(r.Context(), body))
	case "/api/account/fission":
		writeJSON(w, http.StatusOK, s.accountFission(r.Context(), body))
	case "/api/account/delete":
		writeJSON(w, http.StatusOK, s.accountDelete(r.Context(), body))
	case "/api/task/run":
		writeJSON(w, http.StatusOK, s.taskRun(r.Context(), body))
	case "/api/task/toggle":
		writeJSON(w, http.StatusOK, s.taskToggle(r.Context(), body))
	case "/api/service/restart":
		writeJSON(w, http.StatusOK, s.serviceRestart(r.Context()))
	case "/api/features/reasoning-loop":
		writeJSON(w, http.StatusOK, s.reasoningLoopToggle(r.Context(), body))
	case "/api/update/check", "/api/update/apply":
		s.updatePost(w, r, path, body)
	case "/api/models/refresh":
		s.modelsRefresh(w, r)
	case "/api/credit":
		writeJSON(w, http.StatusOK, map[string]any{"credit": s.credits.Get(r.Context(), true)})
	default:
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "not found"})
	}
}

// ── 请求体读取与校验 ──────────────────────────────────────────────────

// readBody 读并校验 JSON 请求体；失败时已写出响应并返回 ok=false。
//
// 每一道校验的**顺序**都是语义：Transfer-Encoding 先于 Content-Type，Content-Type 先于
// 长度。长度必须按 ASCII 数字严格校验——Python 侧吃过 str.isdigit() 对 latin-1 数字
// 字节返回 True、int() 却抛 ValueError 的亏，那种异常会被兜底 except 变成 500 并把
// 内部细节回显给客户端。
func (s *Server) readBody(w http.ResponseWriter, r *http.Request, limit int64) (map[string]any, bool) {
	fail := func(code int, message string) (map[string]any, bool) {
		writeJSON(w, code, map[string]any{"ok": false, "message": message})
		return nil, false
	}
	if len(r.TransferEncoding) > 0 {
		return fail(http.StatusBadRequest, "不支持此请求传输方式")
	}
	contentType := r.Header.Get("Content-Type")
	if semi := strings.IndexByte(contentType, ';'); semi >= 0 {
		contentType = contentType[:semi]
	}
	if strings.ToLower(strings.TrimSpace(contentType)) != "application/json" {
		return fail(http.StatusUnsupportedMediaType, "请使用 JSON 格式提交")
	}
	if !contentLengthOK(r.Header.Values("Content-Length")) {
		return fail(http.StatusBadRequest, "请求长度无效")
	}
	if r.ContentLength <= 0 {
		return fail(http.StatusBadRequest, "请求体不能为空")
	}
	if r.ContentLength > limit {
		return fail(http.StatusRequestEntityTooLarge, "请求体超过大小限制")
	}
	controller := http.NewResponseController(w)
	_ = controller.SetReadDeadline(s.now().Add(10 * time.Second))
	defer func() { _ = controller.SetReadDeadline(time.Time{}) }()
	raw, err := io.ReadAll(io.LimitReader(r.Body, r.ContentLength+1))
	if err != nil || int64(len(raw)) != r.ContentLength {
		return fail(http.StatusBadRequest, "请求体不是完整的 JSON")
	}
	// 解到 any 再判类型，而不是直接解进 map：`null` 解进 map 是 nil、`[]`/`123`/`"x"`
	// 解进 map 直接报错，三种情况会被折成同一句话。而前端要靠这两句话区分
	// 「body 不是 JSON」与「body 是 JSON 但不是对象」。
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var decoded any
	if err := decoder.Decode(&decoded); err != nil {
		return fail(http.StatusBadRequest, "请求体不是完整的 JSON")
	}
	body, ok := decoded.(map[string]any)
	if !ok {
		return fail(http.StatusBadRequest, "请求体必须是 JSON 对象")
	}
	return body, true
}

// contentLengthOK 按 `^[0-9]{1,10}$` 校验 Content-Length（且必须只有一个）。
func contentLengthOK(values []string) bool {
	if len(values) != 1 {
		return false
	}
	return asciiDigitsRe.MatchString(strings.TrimSpace(values[0]))
}

var asciiDigitsRe = regexp.MustCompile(`^[0-9]{1,10}$`)

// rejectUnreadBody 回一个「不再读 body」的错误响应，然后限时限量地把迟到的小请求体排空。
//
// 为什么不能直接关闭连接：客户端正在写 body 时收到 RST，会报出与真实原因无关的网络
// 错误（前端只能显示「请求失败」，运维无从判断是来源校验还是密码错）。排空之后连接
// 可以干净关闭，客户端能读到 403 的 JSON。
//
// 为什么必须限时限量：不限就是一条「用永远发不完的 body 钉住处理协程」的攻击面。
// 0.1 秒 + 64 KiB 是「够把普通误配的请求体吃掉、不够做任何事」的量级。
func (s *Server) rejectUnreadBody(w http.ResponseWriter, r *http.Request, code int, payload map[string]any) {
	w.Header().Set("Connection", "close")
	writeJSON(w, code, payload)
	controller := http.NewResponseController(w)
	// **必须先 flush 再排空**。Go 的响应体先写进 bufio.Writer，只有 handler 返回
	// 或显式 Flush 才会发出去；而排空要等客户端把请求体发完。客户端在等响应、
	// 服务端在等请求体 —— 两边互等，直到连接超时。Python 版没有这个问题
	// （它的 wfile 直接写 socket），但这里不 flush 就会退化成一个死锁：
	// 任何「先发头、拿到响应再发体」的客户端（含 Expect: 100-continue）都会卡住。
	if err := controller.Flush(); err != nil {
		return
	}
	if len(r.TransferEncoding) > 0 || len(r.Header.Values("Content-Length")) != 1 {
		return
	}
	value := strings.TrimSpace(r.Header.Get("Content-Length"))
	if !asciiDigitsRe.MatchString(value) {
		return
	}
	var total int64
	for _, ch := range value {
		total = total*10 + int64(ch-'0')
	}
	if total > 65536 {
		total = 65536
	}
	if total <= 0 {
		return
	}
	deadline := s.now().Add(100 * time.Millisecond)
	_ = controller.SetReadDeadline(deadline)
	defer func() { _ = controller.SetReadDeadline(time.Time{}) }()
	_, _ = io.Copy(io.Discard, io.LimitReader(r.Body, total))
}

// actionShapes 是每个写接口允许的字段集合；不在表里的接口不做字段限制。
var actionShapes = map[string][]string{
	"/api/auth/login":              {"username", "password"},
	"/api/auth/logout":             {},
	"/api/auth/password":           {"current", "username", "password", "confirm"},
	"/api/login/start":             {"realm"},
	"/api/login/poll":              {"realm", "login_id"},
	"/api/trae/login/start":        {},
	"/api/trae/login/finish":       {"callback"},
	"/api/account/toggle":          {"uid", "disabled"},
	"/api/account/delete":          {"uid"},
	"/api/account/revive":          {"uid"},
	"/api/account/fission":         {"uid"},
	"/api/task/run":                {"key"},
	"/api/task/toggle":             {"key", "enabled"},
	"/api/service/restart":         {},
	"/api/credit":                  {},
	"/api/models/refresh":          {},
	"/api/features/reasoning-loop": {"stop_only"},
}

// stringFields 是必须为字符串的字段。
var stringFields = map[string]bool{
	"username": true, "password": true, "current": true, "confirm": true,
	"realm": true, "uid": true, "key": true, "callback": true,
}

var loginIDRe = regexp.MustCompile(`^[a-f0-9]{32}$`)

// validateActionBody 校验写请求的字段集合与取值；返回错误文案（空串表示通过）。
//
// 逐条对应 panel/app.py 的 _validate_action_body。这里的严格程度是刻意的：面板是
// 能改密钥、能重启服务、能删账号的界面，任何「多传一个字段也照做」的宽松都是在给
// 越权留缝（比如借 /api/task/run 的 body 里塞一个 uid 去打别的接口）。
func validateActionBody(path string, body map[string]any) string {
	if allowed, known := actionShapes[path]; known {
		permitted := make(map[string]bool, len(allowed))
		for _, name := range allowed {
			permitted[name] = true
		}
		for field := range body {
			if !permitted[field] {
				return "包含不支持的字段"
			}
		}
	}
	for field, value := range body {
		if stringFields[field] {
			if _, ok := value.(string); !ok {
				return "字段格式不正确：" + field
			}
		}
	}
	if raw, present := body["login_id"]; present {
		text, ok := raw.(string)
		if !ok || !loginIDRe.MatchString(text) {
			return "登录流程标识无效，请重新生成链接"
		}
	}
	for _, route := range []struct{ path, field string }{
		{"/api/account/toggle", "disabled"}, {"/api/task/toggle", "enabled"},
	} {
		if path == route.path {
			if _, ok := body[route.field].(bool); !ok {
				return "开关值必须明确指定为 true 或 false"
			}
		}
	}
	if path == "/api/features/reasoning-loop" {
		if _, ok := body["stop_only"].(bool); !ok {
			return "开关值必须明确指定为 true 或 false"
		}
	}
	if strings.HasPrefix(path, "/api/account/") {
		uid, _ := body["uid"].(string)
		if !uidRe.MatchString(uid) {
			return "账号标识不正确"
		}
	}
	if path == "/api/task/run" || path == "/api/task/toggle" {
		key, _ := body["key"].(string)
		if !KnownTask(key) {
			return "任务标识不正确"
		}
	}
	return ""
}

// originOK 同源校验：没带 Origin（同源表单/脚本）或与 Host 完全一致才算合法。
//
// 有 Origin 就必须**完全等于 Host**，不能只比后缀：`https://evil.example` 与
// `https://panel.example` 的后缀比较会误放行。也不能带 userinfo——`http://a@host/`
// 的 netloc 与 Host 不同，但某些解析器会把 a@ 当成用户名而放过。
func (s *Server) originOK(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	parsed, err := parseOrigin(origin)
	if err != nil {
		return false
	}
	if parsed.User != nil {
		return false
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return false
	}
	return strings.EqualFold(parsed.Host, r.Host)
}

// ── 响应写出 ──────────────────────────────────────────────────────────

// securityHeaders 是每个响应都带的固定头（与 Python 版 _send 一致）。
//
// 面板能改密钥、能看账号凭据，所以不能出现在 iframe 里（X-Frame-Options + CSP
// frame-ancestors），不能被嗅探成别的类型（nosniff），也不能被缓存（no-store）——
// 一个被缓存的 /api/state 会让运维看到过期的账号状态还以为是实时的。
func securityHeaders(w http.ResponseWriter) {
	header := w.Header()
	header.Set("Cache-Control", "no-store")
	header.Set("X-Content-Type-Options", "nosniff")
	header.Set("Referrer-Policy", "same-origin")
	header.Set("X-Frame-Options", "SAMEORIGIN")
	header.Set("Content-Security-Policy", "frame-ancestors 'self'; base-uri 'self'; form-action 'self'")
}

func writeBytes(w http.ResponseWriter, code int, body []byte, contentType string) {
	securityHeaders(w)
	w.Header().Set("Content-Type", contentType)
	w.WriteHeader(code)
	_, _ = w.Write(body)
}

func writeJSON(w http.ResponseWriter, code int, payload any) {
	// 不能用 json.Marshal：它默认把 < > & 转义成 < 之类，而 Python 版用的是
	// json.dumps(ensure_ascii=False)。前端会把某些字段原样塞进 DOM，两边形态必须一致。
	var buf strings.Builder
	encoder := json.NewEncoder(&buf)
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", "")
	if err := encoder.Encode(payload); err != nil {
		writeBytes(w, http.StatusInternalServerError, []byte(`{"ok":false,"message":"响应编码失败"}`),
			"application/json; charset=utf-8")
		return
	}
	writeBytes(w, code, []byte(strings.TrimRight(buf.String(), "\n")), "application/json; charset=utf-8")
}

func writeHTML(w http.ResponseWriter, code int, text string) {
	writeBytes(w, code, []byte(text), "text/html; charset=utf-8")
}

func redirect(w http.ResponseWriter, target string) {
	securityHeaders(w)
	w.Header().Set("Location", target)
	w.WriteHeader(http.StatusFound)
}

// readAsset 从资源文件系统读一个文件；不存在返回错误。
func readAsset(assets fs.FS, name string) (string, error) {
	raw, err := fs.ReadFile(assets, name)
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

func baseName(path string) string {
	if index := strings.LastIndexAny(path, `/\`); index >= 0 {
		return path[index+1:]
	}
	return path
}

// joinPath 拼接路径（避免在非 Windows 上也引 filepath 的平台语义）。
func joinPath(dir, name string) string {
	if dir == "" {
		return name
	}
	separator := string(os.PathSeparator)
	if strings.HasSuffix(dir, "/") || strings.HasSuffix(dir, separator) {
		return dir + name
	}
	return dir + separator + name
}

// parseOrigin 解析 Origin 头。
func parseOrigin(raw string) (*urlParts, error) {
	return splitURL(raw)
}

// urlParts 是 urlsplit 的最小实现（面板只需要 scheme/host/user）。
type urlParts struct {
	Scheme string
	Host   string
	User   *string
}

func splitURL(raw string) (*urlParts, error) {
	schemeEnd := strings.Index(raw, "://")
	if schemeEnd <= 0 {
		return nil, errors.New("no scheme")
	}
	parts := &urlParts{Scheme: strings.ToLower(raw[:schemeEnd])}
	rest := raw[schemeEnd+3:]
	if end := strings.IndexAny(rest, "/?#"); end >= 0 {
		rest = rest[:end]
	}
	if at := strings.LastIndex(rest, "@"); at >= 0 {
		user := rest[:at]
		parts.User = &user
		rest = rest[at+1:]
	}
	parts.Host = rest
	if parts.Host == "" {
		return nil, errors.New("no host")
	}
	return parts, nil
}

// contextWithTimeout 给一次外部调用加超时（面板的每个按钮都不该无限等）。
func contextWithTimeout(parent context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	if parent == nil {
		parent = context.Background()
	}
	return context.WithTimeout(parent, timeout)
}

// formatMessage 是 fmt.Sprintf 的可读包装（面板文案里有大量「已禁用 abcd1234，...」）。
func formatMessage(format string, args ...any) string { return fmt.Sprintf(format, args...) }
