// ═══ 更新日志 ═══
// 2026-10-13：初版——账号与服务的写操作（与 panel/app.py 的同名方法同口径）。
//
// ═══ 两条「重启」路径，别混 ═══
//
//   - **文件层启停**（account_toggle）：改 auth 文件名，网关扫目录时看不到它。
//   - **池层停用**（account_revive）：网关自己记的状态（被上游封禁/内容审核标记），
//     存在 state.json 里，与文件无关。
//
// 两者独立：一个账号可能文件是启用的、却在池里被停用（上游封禁），也可能文件被
// 停用了、池里还留着记录。面板把两种状态并排显示，运维可能两个都要用。
//
// 另外，**每个改动账号的操作都要重启网关**：auth 文件在网关启动时一次性加载，
// 容器里 config.json 还是只读挂载，改完不重启不会生效。重启失败不是「操作失败」——
// 文件已经改对了，所以提示必须写成「已禁用 xxx，但重启失败：…」，让运维知道
// 重试重启即可，而不是以为白改了。
package panel

import (
	"context"
	"os"
	"strings"
	"time"
)

// restartTimeout 是重启网关的超时。要覆盖容器停止宽限期（默认 10s）加上启动时间，
// 所以给到 210 秒；健康检查另有一段自己的预算。
const restartTimeout = 210 * time.Second

// healthTimeout 是重启后等待网关健康的总时长。
const healthTimeout = 45 * time.Second

// restartContainer 重启网关并确认它真的起来了。
//
// 返回 (ok, message, 秒数)。**必须用真实健康响应确认成功**：只看 `docker restart`
// 的退出码是不够的——容器起来了但网关可能因为账号文件坏掉而启动失败，那时报「已重启」
// 会让运维以为账号已加载（2026-09-18 修的就是这个）。
func (s *Server) restartContainer(ctx context.Context) (bool, string, float64) {
	started := s.now()
	result := s.runner.Run(ctx, []string{"restart", s.containerName()}, restartTimeout, nil)
	if result.Code != 0 {
		return false, "重启失败: " + firstNonEmpty(result.Stderr, result.Stdout), 0
	}
	// 原生模式（桌面/安卓）下 restart 就是一次运行时重载，网关自己会确认完成，
	// 没有容器可轮询。
	if s.opts.Runtime == "native" {
		return true, "配置与账号已重新加载", s.elapsed(started)
	}
	deadline := s.now().Add(healthTimeout)
	for s.now().Before(deadline) {
		remaining := deadline.Sub(s.now())
		if remaining <= 0 {
			break
		}
		probeCtx, cancel := contextWithTimeout(ctx, minDuration(5*time.Second, remaining))
		health := s.gateway.Get(probeCtx, "/healthz")
		cancel()
		// 判据是「网关在服务」而不是「网关健康」：/healthz 在**一个账号都没有**时
		// 返回 503（ServableNow 为假），而那种部署（刚装好、还没加号）重启后必然
		// 拿不到 200。只看 200 会把它报成「重启已执行但健康检查未通过」——用户看到
		// 一条永远无法满足的失败提示。service 字段与 X-Service 头是网关的身份标识，
		// 有它、且没有 error 键，就说明进程起来了、能响应了。
		if jsonString(health["service"]) == "workbuddy2api" && health["error"] == nil {
			return true, "已重启并加载账号", s.elapsed(started)
		}
		select {
		case <-ctx.Done():
			return false, "容器重启已执行，但网关健康检查未通过，请查看日志后刷新", s.elapsed(started)
		case <-time.After(time.Second):
		}
	}
	return false, "容器重启已执行，但网关健康检查未通过，请查看日志后刷新", s.elapsed(started)
}

func (s *Server) elapsed(started time.Time) float64 {
	return float64(s.now().Sub(started).Milliseconds()) / 1000
}

func minDuration(a, b time.Duration) time.Duration {
	if a < b {
		return a
	}
	return b
}

// accountToggle 启用/停用账号（改文件名后缀）。
func (s *Server) accountToggle(ctx context.Context, body map[string]any) map[string]any {
	uid := jsonString(body["uid"])
	wantDisabled := jsonBool(body["disabled"], false)
	s.lock.Lock()
	defer s.lock.Unlock()
	entry := s.store.Find(uid)
	if entry == nil {
		return map[string]any{"ok": false, "message": formatMessage("找不到账号 %s", uid)}
	}
	if entry.Disabled == wantDisabled {
		// 状态本来就对：不重启（重启一次要几十秒，白重启很扰民）。
		return map[string]any{"ok": true, "message": "状态未变化", "restart": nil}
	}
	src := entry.Path
	dst := src + ".disabled"
	if !wantDisabled {
		dst = strings.TrimSuffix(src, ".disabled")
	}
	if _, err := os.Stat(dst); err == nil {
		// 同时存在启用与禁用两份：保留两份文件让人核对，不擅自覆盖。
		return map[string]any{"ok": false,
			"message": "存在同名的启用与禁用凭据，已保留两份文件，请先核对重复账号"}
	}
	if err := os.Rename(src, dst); err != nil {
		return map[string]any{"ok": false, "message": "重命名失败：" + err.Error()}
	}
	chownApp(dst)
	ok, message, seconds := s.restartContainer(ctx)
	action := "禁用"
	if !wantDisabled {
		action = "启用"
	}
	out := map[string]any{
		"ok":      ok,
		"restart": nil,
		"message": formatMessage("已%s %s，%s", action, uidPrefix(entry.UID), message),
	}
	if ok {
		out["restart"] = seconds
	}
	return out
}

// accountDelete 把账号移入回收目录（可手动恢复），不直接删除。
func (s *Server) accountDelete(ctx context.Context, body map[string]any) map[string]any {
	uid := jsonString(body["uid"])
	s.lock.Lock()
	defer s.lock.Unlock()
	entry := s.store.Find(uid)
	if entry == nil {
		return map[string]any{"ok": false, "message": formatMessage("找不到账号 %s", uid)}
	}
	trashed, err := s.store.Delete(uid)
	if err != nil {
		return map[string]any{"ok": false, "message": err.Error()}
	}
	ok, message, seconds := s.restartContainer(ctx)
	out := map[string]any{
		"ok":      ok,
		"restart": nil,
		"trashed": trashed,
		"message": formatMessage("已删除 %s(回收件 %s)，%s", uidPrefix(entry.UID), trashed, message),
	}
	if ok {
		out["restart"] = seconds
	}
	return out
}

// accountRevive 复活被网关停用的账号（清 disabled + reason，账号回到可选池）。
//
// 只走网关的管理通道 POST /accounts/revive；**不改 auth 文件**——池级停用是网关自己
// 记的状态，文件层面的启停是另一回事（见 accountToggle）。
func (s *Server) accountRevive(ctx context.Context, body map[string]any) map[string]any {
	uid := jsonString(body["uid"])
	ok, payload := s.gateway.PostJSON(ctx, "/accounts/revive", map[string]any{"uid": uid})
	if !ok {
		return map[string]any{"ok": false, "message": gatewayEnvelopeError(payload)}
	}
	if !jsonBool(payload["ok"], false) {
		return map[string]any{"ok": false, "message": firstNonEmpty(errorMessage(payload), "复活失败")}
	}
	if !jsonBool(payload["was_disabled"], false) {
		return map[string]any{"ok": true, "message": "该账号未被停用，无需复活"}
	}
	return map[string]any{"ok": true, "message": formatMessage("已复活 %s", uidPrefix(uid))}
}

// accountFission 补发 Trae 邀请奖励（面板「补发邀请奖励」按钮）。
//
// 背景：面板上的邀请记录停在「邀请中」，不是服务端没发，而是**发奖动作在 TRAE 桌面端
// 本地**——客户端登录时自己调一次 code_fission/grant。被邀请人没登过桌面端，记录就
// 永远挂着。这里把那一次调用搬到服务端补发。
//
// 走 ./fission_bin（与 ./credit 同一条受限运行通道），只传 -uid，设备号用账号默认代数；
// 重复调用是幂等的（上游返回 1028「已发放」）。
func (s *Server) accountFission(ctx context.Context, body map[string]any) map[string]any {
	uid := strings.TrimSpace(jsonString(body["uid"]))
	if !uidRe.MatchString(uid) {
		return map[string]any{"ok": false, "message": "账号标识不正确"}
	}
	if !s.containerRunning(ctx) {
		return map[string]any{"ok": false, "message": "容器未运行"}
	}
	result := s.runner.Run(ctx, []string{"exec", s.containerName(), "./fission_bin", "-uid", uid}, 120*time.Second, nil)
	if result.Code != 0 && strings.TrimSpace(result.Stdout) == "" {
		return map[string]any{"ok": false, "message": firstNonEmpty(result.Stderr, "补发执行失败")}
	}
	data, raw := DecodeExecJSON(result.Stdout)
	if data == nil {
		return map[string]any{"ok": false, "message": "补发输出无法解析", "raw": raw}
	}
	rows, _ := data["accounts"].([]any)
	if len(rows) == 0 {
		return map[string]any{"ok": false, "message": "没有匹配的 Trae 账号（该 uid 可能不是 Trae 域）"}
	}
	row, _ := rows[0].(map[string]any)
	outcome := jsonString(row["result"])
	return map[string]any{"ok": outcome != "failed", "result": outcome, "message": fissionMessage(row)}
}

// fissionMessage 把补发结果翻译成一句给运维看的中文。
//
// 上游的业务码在 CLI 里已经翻成 result 三态（granted/already/failed），这里只负责补上
// 「发生了什么」和「下一步」。失败时保留上游原文——排障要看它。
func fissionMessage(row map[string]any) string {
	result := jsonString(row["result"])
	before, _ := jsonNumber(row["before"])
	after, _ := jsonNumber(row["after"])
	switch result {
	case "granted":
		delta, _ := jsonNumber(row["delta"])
		if delta != 0 {
			return formatMessage("已发放邀请奖励 %.0f → %.0f（+%.0f）", before, after, delta)
		}
		// 上游说 success 但积分没动：通常是奖励已入账、本次是重复触发。
		return formatMessage("上游返回成功，但积分未变化（当前 %.0f）；可能此前已发放", firstNonZero(after, before))
	case "already":
		return formatMessage("此前已发放过，无需重复补发（当前 %.0f）", before)
	}
	return "补发失败：" + firstNonEmpty(jsonString(row["error"]), "上游未返回结果")
}

func firstNonZero(values ...float64) float64 {
	for _, value := range values {
		if value != 0 {
			return value
		}
	}
	return 0
}

// containerRunning 判断网关是否在跑。
//
// ═══ 为什么与 state.go 共用同一个判据 ═══
//
// 这里原本走 runner 的 `inspect` 分支，而 state.go 走 /healthz —— **同一件事两套
// 实现**。两者会不一致：2026-10-13 真机实测，安卓上面板状态显示「网关运行中」、
// /api/state 里 containerRunning=true，但点「添加账号」却报「容器未运行，先启动
// 服务」（登录、任务、积分这些操作都走这个判据），功能直接不可用。
//
// 现在两边都走同一个函数：**看网关自己回的 service 字段**。这是唯一能同时回答
// 「进程活着吗」与「服务可用吗」的判据——面板本身就是被网关托管的，管理通道通了
// 只说明网关进程在，不代表它在服务。
//
// 注意**不能要求 HTTP 200**：一个账号都没有时 /healthz 返回 503，而那正是刚装好、
// 还没加号时的正常状态；只认 200 会让首次使用被卡死。
func (s *Server) containerRunning(ctx context.Context) bool {
	return gatewayServing(ctx, s.gateway)
}

// taskRun 手动触发一个排程任务。
func (s *Server) taskRun(ctx context.Context, body map[string]any) map[string]any {
	key := strings.TrimSpace(jsonString(body["key"]))
	if !KnownTask(key) {
		return map[string]any{"ok": false, "message": formatMessage("未知任务：%s", key)}
	}
	ok, payload := s.gateway.Post(ctx, "/tasks/"+key+"/run")
	if !ok {
		return map[string]any{"ok": false, "message": firstNonEmpty(errorMessage(payload), "触发失败")}
	}
	// 前端收到 ok 会直接打开该任务的日志窗口（2026-10-15 起），不再指引
	// 「下方日志」——任务页没有内嵌日志区，那句话让人找不到入口。
	return map[string]any{"ok": true, "message": "已触发"}
}

// taskToggle 改任务开关（写 config.json，必须重启才生效）。
func (s *Server) taskToggle(ctx context.Context, body map[string]any) map[string]any {
	key := strings.TrimSpace(jsonString(body["key"]))
	enabled := jsonBool(body["enabled"], false)
	s.lock.Lock()
	defer s.lock.Unlock()
	ok, message := SetTaskEnabled(s.opts.ConfigPath, key, enabled)
	if !ok {
		return map[string]any{"ok": false, "message": message}
	}
	if message == "状态未变化" {
		return map[string]any{"ok": true, "message": message}
	}
	restartOK, restartMessage, seconds := s.restartContainer(ctx)
	out := map[string]any{"ok": restartOK, "restart": nil, "message": restartMessage}
	if restartOK {
		out["restart"] = seconds
		out["message"] = formatMessage("已%s「%s」，%s", enabledLabel(enabled), key, restartMessage)
	} else {
		out["message"] = formatMessage("已%s「%s」的设置，但%s", enabledLabel(enabled), key, restartMessage)
	}
	return out
}

func enabledLabel(enabled bool) string {
	if enabled {
		return "启用"
	}
	return "停用"
}

// serviceRestart 手动重启网关。
func (s *Server) serviceRestart(ctx context.Context) map[string]any {
	s.lock.Lock()
	defer s.lock.Unlock()
	ok, message, seconds := s.restartContainer(ctx)
	out := map[string]any{"ok": ok, "restart": nil, "message": message}
	if ok {
		out["restart"] = seconds
	}
	return out
}

// reasoningLoopToggle 热切换重复推理保护的「命中后怎么办」。
//
// 与 config.json 里的 features.reasoning_loop_stop_only 是同一个语义：false（默认）
// 命中后同账号重发一次，true 命中即停止并如实报错。运行期值优先，不需要重启。
func (s *Server) reasoningLoopToggle(ctx context.Context, body map[string]any) map[string]any {
	stopOnly := jsonBool(body["stop_only"], false)
	if s.gateway.Management == nil || !s.gateway.Management.ManagementEnabled() {
		return map[string]any{"ok": false, "message": "无法读取网关配置"}
	}
	code, result := s.gateway.Management.Request(ctx, "POST", "/features/reasoning-loop",
		map[string]any{"stop_only": stopOnly})
	if code != 200 || !jsonBool(result["ok"], false) {
		return map[string]any{"ok": false, "message": gatewayEnvelopeError(result)}
	}
	applied := jsonBool(result["stop_only"], false)
	message := "已切换为「命中后自动重发」"
	if applied {
		message = "已切换为「命中即停止」"
	}
	return map[string]any{"ok": true, "stop_only": applied, "message": message}
}

// uidPrefix 取 uid 前 8 位（与网关日志的 uidPrefix 口径一致）。
//
// 面板的提示文案里只露前 8 位：完整 uid 是账号标识，显示在屏幕上没有额外价值，
// 而截图/工单里带上完整 uid 反而是一次不必要的泄露。
func uidPrefix(uid string) string {
	if len(uid) > 8 {
		return uid[:8]
	}
	return uid
}
