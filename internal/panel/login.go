// ═══ 更新日志 ═══
// 2026-10-13：初版——扫码加号与 Trae 网页授权（与 panel/app.py 的同名方法同口径）。
//
// ═══ 两条加号路径，操作量不同但落盘同一形态 ═══
//
//   - **cn / global 扫码**：面板起 ./login 拿授权链接 → 用户扫码 → 轮询 ./login poll
//     拿到凭据 → 落盘 → 重启网关加载。登录流程用 32 位 hex 的 login_id 贯穿
//     （每次扫码一个独立流程 ID），所以轮询不会串到上一次的流程上。
//
//   - **Trae 网页授权**：Trae 的授权页强制把回调设成 http://127.0.0.1:8765/authorize，
//     浏览器打不开那个地址，但**地址栏里带着全部凭据**。所以只生成链接，由用户完成
//     授权后把地址栏整条粘回来。代价是每次加号要复制粘贴一次，换来零本机依赖。
//
// 两条路径最终都调用同一个 writeAuthFile / writeTraeAuthFile，落盘形态一致——网关
// 只认一种格式，分流只发生在「怎么拿到凭据」这一步。
package panel

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/upstream/trae"
)

// loginStart 生成扫码登录链接。
func (s *Server) loginStart(ctx context.Context, body map[string]any) map[string]any {
	realm := strings.ToLower(strings.TrimSpace(jsonString(body["realm"])))
	if realm == "" {
		realm = "cn"
	}
	if !knownRealm(realm) {
		return map[string]any{"ok": false, "message": realmUnsupportedMessage(realm)}
	}
	if !s.containerRunning(ctx) {
		return map[string]any{"ok": false, "message": "容器未运行，先启动服务"}
	}
	loginID, err := randomHex(16)
	if err != nil {
		return map[string]any{"ok": false, "message": "生成登录流程标识失败：" + err.Error()}
	}
	result := s.runner.Run(ctx, []string{"exec", s.containerName(), "./login",
		"--realm=" + realm, "--session=" + loginID, "url"}, 60*time.Second, nil)
	if result.Code != 0 {
		return map[string]any{"ok": false,
			"message": firstNonEmpty(result.Stderr, result.Stdout, "获取授权链接失败")}
	}
	// 取最后一行：辅助程序可能先打日志再打 URL，最后一行才是链接。
	url := ""
	if lines := strings.Split(strings.TrimSpace(result.Stdout), "\n"); len(lines) > 0 {
		url = strings.TrimSpace(lines[len(lines)-1])
	}
	if !strings.HasPrefix(url, "http") {
		return map[string]any{"ok": false,
			"message": "没拿到授权链接: " + truncateText(firstNonEmpty(result.Stdout, result.Stderr), 200)}
	}
	return map[string]any{"ok": true, "url": url, "realm": realm, "login_id": loginID}
}

// loginPoll 轮询扫码结果；拿到凭据就落盘并重启网关加载。
func (s *Server) loginPoll(ctx context.Context, body map[string]any) map[string]any {
	realm := strings.ToLower(strings.TrimSpace(jsonString(body["realm"])))
	if realm == "" {
		realm = "cn"
	}
	if !knownRealm(realm) {
		return map[string]any{"ok": false, "message": realmUnsupportedMessage(realm)}
	}
	loginID := jsonString(body["login_id"])
	if !loginIDRe.MatchString(loginID) {
		return map[string]any{"ok": false, "message": "登录流程已失效，请重新生成链接"}
	}
	result := s.runner.Run(ctx, []string{"exec", s.containerName(), "./login",
		"--realm=" + realm, "--session=" + loginID, "poll"}, 90*time.Second, nil)
	if result.Code != 0 {
		return map[string]any{"ok": false, "pending": true,
			"message": firstNonEmpty(result.Stderr, result.Stdout, "poll 失败")}
	}
	var pollResult map[string]any
	if err := json.Unmarshal([]byte(result.Stdout), &pollResult); err != nil {
		return map[string]any{"ok": false,
			"message": "poll 输出无法解析: " + truncateText(result.Stdout, 200)}
	}
	s.lock.Lock()
	defer s.lock.Unlock()
	summary, err := s.writeAuthFile(pollResult)
	if err != nil {
		return map[string]any{"ok": false, "message": err.Error()}
	}
	ok, message, seconds := s.restartContainer(ctx)
	if !ok {
		return map[string]any{"ok": false, "message": "凭证已保存，但 " + message, "account": summary}
	}
	return map[string]any{"ok": true, "account": summary,
		"message": formatMessage("账号已添加(%s)，重启耗时 %ss", jsonString(summary["nickname"]), formatSeconds(seconds))}
}

// knownRealm 判断 realm 是否合法。
func knownRealm(realm string) bool {
	return realm == "cn" || realm == "global" || realm == "trae"
}

// realmUnsupportedMessage 给出与 Python 版一致的 realm 错误文案。
func realmUnsupportedMessage(realm string) string {
	if realm == "trae" {
		return "该区域不支持扫码登录，请用 Trae 授权"
	}
	return "realm 只能是 cn 或 global"
}

// traeLoginStart 生成 Trae 授权链接。
//
// 只用**本机回调**（127.0.0.1:8765/authorize）：这是 Trae 唯一接受的回调地址。
// 实测把回调指向本服务公网地址时，Trae 侧一律报「网络错误」，服务器上一条回调请求
// 都收不到——官方 client_id 的回调白名单只认 localhost。
func (s *Server) traeLoginStart() map[string]any {
	traceID, err := randomUUID()
	if err != nil {
		return map[string]any{"ok": false, "message": "生成授权链接失败：" + err.Error()}
	}
	machineID, err := randomHex(32)
	if err != nil {
		return map[string]any{"ok": false, "message": "生成授权链接失败：" + err.Error()}
	}
	deviceID, err := randomDigits(19)
	if err != nil {
		return map[string]any{"ok": false, "message": "生成授权链接失败：" + err.Error()}
	}
	return map[string]any{"ok": true, "url": TraeBuildAuthURL(traceID, machineID, deviceID, ""), "login_id": traceID}
}

// traeLoginFinish 接收用户粘贴的回调 URL，解析凭据并落盘成 Trae 账号。
func (s *Server) traeLoginFinish(ctx context.Context, body map[string]any) map[string]any {
	raw := jsonString(body["callback"])
	if len(raw) > 16384 {
		return map[string]any{"ok": false, "message": "回调内容格式不正确"}
	}
	creds, err := TraeParseCallback(raw)
	if err != nil {
		return map[string]any{"ok": false, "message": err.Error()}
	}
	uid := jsonString(creds["uid"])
	if !uidRe.MatchString(uid) {
		return map[string]any{"ok": false, "message": "授权结果里没有拿到有效的用户 ID，请重新授权"}
	}
	expiresAt := TraeNormalizeExpire(creds["token_expire_at"])
	if expiresAt <= 0 {
		// Trae 的 JWT 自带过期时间但回调里不一定带；给一个保守值，
		// 网关会按 refreshToken 自动续期（trae.Refresh）。
		expiresAt = s.now().Add(time.Hour).Unix()
	}
	s.lock.Lock()
	defer s.lock.Unlock()
	summary, err := s.writeTraeAuthFile(creds, uid, expiresAt)
	if err != nil {
		return map[string]any{"ok": false, "message": err.Error()}
	}
	ok, message, seconds := s.restartContainer(ctx)
	if !ok {
		return map[string]any{"ok": false, "message": "凭证已保存，但 " + message, "account": summary}
	}
	return map[string]any{"ok": true, "account": summary,
		"message": formatMessage("Trae 账号已添加(%s)，重启耗时 %ss", jsonString(summary["nickname"]), formatSeconds(seconds))}
}

// traeCallback 接收 Trae 授权回调（浏览器直接跳到这里，无管理会话）。
//
// 成功：解析凭据 → 落盘成 trae 账号 → 重启网关加载 → 返回一个可关闭的提示页。
// 失败：返回可读的原因，并把原始参数留在页面上便于排查。
//
// 这条路径经 nginx 暴露在公网（Trae 只能回调到公网地址），因此**不返回任何凭据内容**，
// 页面只说成功或失败原因。
func (s *Server) traeCallback(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	query := r.URL.RawQuery
	raw := ""
	if query != "" {
		raw = "?" + query
	}
	creds, err := TraeParseCallback(raw)
	if err != nil {
		writeHTML(w, http.StatusBadRequest, traeCallbackPage(false, err.Error(), ""))
		return
	}
	uid := jsonString(creds["uid"])
	if !uidRe.MatchString(uid) {
		writeHTML(w, http.StatusBadRequest, traeCallbackPage(false, "授权结果里没有拿到有效的用户 ID", ""))
		return
	}
	expiresAt := TraeNormalizeExpire(creds["token_expire_at"])
	if expiresAt <= 0 {
		expiresAt = s.now().Add(time.Hour).Unix()
	}
	s.lock.Lock()
	summary, writeErr := s.writeTraeAuthFile(creds, uid, expiresAt)
	ok, message, _ := s.restartContainer(ctx)
	s.lock.Unlock()
	if writeErr != nil {
		writeHTML(w, http.StatusInternalServerError, traeCallbackPage(false, "保存失败："+writeErr.Error(), ""))
		return
	}
	nickname := jsonString(summary["nickname"])
	if !ok {
		writeHTML(w, http.StatusOK, traeCallbackPage(true, "凭据已保存，但 "+message, nickname))
		return
	}
	writeHTML(w, http.StatusOK, traeCallbackPage(true, "账号已添加并加载", nickname))
}

// writeAuthFile 把扫码结果落盘成网关可读的 auth 文件。
//
// 落盘顺序是**先 chown/chmod 再 rename**，不能反过来：先 rename 会留下一个窗口，
// 期间文件属 root:0600，网关以 uid 10001 运行读不到，会静默跳过该账号
// （2026-09-23 实测：加号后只加载 23 个账号，10 秒后才恢复 24）。
func (s *Server) writeAuthFile(pollResult map[string]any) (map[string]any, error) {
	uid := jsonString(pollResult["uid"])
	if !uidRe.MatchString(uid) {
		return nil, formatError("上游返回的 uid 不合法: %q", uid)
	}
	expiresIn := jsonInt64(pollResult["expires_in"])
	doc := map[string]any{
		"account": map[string]any{
			"uid":          uid,
			"enterpriseId": jsonString(pollResult["enterprise_id"]),
			"nickname":     jsonString(pollResult["nickname"]),
		},
		"auth": map[string]any{
			"accessToken":  jsonString(pollResult["access_token"]),
			"refreshToken": jsonString(pollResult["refresh_token"]),
			"expiresAt":    s.now().Unix() + expiresIn,
			"domain":       jsonString(pollResult["domain"]),
			"realm":        firstNonEmpty(jsonString(pollResult["realm"]), "cn"),
		},
	}
	auth := doc["auth"].(map[string]any)
	if jsonString(auth["accessToken"]) == "" {
		return nil, formatError("上游没有返回 access_token")
	}
	finalPath := filepath.Join(s.opts.AuthDir, "workbuddy-"+uid+".json")
	if err := s.writeAuthDocument(finalPath, doc, ".workbuddy-"); err != nil {
		return nil, err
	}
	nickname := jsonString(doc["account"].(map[string]any)["nickname"])
	return map[string]any{
		"uid": uid, "nickname": nickname, "realm": auth["realm"], "expiresAt": auth["expiresAt"],
		"expiresInDays": round1(float64(expiresIn) / 86400.0),
	}, nil
}

// writeTraeAuthFile 把 Trae 凭据落盘成网关可读的 auth 文件。
//
// 文件名用 trae- 前缀区分域（网关按内容里的 realm 判定，前缀只是运维可读性），
// 但**必须仍匹配网关的 AuthFileGlob**（workbuddy*.json）才会被扫描到。
func (s *Server) writeTraeAuthFile(creds map[string]any, uid string, expiresAt int64) (map[string]any, error) {
	accessToken := jsonString(creds["access_token"])
	account := map[string]any{
		"uid":      uid,
		"nickname": firstNonEmpty(jsonString(creds["nickname"]), uidPrefix(uid)),
	}
	// 补手机号：Trae 的 uid 是一串内部数字（如 411685466945619），运维看不出
	// 哪个号是哪个。GetUserInfo 会返回掩码手机号（138******00）与真实昵称，
	// 这是唯一能把账号和「人」对上的标识。
	//
	// 失败不阻断加号：手机号只是展示用，拿不到就留空，前端回落到 uid。
	// 上游偶发超时/接口变更都不该让「加号」这个主流程失败。
	if info, err := s.fetchTraeUserInfo(accessToken, uid); err == nil {
		if info.Mobile != "" {
			account["mobile"] = info.Mobile
		}
		if name := strings.TrimSpace(info.ScreenName); name != "" {
			account["nickname"] = name
		}
	} else {
		log.Printf("WARN: [面板] 取 Trae 账号资料失败（不影响加号）uid=%s: %v", uid, err)
	}
	doc := map[string]any{
		"account": account,
		"auth": map[string]any{
			"accessToken":  accessToken,
			"refreshToken": jsonString(creds["refresh_token"]),
			"expiresAt":    expiresAt,
			"domain":       firstNonEmpty(jsonString(creds["host"]), "api.trae.cn"),
			"realm":        "trae",
		},
	}
	if accessToken == "" {
		return nil, formatError("授权结果里没有 access_token")
	}
	finalPath := filepath.Join(s.opts.AuthDir, "workbuddy-trae-"+uid+".json")
	if err := s.writeAuthDocument(finalPath, doc, ".workbuddy-trae-"); err != nil {
		return nil, err
	}
	return map[string]any{
		"uid": uid, "nickname": jsonString(doc["account"].(map[string]any)["nickname"]),
		"mobile": jsonString(account["mobile"]),
		"realm":  "trae", "expiresAt": expiresAt,
		"expiresInDays": round1(float64(maxInt64(0, expiresAt-s.now().Unix())) / 86400.0),
	}, nil
}

// fetchTraeUserInfo 用刚拿到的 accessToken 查账号资料（掩码手机号 + 昵称）。
//
// 单独包一层是为了让调用点能简单地「失败就跳过」：这个调用不是加号的关键路径。
func (s *Server) fetchTraeUserInfo(accessToken, uid string) (trae.UserInfo, error) {
	if accessToken == "" {
		return trae.UserInfo{}, formatError("缺少 accessToken")
	}
	return trae.GetUserInfo(context.Background(), &auth.Auth{
		UID:         uid,
		AccessToken: accessToken,
		Domain:      "api.trae.cn",
	})
}

// writeAuthDocument 原子写一个 auth 文件：先落临时文件、改属主权限，再 rename。
//
// 顺带清掉同名的 .disabled 文件：同一个账号重新加号时，旧的那份禁用副本会让
// 「已启用」与「已禁用」两份并存，之后启停/删除都会撞上「存在同名凭据」的冲突。
func (s *Server) writeAuthDocument(finalPath string, doc map[string]any, prefix string) error {
	if err := os.MkdirAll(s.opts.AuthDir, 0700); err != nil {
		return err
	}
	// 缩进 1 空格：与 Python 的 json.dump(indent=1) 逐字一致。auth 文件会被运维
	// 直接打开看，缩进变化会让 diff 变成全文件重写。
	raw, err := marshalOrderedIndent(doc, " ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(s.opts.AuthDir, prefix)
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if _, err := tmp.Write(raw); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	// 先改属主与权限，再原子替换：文件一落地就是网关可读的形态。
	chownApp(tmpName)
	if err := os.Rename(tmpName, finalPath); err != nil {
		return err
	}
	if _, err := os.Stat(finalPath + ".disabled"); err == nil {
		_ = os.Remove(finalPath + ".disabled")
	}
	return nil
}

// marshalOrderedIndent 把普通 map 编成保序 JSON（auth 文件的字段序固定：account 在前）。
//
// auth 文件的键序对网关无影响（它按字段名读），但对运维有影响：每次加号后的 diff
// 应该是「值变了」，而不是「整段重排」。
func marshalOrderedIndent(doc map[string]any, indent string) ([]byte, error) {
	object := newOrderedObject()
	for _, key := range []string{"account", "auth"} {
		value, ok := doc[key]
		if !ok {
			continue
		}
		object.Set(key, nativeToOrdered(value))
	}
	return marshalOrdered(object, indent)
}

// nativeToOrdered 把 map/slice 转成保序对象（键按 map 迭代序，调用方负责传入有序来源）。
func nativeToOrdered(value any) any {
	switch v := value.(type) {
	case map[string]any:
		object := newOrderedObject()
		// account/auth 段内部字段少且固定，按显式顺序写，保证每次落盘形态一致。
		order := []string{"uid", "enterpriseId", "nickname", "accessToken", "refreshToken",
			"expiresAt", "domain", "realm"}
		for _, key := range order {
			if item, ok := v[key]; ok {
				object.Set(key, nativeToOrdered(item))
			}
		}
		for key, item := range v {
			if _, exists := object.Get(key); !exists {
				object.Set(key, nativeToOrdered(item))
			}
		}
		return object
	case []any:
		items := make([]any, 0, len(v))
		for _, item := range v {
			items = append(items, nativeToOrdered(item))
		}
		return items
	default:
		return value
	}
}

// traeCallbackPage 是 Trae 授权回调的结果页（自包含，不需要面板会话与静态资源）。
func traeCallbackPage(ok bool, message, nickname string) string {
	title := "授权失败"
	color := "#c0392b"
	if ok {
		title = "授权成功"
		color = "#1a8c5c"
	}
	extra := ""
	if nickname != "" {
		extra = "<p>账号：" + escapeHTML(nickname) + "</p>"
	}
	return `<!doctype html>
<html lang="zh-CN"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>Trae ` + title + `</title>
<style>
body{font:15px system-ui,-apple-system,"Segoe UI","Microsoft YaHei",sans-serif;
background:#0f1117;color:#e8eaed;display:flex;align-items:center;
justify-content:center;min-height:100vh;margin:0;padding:20px}
.p{background:#1a1d28;border:1px solid #2d3140;border-radius:10px;
max-width:460px;width:100%;padding:26px 24px}
h1{font-size:17px;margin:0 0 12px;color:` + color + `}
p{margin:8px 0;color:#9aa0b0;font-size:13px;line-height:1.6;word-break:break-all}
</style></head><body><div class="p">
<h1>` + title + `</h1>` + extra + `<p>` + escapeHTML(message) + `</p>
<p>这个页面可以关闭了。</p>
</div></body></html>`
}

// escapeHTML 转义 HTML 特殊字符。
func escapeHTML(text string) string {
	replacer := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;")
	return replacer.Replace(text)
}

// randomUUID 生成一个 RFC4122 v4 形态的 UUID 串（Trae 的 login_trace_id 要这个形状）。
//
// 不用第三方库：这里只需要「看起来是 UUID」，而面板二进制要能塞进安卓包，
// 每多一个依赖都是体积。
func randomUUID() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	buf[6] = (buf[6] & 0x0f) | 0x40 // version 4
	buf[8] = (buf[8] & 0x3f) | 0x80 // variant 10
	return fmt.Sprintf("%x-%x-%x-%x-%x", buf[0:4], buf[4:6], buf[6:8], buf[8:10], buf[10:16]), nil
}

// formatError 是 fmt.Errorf 的薄包装（面板文案里大量是「上游返回的 uid 不合法: %q」）。
func formatError(format string, args ...any) error { return fmt.Errorf(format, args...) }

func round1(value float64) float64 {
	if value < 0 {
		return 0
	}
	return float64(int64(value*10+0.5)) / 10
}

func maxInt64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}
