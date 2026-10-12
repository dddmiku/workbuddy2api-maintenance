// ═══ 更新日志 ═══
// 2026-10-13：初版——运维只读通道与密钥管理代理（与 panel/app.py 的 ops_get / keys_* 同口径）。
//
// 这一层的核心职责是**参数校验**：面板把运维的筛选条件转发给网关的私有通道，任何
// 没校验干净的值都会直接进入网关的内部接口。所以每个参数都按「允许集合 + 长度 +
// 字符集」三重判据过一遍，多余的、重复的、带控制字符的一律 400，并且**不发出**任何
// 转发请求——一条被拒的请求不该在网关侧留下痕迹。
package panel

import (
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// requestFacetParams 是 /api/requests 允许的筛选参数（与网关同语义）。
var requestFacetParams = []string{"key_id", "model", "status", "request_id", "offset", "limit"}

// requestStatuses 是允许的状态取值。
var requestStatuses = []string{"", "success", "error", "canceled", "rejected"}

var (
	requestIDRe = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,128}$`)
	digits9Re   = regexp.MustCompile(`^[0-9]{1,9}$`)
	keyIDRe     = regexp.MustCompile(`^legacy|key_[0-9a-f]{24}$`)
	updateTagRe = regexp.MustCompile(`^[A-Za-z0-9._-]*$`)
)

// opsGet 处理 /api/key-limits、/api/requests、/api/requests/{id}、/api/requests/facets。
func (s *Server) opsGet(w http.ResponseWriter, r *http.Request, path string) {
	query := r.URL.RawQuery
	if len(query) > 2048 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "message": "筛选条件过长"})
		return
	}
	if path == "/api/key-limits" {
		if query != "" {
			writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "message": "限流状态接口不接受筛选参数"})
			return
		}
		s.keysRequest(w, r, http.MethodGet, "/key-limits", nil)
		return
	}
	if path == "/api/requests/facets" {
		// 必须排在请求 ID 路由之前，否则会被当成 request_id="facets" 转发。
		if query != "" {
			writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "message": "筛选项接口不接受参数"})
			return
		}
		s.keysRequest(w, r, http.MethodGet, "/requests/facets", nil)
		return
	}
	if strings.HasPrefix(path, "/api/requests/") {
		raw := path[len("/api/requests/"):]
		requestID, err := url.PathUnescape(raw)
		if err != nil || !utf8.ValidString(requestID) {
			requestID = ""
		}
		// 带查询串或 ID 形态不合法一律拒掉：这条路由会把 ID 拼进网关的 URL 路径，
		// 放行一个含 `/` 或 `..` 的值等于给出了一条路径注入。
		if query != "" || !requestIDRe.MatchString(requestID) {
			writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "message": "请求标识不正确"})
			return
		}
		s.keysRequest(w, r, http.MethodGet, "/requests/"+requestID, nil)
		return
	}
	// 列表接口：解析并校验筛选参数。
	pairs, message := parseFacetPairs(query)
	if message != "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "message": message})
		return
	}
	endpoint := "/requests"
	if len(pairs) > 0 {
		encoded := url.Values{}
		for _, pair := range pairs {
			encoded.Add(pair[0], pair[1])
		}
		endpoint += "?" + encoded.Encode()
	}
	s.keysRequest(w, r, http.MethodGet, endpoint, nil)
}

// parseFacetPairs 解析筛选参数；返回错误文案（空串表示通过）。
func parseFacetPairs(query string) ([][2]string, string) {
	if query == "" {
		return nil, ""
	}
	allowed := map[string]bool{}
	for _, name := range requestFacetParams {
		allowed[name] = true
	}
	statusAllowed := map[string]bool{}
	for _, name := range requestStatuses {
		statusAllowed[name] = true
	}
	values := url.Values{}
	var pairs [][2]string
	for _, chunk := range strings.Split(query, "&") {
		if chunk == "" {
			return nil, "筛选参数格式错误"
		}
		key, raw, found := strings.Cut(chunk, "=")
		if !found {
			// Python 用 strict_parsing=True：缺 `=` 的片段直接报错，不当成空值。
			return nil, "筛选参数格式错误"
		}
		name, err := url.QueryUnescape(key)
		if err != nil {
			return nil, "筛选参数格式错误"
		}
		value, err := url.QueryUnescape(raw)
		if err != nil || !utf8.ValidString(value) {
			return nil, "筛选参数格式错误"
		}
		if !allowed[name] {
			return nil, "包含重复或不支持的筛选参数"
		}
		// 重复键必须拒：`?limit=1&limit=100` 在两边解析出不同的值，
		// 面板校验的是第一个、网关用的是最后一个（或反之），是一条绕过校验的缝。
		if _, duplicate := values[name]; duplicate {
			return nil, "包含重复或不支持的筛选参数"
		}
		limit := 128
		if name == "model" {
			limit = 256
		}
		if len(value) > limit || hasControlChar(value) {
			return nil, "筛选值无效"
		}
		if name == "offset" || name == "limit" {
			if !digits9Re.MatchString(value) {
				return nil, "分页参数无效"
			}
			if name == "limit" {
				number, _ := strconv.Atoi(value)
				if number < 1 || number > 100 {
					return nil, "分页参数无效"
				}
			}
		}
		values.Set(name, value)
		pairs = append(pairs, [2]string{name, value})
	}
	if status, ok := values["status"]; ok && !statusAllowed[status[0]] {
		return nil, "请求状态无效"
	}
	return pairs, ""
}

// hasControlChar 判断字符串里是否含控制字符（含 DEL）。
//
// 这些值会进入网关的内部接口与日志行；一个内嵌的换行足以伪造出一条日志记录，
// 或者把筛选条件拆成两个参数。
func hasControlChar(value string) bool {
	for _, r := range value {
		if r < 0x20 || r == 0x7f {
			return true
		}
	}
	return false
}

// keysRequest 经管理通道调用密钥库接口并回传结果。
func (s *Server) keysRequest(w http.ResponseWriter, r *http.Request, method, endpoint string, body any) {
	if s.gateway.Management == nil || !s.gateway.Management.ManagementEnabled() {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"ok": false,
			"message": "无法读取密钥管理配置"})
		return
	}
	code, result := s.gateway.Management.Request(r.Context(), method, endpoint, body)
	writeJSON(w, code, result)
}

// keysWithUsage 密钥列表附带每把密钥的累计 token 用量。
//
// 用量账本与密钥库是两个独立文件，面板在这里做一次合并，省得前端再拉一次
// /api/usage 并按 key_id 对齐。账本不可用时只丢用量列，密钥列表照常返回。
func (s *Server) keysWithUsage(w http.ResponseWriter, r *http.Request) {
	if s.gateway.Management == nil || !s.gateway.Management.ManagementEnabled() {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"ok": false,
			"message": "无法读取密钥管理配置"})
		return
	}
	socket := s.gateway.Management
	code, result := socket.Request(r.Context(), http.MethodGet, "/keys", nil)
	if code != 200 || !jsonBool(result["ok"], false) {
		writeJSON(w, code, result)
		return
	}
	entries, _ := result["keys"].([]any)
	if entries == nil {
		writeJSON(w, code, result)
		return
	}
	// 一次遍历同时取 token 与积分：两者同源（同一份 usage 账本），分两次取会让
	// 两列在并发写入下对不上。
	totals := map[string]any{}
	credits := map[string]any{}
	usageCode, usage := socket.Request(r.Context(), http.MethodGet, "/usage", nil)
	usageAvailable := usageCode == 200 && jsonBool(usage["ok"], false)
	if usageAvailable {
		rows, _ := usage["keys"].([]any)
		for _, item := range rows {
			entry, ok := item.(map[string]any)
			if !ok {
				continue
			}
			keyID := jsonString(entry["key_id"])
			if keyID == "" {
				continue
			}
			section := jsonObject(entry["totals"])
			if section == nil {
				continue
			}
			totals[keyID] = orZero(section["total_tokens"])
			// credit 只在上游 usage 显式带 credit 时累计（缺失≠0），所以这里原样
			// 透传，让面板区分「0 积分」与「没有该观测」。
			credits[keyID] = section["credit"]
		}
	}
	for _, item := range entries {
		entry, ok := item.(map[string]any)
		if !ok {
			continue
		}
		keyID := jsonString(entry["id"])
		if usageAvailable {
			entry["total_tokens"] = orZero(totals[keyID])
			entry["credit"] = credits[keyID]
		} else {
			entry["total_tokens"] = nil
			entry["credit"] = nil
		}
	}
	result["usage_available"] = usageAvailable
	writeJSON(w, http.StatusOK, result)
}

// keysPost 处理密钥的创建/修改/删除/复制。
//
// 这些路由在**会话之前**分流（与 Python 版一致）：它们要在读 body 时就用 8192 的上限，
// 而通用路径要按路径决定上限。闸门本身一样不少——会话、同源、管理标记，缺一即拒。
func (s *Server) keysPost(w http.ResponseWriter, r *http.Request, path string) {
	if s.session(w, r, false) == nil {
		s.rejectUnreadBody(w, r, http.StatusUnauthorized, map[string]any{"ok": false, "message": "请先登录管理面板"})
		return
	}
	if !s.originOK(r) || r.Header.Get("X-Admin-Request") != "1" {
		s.rejectUnreadBody(w, r, http.StatusForbidden,
			map[string]any{"ok": false, "message": "请求来源无效，请从管理页面重新操作"})
		return
	}
	body, ok := s.readBody(w, r, 8192)
	if !ok {
		return
	}
	if raw, present := body["reasoning_loop_guard"]; present {
		if _, isBool := raw.(bool); !isBool {
			writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "message": "重复推理保护必须为开启或关闭"})
			return
		}
	}
	if raw, present := body["global_fallback_to_cn"]; present {
		if _, isBool := raw.(bool); !isBool {
			writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "message": "CN 回落必须为开启或关闭"})
			return
		}
	}
	if raw, present := body["limits"]; present && !validLimits(raw) {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false,
			"message": "限流需为整数：每分钟 0–60000 次、并发 0–256、等待 0–30 秒；排队需先设置并发上限"})
		return
	}
	if path == "/api/keys" {
		if message := unsupportedFields(body, "name", "note", "models", "reasoning_loop_guard",
			"global_fallback_to_cn", "expires_at", "limits"); message != "" {
			writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "message": message})
			return
		}
		if raw, present := body["expires_at"]; present && !validExpiry(raw) {
			writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false,
				"message": "有效期需为 RFC3339 时间，留空表示无限制"})
			return
		}
		if message := modelBindingError(body["models"]); message != "" {
			writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "message": message})
			return
		}
		s.keysRequest(w, r, http.MethodPost, "/keys", body)
		return
	}
	keyID := jsonString(body["id"])
	if !keyIDRe.MatchString(keyID) {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "message": "密钥标识不正确"})
		return
	}
	switch path {
	case "/api/keys/copy":
		if message := exactFields(body, "id"); message != "" {
			writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "message": message})
			return
		}
		s.keysRequest(w, r, http.MethodPost, "/keys/"+keyID+"/copy", map[string]any{})
		return
	case "/api/keys/delete":
		if message := exactFields(body, "id"); message != "" {
			writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "message": message})
			return
		}
		s.keysRequest(w, r, http.MethodDelete, "/keys/"+keyID, nil)
		return
	}
	changes := map[string]any{}
	for key, value := range body {
		if key != "id" {
			changes[key] = value
		}
	}
	if len(changes) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "message": "没有有效的修改字段"})
		return
	}
	if message := unsupportedFields(changes, "name", "note", "enabled", "models",
		"reasoning_loop_guard", "global_fallback_to_cn", "expires_at", "limits"); message != "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "message": "没有有效的修改字段"})
		return
	}
	if raw, present := changes["expires_at"]; present && !validExpiry(raw) {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false,
			"message": "有效期需为 RFC3339 时间，留空表示无限制"})
		return
	}
	if raw, present := changes["models"]; present {
		if message := modelBindingError(raw); message != "" {
			writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "message": message})
			return
		}
	}
	s.keysRequest(w, r, http.MethodPatch, "/keys/"+keyID, changes)
}

// unsupportedFields 检查是否含允许集合之外的字段；返回错误文案。
func unsupportedFields(body map[string]any, allowed ...string) string {
	permitted := map[string]bool{}
	for _, name := range allowed {
		permitted[name] = true
	}
	for field := range body {
		if !permitted[field] {
			return "包含不支持的字段"
		}
	}
	return ""
}

// exactFields 要求字段集合与 allowed 完全一致（多一个少一个都不行）。
func exactFields(body map[string]any, allowed ...string) string {
	if len(body) != len(allowed) {
		return "包含不支持的字段"
	}
	return unsupportedFields(body, allowed...)
}

// validLimits 校验限流策略：允许 null（恢复无限制），否则必须是整数且在范围内。
//
// `type(number) is not int` 在 Python 里会拒掉 True（bool 是 int 的子类）与 "2"、2.5。
// Go 的 JSON 解码把数字都解成 float64，所以这里必须自己判「是不是整数」——
// 否则前端传 2.5 会被悄悄截断成 2，运维以为限流是 2.5。
func validLimits(value any) bool {
	if value == nil {
		return true
	}
	object, ok := value.(map[string]any)
	if !ok {
		return false
	}
	maximums := map[string]float64{
		"requests_per_minute":   60000,
		"max_concurrent":        256,
		"queue_timeout_seconds": 30,
	}
	for name, raw := range object {
		limit, known := maximums[name]
		if !known {
			return false
		}
		number, ok := jsonNumber(raw)
		if !ok || number != float64(int64(number)) || number < 0 || number > limit {
			return false
		}
	}
	// 排队必须先有并发上限：只配 queue_timeout 而不配 max_concurrent 的语义是
	// 「无限排队但只等 N 秒」，实现上是「永远排不进去」，配置本身自相矛盾。
	queueTimeout, _ := jsonNumber(object["queue_timeout_seconds"])
	maxConcurrent, _ := jsonNumber(object["max_concurrent"])
	return queueTimeout == 0 || maxConcurrent > 0
}

// validExpiry 有效期允许 null（无限制）或长度 ≤40 的字符串。
func validExpiry(value any) bool {
	if value == nil {
		return true
	}
	text, ok := value.(string)
	return ok && len(text) <= 40
}

// modelBindingError 校验模型绑定，返回错误文案；空串表示通过。
//
// 绑定按完整模型名逐字比对，裸名一条也匹配不上——模型列表里只有带 cn:/global:/
// trae: 前缀的名字。网关侧也会拒裸名，这里先拦一道，是为了让手填的场景在表单上
// 就得到明确提示，而不是保存成功却发现调用全 403。
func modelBindingError(value any) string {
	if value == nil {
		return ""
	}
	items, ok := value.([]any)
	if !ok || len(items) > 64 {
		return "模型绑定需为最多 64 个不重复的模型名"
	}
	seen := map[string]bool{}
	for _, item := range items {
		name, ok := item.(string)
		if !ok || name == "" || len(name) > 64 || strings.TrimSpace(name) != name {
			return "模型绑定需为最多 64 个不重复的模型名"
		}
		if hasSpaceOrControl(name) {
			return "模型绑定需为最多 64 个不重复的模型名"
		}
		if seen[name] {
			return "模型绑定需为最多 64 个不重复的模型名"
		}
		seen[name] = true
		// 前缀后必须还有模型名：`cn:` / `global:` 这种只有前缀的写法同样匹配不上。
		if !modelNameRe.MatchString(name) {
			return "模型绑定必须选完整模型名（cn: / global: / trae: 开头），请从模型列表添加"
		}
	}
	return ""
}

var modelNameRe = regexp.MustCompile(`^(cn|global|trae):.+`)

func hasSpaceOrControl(text string) bool {
	for _, r := range text {
		if r <= 0x20 || r == 0x7f {
			return true
		}
	}
	return false
}

// modelsPayload 把 /v1/models 的原始响应整理成面板要的形状。
//
// models 是纯 id 列表（兼容旧前端），items 带展示字段（倍率/名称/供应商/能力旗标），
// 全部原样透传，缺字段就省略——不编造。
func modelsPayload(payload map[string]any) map[string]any {
	ids := []string{}
	items := []map[string]any{}
	rows, _ := payload["data"].([]any)
	for _, row := range rows {
		entry, ok := row.(map[string]any)
		if !ok {
			continue
		}
		id := jsonString(entry["id"])
		if id == "" {
			continue
		}
		ids = append(ids, id)
		item := map[string]any{"id": id}
		for _, key := range []string{"credits", "name", "vendor", "tags", "description"} {
			if value := entry[key]; value != nil && !isZeroJSON(value) {
				item[key] = value
			}
		}
		for _, key := range []string{"supports_images", "supports_reasoning", "supports_tool_call"} {
			if jsonBool(entry[key], false) {
				item[key] = true
			}
		}
		items = append(items, item)
	}
	sort.Strings(ids)
	unique := ids[:0]
	for index, id := range ids {
		if index == 0 || ids[index-1] != id {
			unique = append(unique, id)
		}
	}
	return map[string]any{"ok": true, "models": unique, "items": items}
}

// isZeroJSON 判断 JSON 值是否「假值」（Python 的 `if item.get(key)` 语义）。
func isZeroJSON(value any) bool {
	switch v := value.(type) {
	case nil:
		return true
	case bool:
		return !v
	case string:
		return v == ""
	case float64:
		return v == 0
	case int:
		return v == 0
	case []any:
		return len(v) == 0
	case map[string]any:
		return len(v) == 0
	}
	return false
}

// modelsRefresh 强制网关回源刷新模型列表，成功后连同新列表一起返回。
//
// 网关侧模型列表缓存 1h（刻意的：展示端点不该每次都打上游）。这里给运维一个按需
// 刷新入口，不缩短常态 TTL。刷新失败如实回错——不能让「没刷到」看起来像「已刷新」。
func (s *Server) modelsRefresh(w http.ResponseWriter, r *http.Request) {
	if s.gateway.Management == nil || !s.gateway.Management.ManagementEnabled() {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "message": "无法读取网关配置"})
		return
	}
	code, result := s.gateway.Management.Request(r.Context(), http.MethodPost, "/models/refresh", nil)
	// 两道判据都要看：HTTP 码（网关直接报错时是 503）与 ok 字段（信封里明确说不成功）。
	// 只看码会漏掉「200 但 ok=false」这种形态，把没刷到显示成已刷新。
	if code != 200 || !jsonBool(result["ok"], false) {
		message := gatewayEnvelopeError(result)
		if strings.Contains(message, "网关未响应") {
			message = "刷新失败：" + message
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "message": message})
		return
	}
	body := modelsPayload(s.gateway.Get(r.Context(), "/v1/models"))
	body["ok"] = true
	body["refreshed"] = true
	writeJSON(w, http.StatusOK, body)
}

// gatewayAdmin 经管理通道调用网关内部接口，并统一把失败折成面板文案。
func (s *Server) gatewayAdmin(w http.ResponseWriter, r *http.Request, method, endpoint string, body any) {
	if s.gateway.Management == nil || !s.gateway.Management.ManagementEnabled() {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "message": "无法读取网关配置"})
		return
	}
	code, result := s.gateway.Management.Request(r.Context(), method, endpoint, body)
	if code != 200 {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "message": gatewayEnvelopeError(result)})
		return
	}
	writeJSON(w, http.StatusOK, result)
}

// taskLog 读某个任务的最近日志。
func (s *Server) taskLog(w http.ResponseWriter, r *http.Request) {
	key := r.URL.Query().Get("key")
	if !KnownTask(key) {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "lines": []any{},
			"message": formatMessage("未知任务：%s", key)})
		return
	}
	payload := s.gateway.Get(r.Context(), "/tasks/"+key+"/log")
	if payload == nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "lines": []any{},
			"message": "连不上网关或网关未接入排程"})
		return
	}
	lines, _ := payload["lines"].([]any)
	if lines == nil {
		lines = []any{}
	}
	// running/ended_at 透传给面板：前者决定弹窗是否继续自动刷新（任务在跑时日志还在长），
	// 后者给「最近一次」一个可读时刻。
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "lines": lines, "count": len(lines),
		"running": jsonBool(payload["running"], false),
		"endedAt": jsonString(payload["ended_at"]),
		"message": jsonString(jsonObject(payload["error"])["message"]),
	})
}

// logs 读网关日志并解析成请求行。
//
// 这里收到的 lines= 是「要多少条**请求**」，不是 docker --tail 的原始行数。日志里
// 请求行与 WARN/ERR 行混在一起、比例不固定，所以先多取一段原始行，解析后按请求行
// 裁到目标条数；日志本身不够长时就给多少算多少。
func (s *Server) logs(w http.ResponseWriter, r *http.Request) {
	want := 120
	if raw := r.URL.Query().Get("lines"); raw != "" {
		if number, err := strconv.Atoi(raw); err == nil {
			want = number
		}
	}
	if want < 1 {
		want = 1
	}
	if want > 1000 {
		want = 1000
	}
	ctx, cancel := contextWithTimeout(r.Context(), 40*time.Second)
	defer cancel()
	result := s.runner.Run(ctx, []string{"logs", "--tail", itoa(int64(RequestWindowSize(want))), s.containerName()}, 40*time.Second, nil)
	raw := strings.TrimSpace(strings.Join([]string{result.Stdout, result.Stderr}, "\n"))
	rows, other := ParseRequestLog(raw)
	if len(rows) > want {
		rows = rows[len(rows)-want:]
	}
	if rows == nil {
		rows = []RequestRow{}
	}
	if other == nil {
		other = []string{}
	}
	// 不回传原始日志文本：前端只用解析后的 rows/other，而这个界面每 5 秒轮询一次，
	// 原文白白占带宽。
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": result.Code == 0, "rows": rows, "other": other,
		"count": len(rows), "rc": result.Code,
	})
}

// updatePost 处理 /api/update/check 与 /api/update/apply。
func (s *Server) updatePost(w http.ResponseWriter, r *http.Request, path string, body map[string]any) {
	// 会话已由 servePost 验过，但热更新会替换正在运行的二进制，风险等级与密钥写操作
	// 相同，所以这里再核一次来源闸门（与 Python 版一致）。
	if !s.originOK(r) || r.Header.Get("X-Admin-Request") != "1" {
		writeJSON(w, http.StatusForbidden, map[string]any{"ok": false, "message": "请求来源无效，请从管理页面重新操作"})
		return
	}
	if message := unsupportedFields(body, "tag"); message != "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "message": "包含不支持的字段"})
		return
	}
	tag := jsonString(body["tag"])
	if _, present := body["tag"]; present {
		if len(tag) > 64 || !updateTagRe.MatchString(tag) {
			writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "message": "版本号格式不正确"})
			return
		}
	}
	endpoint := "/update/apply"
	if strings.HasSuffix(path, "/check") {
		endpoint = "/update/check"
	}
	payload := map[string]any{}
	if tag != "" {
		payload["tag"] = tag
	}
	s.gatewayAdmin(w, r, http.MethodPost, endpoint, payload)
}

// containerName 返回网关容器名。
func (s *Server) containerName() string {
	if name := strings.TrimSpace(s.opts.Container); name != "" {
		return name
	}
	return "workbuddy2api"
}
