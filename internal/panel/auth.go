// ═══ 更新日志 ═══
// 2026-10-13：初版——登录、改密、退出与会话 cookie（与 panel/app.py 同口径）。
//
// ═══ 为什么这里的顺序和判断都是「故意的」 ═══
//
//  1. **先占尝试预算、再验口令**。并发登录时，如果先验证再记账，两个请求会同时通过
//     验证、只记一次失败——限流被并发绕开。Python 版的顺序是先 login_blocked 判额度、
//     未封禁就立刻 login_failed 记账，然后才读凭证比对。
//
//  2. **验证之后再比一次凭证**。读凭证到写会话之间有一段窗口，期间管理员可能改了密码
//     （或另一个请求改了）。所以验证通过后要重新加载并与手上这份逐字段比对；不一致就
//     401，绝不签发一个基于旧口令的会话，也绝不覆盖更新的凭证。
//
//  3. **换密即换 sessionKey**。所有旧会话（包括当前这条）一并失效。这是「改了密码，
//     别的设备应该掉线」这条承诺的唯一实现方式——只改 password 哈希而留着 sessionKey，
//     旧 cookie 还能用到过期。
//
//  4. **退出要落盘撤销**。只把 nonce 记在内存里，面板一重启「已登出的会话」就复活了。
package panel

import (
	"crypto/hmac"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"net/http"
	"regexp"
	"strings"
	"time"
)

// CookieName 是管理会话 cookie 名。
const CookieName = "wb2a_admin"

// maxRevokedSessions 撤销表的容量上限。超过后换 sessionKey 并把表清空——
// 换密钥让所有旧令牌一次性失效，比继续无限增长更省事也更安全。
const maxRevokedSessions = 4096

// renewThreshold 是**默认** TTL 下的滑动续期触发线：剩余不足 TTL 的 1/3 时续期。
// 令牌本身不换新（nonce 沿用），只延长过期时间——换 nonce 会让「同一会话的旧副本」
// 重新变成有效令牌（见 authLogout 的注释）。
//
// 运行时判据用的是 sessionTTL()/3（TTL 可被 Options 覆盖，见 sessionTTL），
// 这个常量只给测试当默认值的参照点。
const renewThreshold = SessionTTL / 3

// sessionHint 登录页上那句「会话有效期 …」的文案。
//
// 超过一年就说「长期有效」而不是报一个「36500 天」之类的数字：那个数字既难读，
// 也会让用户以为还要惦记着它。安卓端设的就是这种长期会话（见 LongSessionTTL）。
func sessionHint(ttl time.Duration) string {
	if ttl >= 365*24*time.Hour {
		return "会话长期有效"
	}
	return formatMessage("会话有效期 %d 天", int(ttl/(24*time.Hour)))
}

// loginHintLiteral 是 login.html 里那句写死的默认文案，也是注入时的锚点。
//
// 锚在**现成文案**上而不是留一个 {{占位符}}，是因为同一份 login.html 也被 Python
// 引擎逐字读出来发给浏览器（panel/app.py 的 LOGIN_PATH）。留占位符会让容器部署的
// 登录页上直接显示 "{{SESSION_HINT}}"——那是把一个「安卓端文案不准」的小问题，
// 换成「容器端界面出现模板变量」的更大问题。
const loginHintLiteral = "会话有效期 30 天"

// replaceSessionHint 把登录页里的默认会话文案换成本次运行的真实值。
func replaceSessionHint(page string, ttl time.Duration) string {
	hint := sessionHint(ttl)
	if hint == loginHintLiteral {
		return page // 默认值：页面里本来就是它，不必替换
	}
	return strings.Replace(page, loginHintLiteral, hint, 1)
}

// sessionTTL 本次运行的会话有效期：Options.SessionTTL 优先，未设则用默认 30 天。
//
// 走方法而不是直接用常量，是为了让「同一个部署里所有签发/续期/判过期的地方」
// 都取到同一个值——散落的常量引用正是「登录时给了长期令牌、续期时又缩回 30 天」
// 这类不一致的来源。
func (s *Server) sessionTTL() time.Duration {
	if s.opts.SessionTTL > 0 {
		return s.opts.SessionTTL
	}
	return SessionTTL
}

var (
	usernameRe = regexp.MustCompile(`^[A-Za-z0-9_.-]{3,32}$`)
)

// session 从 Cookie 取会话；renew=true 时顺带做滑动续期。
//
// 续期是把 Set-Cookie 追加到当前响应上，而不是另起一个响应——面板的每个请求都可能
// 触发续期，多一次往返没有必要。
func (s *Server) session(w http.ResponseWriter, r *http.Request, renew bool) *SessionPayload {
	raw := r.Header.Get("Cookie")
	if raw == "" {
		return nil
	}
	token := cookieValue(raw, CookieName)
	if token == "" {
		return nil
	}
	creds, err := s.loadCredentials()
	if err != nil {
		return nil
	}
	payload := ReadSessionAt(creds, token, s.now())
	if payload == nil {
		return nil
	}
	ttl := s.sessionTTL()
	remaining := time.Duration(payload.Expires-s.now().Unix()) * time.Second
	if renew && remaining < ttl/3 {
		renewed, err := IssueSessionAt(creds, payload.Nonce, ttl, s.now())
		if err == nil {
			s.setSessionCookie(w, r, renewed, ttl)
		}
	}
	return payload
}

// cookieValue 从 Cookie 头里取一个 cookie 的值（不用 net/http 的 ParseCookie：
// 它对畸形输入的宽容度与 Python 的 SimpleCookie 不一致，而畸形 Cookie 正是攻击面）。
func cookieValue(raw, name string) string {
	for _, part := range strings.Split(raw, ";") {
		part = strings.TrimSpace(part)
		key, value, found := strings.Cut(part, "=")
		if !found || strings.TrimSpace(key) != name {
			continue
		}
		return strings.TrimSpace(value)
	}
	return ""
}

// setSessionCookie 写会话 cookie（含清除旧路径副本的那一条）。
//
// 为什么同时发两条：历史上 cookie 曾在 Path=/admin/ 下签出过，浏览器里可能还留着
// 那一份。只写 Path=/ 的新 cookie 会让旧的那份继续存在，登出时更会「清了 / 却没清
// /admin/」，表现为「点了退出还是登录态」。
func (s *Server) setSessionCookie(w http.ResponseWriter, r *http.Request, token string, ttl time.Duration) {
	value := CookieName + "=" + token + "; Path=/; HttpOnly; SameSite=Lax; Max-Age=" +
		itoa(int64(ttl/time.Second))
	// Secure 只在确认是 HTTPS 时加：面板常挂在 http://127.0.0.1 后面，
	// 无条件加会让本地部署根本收不到 cookie。
	if strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https") {
		value += "; Secure"
	}
	w.Header().Add("Set-Cookie", value)
	w.Header().Add("Set-Cookie", CookieName+"=; Path=/admin/; HttpOnly; SameSite=Lax; Max-Age=0")
}

// clearSessionCookies 清掉两个路径下的会话 cookie。
func clearSessionCookies() []string {
	return []string{
		CookieName + "=; Path=/; HttpOnly; SameSite=Lax; Max-Age=0",
		CookieName + "=; Path=/admin/; HttpOnly; SameSite=Lax; Max-Age=0",
	}
}

// authLogin 处理 /api/auth/login。
func (s *Server) authLogin(w http.ResponseWriter, r *http.Request, body map[string]any) {
	// 全站节流最先判：轮换 IP 的分布式爆破靠它挡，单 IP 分桶挡不住。
	if retryAfter := s.throttle.GlobalRetryAfter(); retryAfter > 0 {
		// Retry-After 必须是秒数字符串；用整数值而不是 Duration 格式，
		// 客户端（含浏览器）都按秒解析。
		w.Header().Set("Retry-After", itoa(int64(retryAfter)))
		writeJSON(w, http.StatusTooManyRequests, map[string]any{
			"ok":      false,
			"message": formatMessage("登录尝试过于频繁，请 %d 秒后再试", retryAfter),
		})
		return
	}
	ip := s.throttle.ClientIP(r)
	blocked, hits := s.throttle.BlockedAndRecord(ip)
	if blocked {
		writeJSON(w, http.StatusTooManyRequests, map[string]any{
			"ok":      false,
			"message": formatMessage("失败次数过多，请 %d 分钟后再试", int(LoginWindow/time.Minute)),
		})
		return
	}

	doc, err := s.loadCredentials()
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "message": err.Error()})
		return
	}
	name := strings.TrimSpace(jsonString(body["username"]))
	password := jsonString(body["password"])
	if name == "" || password == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "message": "请输入用户名和密码"})
		return
	}

	// 定长比较用户名，避免时序侧信道泄露「用户名对不对」。
	nameOK := hmac.Equal([]byte(name), []byte(doc.Username()))
	if !nameOK || !VerifyPassword(password, doc.Password()) {
		left := LoginMaxFails - (hits + 1)
		if left < 0 {
			left = 0
		}
		message := "用户名或密码错误"
		if left > 0 {
			message = formatMessage("用户名或密码错误，还可尝试 %d 次", left)
		}
		writeJSON(w, http.StatusUnauthorized, map[string]any{"ok": false, "message": message})
		return
	}

	// 验证通过后重新加载并比对：中间可能有人改了密码。
	fresh, err := s.loadCredentials()
	if err != nil || !sameCredentials(doc, fresh) {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"ok": false,
			"message": "登录凭证已更改，请使用新密码重新登录"})
		return
	}
	s.throttle.OK(ip)
	ttl := s.sessionTTL()
	token, err := IssueSessionAt(fresh, "", ttl, s.now())
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "message": err.Error()})
		return
	}
	payload := ReadSessionAt(fresh, token, s.now())
	expires := s.now().Add(ttl).Unix()
	if payload != nil {
		expires = payload.Expires
	}
	s.setSessionCookie(w, r, token, ttl)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "username": fresh.Username(), "expiresAt": expires,
		"ttl": int64(ttl / time.Second),
	})
}

// authLogout 处理 /api/auth/logout。
//
// 关键点：撤销记录必须**落盘**，且要把「同一 nonce 的续期副本」也一起撤销。
// 续期沿用同一个 nonce（不换令牌），所以按 nonce 撤销天然覆盖了续期前后的两份令牌——
// 这也是为什么续期不换 nonce：换了就会留下一个「登出后仍然有效」的旧副本。
func (s *Server) authLogout(w http.ResponseWriter, r *http.Request) {
	payload := s.session(w, r, false)
	// 先清掉可能已写入的续期 Set-Cookie，再写清除 cookie。
	w.Header().Del("Set-Cookie")
	for _, cookie := range clearSessionCookies() {
		w.Header().Add("Set-Cookie", cookie)
	}
	if payload != nil && payload.Nonce != "" {
		if doc, err := s.loadCredentials(); err == nil {
			now := s.now()
			revoked := map[string]any{}
			if existing, ok := doc["revokedSessions"].(map[string]any); ok {
				for nonce, expiry := range existing {
					if value, ok := jsonNumber(expiry); ok && int64(value) > now.Unix() {
						revoked[nonce] = expiry
					}
				}
			}
			// 撤销记录的存活期取「令牌过期」与「现在 + TTL」的较大者：
			// 取小了会出现「撤销记录先过期、令牌后过期」的窗口，那段时间旧令牌复活。
			expiry := payload.Expires
			if fallback := now.Add(s.sessionTTL()).Unix(); fallback > expiry {
				expiry = fallback
			}
			revoked[payload.Nonce] = expiry
			if len(revoked) > maxRevokedSessions {
				// 撤销表太大：换 sessionKey 让所有旧令牌一次性失效，比继续增长更省事。
				key, err := randomHex(32)
				if err == nil {
					doc["sessionKey"] = key
					revoked = map[string]any{}
				}
			}
			doc["revokedSessions"] = revoked
			if err := s.creds.Save(doc); err != nil {
				writeJSON(w, http.StatusOK, map[string]any{"ok": false,
					"message": "退出登录未能写入撤销记录：" + err.Error()})
				return
			}
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "message": "已退出登录"})
}

// authPassword 处理 /api/auth/password。
func (s *Server) authPassword(w http.ResponseWriter, r *http.Request, body map[string]any, session *SessionPayload) {
	doc, err := s.loadCredentials()
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "message": err.Error()})
		return
	}
	current := jsonString(body["current"])
	newUser := strings.TrimSpace(jsonString(body["username"]))
	newPassword := jsonString(body["password"])
	confirm := jsonString(body["confirm"])

	// 逐条判据与提示文案必须与 Python 版一致：前端把它们直接显示给用户，
	// 也是改密表单的字段级校验依据。
	if !VerifyPassword(current, doc.Password()) {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "message": "当前密码不正确"})
		return
	}
	if len([]rune(newPassword)) < 8 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "message": "新密码至少 8 位"})
		return
	}
	if newPassword != confirm {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "message": "两次输入的新密码不一致"})
		return
	}
	if newPassword == current {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "message": "新密码不能与当前密码相同"})
		return
	}
	if !usernameRe.MatchString(newUser) {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false,
			"message": "用户名需为 3-32 位字母、数字、下划线、点或连字符"})
		return
	}

	replacement, err := HashPassword(newPassword)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false,
			"message": "写入失败：" + err.Error()})
		return
	}
	// 再比一次：改密请求可能和另一个改密/登录并发，谁先写谁赢，后到的必须重来。
	fresh, err := s.loadCredentials()
	if err != nil || !sameCredentials(doc, fresh) {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"ok": false,
			"message": "登录凭证已更改，请重新登录后修改"})
		return
	}
	history := []any{}
	if existing, ok := fresh["history"].([]any); ok {
		history = existing
	}
	history = append(history, map[string]any{"password": fresh.Password(), "at": s.now().Unix()})
	if len(history) > 5 {
		history = history[len(history)-5:]
	}
	key, err := randomHex(32)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false,
			"message": "写入失败：" + err.Error()})
		return
	}
	// 换密即换 sessionKey：所有旧会话（包括当前这条）一并失效，强制重登。
	next := Credentials{
		"version": 1, "username": newUser, "password": replacement, "sessionKey": key,
		"updatedAt": s.now().Unix(), "history": history, "inherited": false,
	}
	if err := s.creds.Save(next); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false,
			"message": "写入失败：" + err.Error()})
		return
	}
	ttl := s.sessionTTL()
	token, err := IssueSessionAt(next, "", ttl, s.now())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "message": err.Error()})
		return
	}
	s.setSessionCookie(w, r, token, ttl)
	payload := ReadSessionAt(next, token, s.now())
	expires := s.now().Add(ttl).Unix()
	if payload != nil {
		expires = payload.Expires
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "message": "账号信息已更新，其他设备上的登录已失效",
		"username": next.Username(), "expiresAt": expires,
	})
}

// sameCredentials 逐字段比较两份凭证是否来自同一次写入。
func sameCredentials(first, second Credentials) bool {
	for _, field := range []string{"username", "password", "sessionKey"} {
		if jsonString(first[field]) != jsonString(second[field]) {
			return false
		}
	}
	return true
}

// randomHex 生成 n 字节随机数的 hex 串（对应 Python 的 secrets.token_hex(n)）。
func randomHex(n int) (string, error) {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

// randomURLSafe 对应 Python 的 secrets.token_urlsafe(n)：base64url，无 padding。
func randomURLSafe(n int) (string, error) {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// randomDigits 生成 n 位十进制数字串（Trae 授权要 19 位 device_id）。
func randomDigits(n int) (string, error) {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	var out strings.Builder
	for _, b := range buf {
		out.WriteByte('0' + b%10)
	}
	return out.String(), nil
}

// itoa 是 strconv.Itoa 的薄包装（避免在多个文件里重复引入 strconv）。
func itoa(value int64) string {
	if value == 0 {
		return "0"
	}
	negative := value < 0
	if negative {
		value = -value
	}
	var digits [20]byte
	index := len(digits)
	for value > 0 {
		index--
		digits[index] = byte('0' + value%10)
		value /= 10
	}
	if negative {
		index--
		digits[index] = '-'
	}
	return string(digits[index:])
}
