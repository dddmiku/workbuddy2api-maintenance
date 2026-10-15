// remote.go — Trae Remote 会话端点（两步协议）的**主路径**实现。
//
// ═══ 架构对齐（2026-10-16 重写）═══
//
// 参考项目（autumnsentiment/Trae2api-cn）的 UPSTREAM_MODE 默认是 **remote**：
// 远程会话为主路径。这次重写把网关的次序对齐过来——ChatStream 优先走 Remote，
// 带客户端工具的请求按参考的 caller_tools_use_work 默认路由到 solo_work_remote，
// 纯文本请求走 solo_agent_remote（保有 1M max 档能力）。
//
// Remote 两步协议：
//	1. POST {base}/chat_sessions              建会话 → chat_session_id + message_id
//	2. GET  {base}/chat_sessions/{sid}/events?reply_to_message_id={mid}   收流
//	3. POST {base}/chat_sessions/{sid}/stop    释放槽位（尽力而为）
//
// ═══ 为什么 query 必须扁平化（3004 实测结论，2026-10-15）═══
//
// llm_utils_chat 对**工具交互轮次**有硬限制，超限返回 code 3004：
//
//	最近 100 条 / 293KB → 3004      最近 100 条 → Remote 成功，正文 794 字符
//	2 条 / 1600KB 纯文本  → 成功     （体积不是条件）
//	300 条 / 336KB 纯文本  → 成功     （条数不是条件）
//	120 条真实，两者都转纯文本 → 成功  ← 唯一通过的变体
//
// Remote 的 query 是扁平化文本——工具调用转成 "[Client Tool Result: ...]" 标记，
// 不带 tool_calls/tool_call_id 结构化字段，正好绕开这个限制。
//
// ═══ 代价（必须如实记录）═══
//
//  1. Remote 是自带沙箱的 agent：它有自己的工具（LS / Glob / Read / finish…），
//     自行执行。客户端工具以 opencode_tool_call 文本块协议注入 system prompt，
//     模型用文本块请求调用，由 API 调用方执行后在下一轮回填（见 prompt.go）。
//  2. query 有长度上限：上游把 query 当历史处理，超过约 500K 字符（实测 533K 起）
//     会**静默结束事件流**（无正文、无 done），必须裁剪。
package trae

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"workbuddy2api/internal/auth"
)

const (
	// RemoteBase 是 Remote 会话 API 的基址（与模型网关同主机，路径不同）。
	RemoteBase = GatewayBase + "/api/remote/v1"

	// remoteCreatePath 建会话；remoteEventsFmt 收流（路径含 sid）。
	remoteCreatePath = "/chat_sessions"
	remoteEventsFmt  = "/chat_sessions/%s/events?reply_to_message_id=%s"
	remoteStopFmt     = "/chat_sessions/%s/stop"

	// remoteModelsPath 网页端模型目录（manual 选模型需要 custom_model）。
	remoteModelsPath = "/models?functions=solo_agent_remote%2Csolo_work_remote%2Csolo_design_remote&show_custom_model=true"

	// remoteDefaultOrigin Remote 头族要求的 Origin/Referer（SOLO 网页端）。
	remoteDefaultOrigin = "https://solo.trae.cn"

	// remoteDefaultUserAgent SOLO 网页端的 UA。
	remoteDefaultUserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 " +
		"(KHTML, like Gecko) Chrome/149.0.0.0 Safari/537.36"

	// remoteQueryMaxChars 扁平化 query 的字符上限。
	//
	// 上游在 query 超过约 500K 字符时**静默结束事件流**（不报错、不给 done）。
	// 实测边界（2026-10-15，逐档 50K→716K）：512K 正常、533K 静默断流。
	// 取 460K 留约 10% 余量（query 之外还有 common_params、运行时提示等字段）。
	remoteQueryMaxChars = 460_000

	// remoteHistoryMaxMessages / remoteHistoryMaxChars 历史压缩的上限
	//（对齐参考 TRAE_REMOTE_MAX_MESSAGES=500 / TRAE_REMOTE_MAX_HISTORY_CHARS=480000）。
	remoteHistoryMaxMessages = 500
	remoteHistoryMaxChars    = 480_000

	// remoteFirstEventTimeout 首个模型事件的超时。
	//
	// 建会话后上游可能先排队（只发 heartbeat/queue 帧）再开始执行。参考默认 120s：
	// 大 prompt 的排队期真实存在，比 120s 短会把可成功的请求误判成失败。
	remoteFirstEventTimeout = 120_000_000_000 // 120s（纳秒）

	// remoteDefaultAgentType 纯文本请求的执行档（保有 1M max 档）。
	remoteDefaultAgentType = "solo_agent_remote"
	// remoteWorkAgentType 带客户端工具请求的执行档（Work 沙箱不做文件操作）。
	remoteWorkAgentType = "solo_work_remote"

	// max 模式六件套的默认值（对齐参考 DEFAULT_MAX_*）。
	defaultMaxContextTokens = 1_000_000
	defaultMaxPromptTokens  = 936_000
	defaultMaxOutputTokens  = 64_000
	defaultMaxModeType      = 1
)

// RemoteBaseVar 是出站实际使用的基址（测试用 httptest 改写；生产代码从不写它）。
var RemoteBaseVar = RemoteBase

// RemoteEnabled 报告是否允许 Remote 主路径（默认开）。
//
// 关掉它等于「全部回落到 llm_utils_chat」——保留开关是为了排障。
func RemoteEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("WB2API_TRAE_REMOTE"))) {
	case "0", "false", "no", "off":
		return false
	}
	return true
}

// remoteHeaders 构造 Remote 头族（网页端形态，与 llm_utils_chat 的 SOLO/IDE 指纹完全不同）。
func remoteHeaders(a *auth.Auth, stream bool) http.Header {
	h := http.Header{}
	h.Set("Authorization", "Cloud-IDE-JWT "+a.AccessToken)
	h.Set("Content-Type", "application/json")
	h.Set("X-Trae-Client-Type", "web")
	h.Set("X-Preferenced-Language", "zh-CN")
	h.Set("x-user-region", "CN")
	h.Set("Origin", remoteDefaultOrigin)
	h.Set("Referer", remoteDefaultOrigin+"/")
	h.Set("User-Agent", remoteDefaultUserAgent)
	if stream {
		h.Set("Accept", "text/event-stream")
	} else {
		h.Set("Accept", "application/json")
	}
	return h
}

// remoteInitialMessage Remote 建会话体里的 initial_message（map[string]any 形态，
// 字段随策略可选——manual 带 custom_model 六件套，auto 不带）。
type remoteInitialMessage struct {
	ChatSessionID string          `json:"chat_session_id"`
	Content       []any           `json:"content"`
	Query         string          `json:"query"`
	ModelName     string          `json:"model_name"`
	AgentType     string          `json:"agent_type"`
	AgentID       string          `json:"agent_id"`
	Strategy      string          `json:"model_selection_strategy"`
	CommonParams  string          `json:"common_params"`

	ModelConfigSource *int64          `json:"model_config_source,omitempty"`
	ModelIsPreset     *bool           `json:"model_is_preset,omitempty"`
	ModelProvider     *string         `json:"model_provider,omitempty"`
	CustomModel       map[string]any  `json:"custom_model,omitempty"`

	ModelAutoSelection map[string]any `json:"model_auto_selection,omitempty"`
	ModeType           *int64         `json:"mode_type,omitempty"`
	ContextWindowSize  *int64         `json:"context_window_size,omitempty"`
	PromptMaxTokens    *int64         `json:"prompt_max_tokens,omitempty"`
	MaxTokens          *int64         `json:"max_tokens,omitempty"`
}

type remoteCreateBody struct {
	Mode              string               `json:"mode"`
	EnvironmentID     string               `json:"environment_id"`
	InitialMessage    remoteInitialMessage `json:"initial_message"`
	Env               string               `json:"env"`
	AutoCreateProject bool                 `json:"auto_create_project"`
	Origin            string               `json:"origin"`
}

// remoteSession 一次 Remote 会话的标识。
type remoteSession struct {
	id        string
	messageID string
}

// remoteAgentTypeFor 决定一次请求的执行档。
//
// 对齐参考 run_remote_session 的 caller_tools_use_work 默认（=1）：
// 带客户端工具的请求路由到 solo_work_remote——Agent 档的沙箱会自己动文件/终端
// 然后只回一句成功，工具语义错位；Work 档把客户端声明的工具保持为调用。
// 纯文本请求走 solo_agent_remote（1M max 档只对它生效）。
// 模型名 work/auto-work/solo-work 显式要求 Work 档。
func remoteAgentTypeFor(model string, withTools bool) string {
	switch strings.ToLower(strings.TrimSpace(model)) {
	case "work", "auto-work", "solo-work":
		return remoteWorkAgentType
	}
	if withTools {
		return remoteWorkAgentType
	}
	return remoteDefaultAgentType
}

// buildCommonParams 组装 common_params（JSON 字符串）。
//
// 对齐参考 trae_remote_client.common_params 的 CN code 档。
func buildCommonParams(sessionID string) string {
	params := map[string]any{
		"language":        "zh-cn",
		"app_language":    "zh-CN",
		"quality":         "stable",
		"app_version":     "1.0.0.1229",
		"web_id":          "",
		"user_identity":   "Free",
		"is_freshman":     "0",
		"biz_user_id":     "",
		"user_unique_id":  "",
		"scope":           "marscode-cn",
		"tenant":          "marscode",
		"region":          "cn",
		"aiRegion":        "cn",
		"is_privacy_mode": 0,
		"privacy_mode":    "off",
		"solo_chat_mode":  "code",
	}
	if sessionID != "" {
		params["biz_session_id"] = sessionID
	}
	encoded, err := json.Marshal(params)
	if err != nil {
		return "{}"
	}
	return string(encoded)
}

// buildRemoteBody 组装 Remote 建会话请求体。
//
// 返回 (body, 执行档, 是否 max 模式, 错误)。
// 步骤对齐参考 create_session：
//  1. 从出站请求体取 messages，注入运行时提示（prompt.go）；
//  2. 有界历史压缩 + query 裁剪；
//  3. manual 选模型时从目录解析 custom_model 并绑定（_bound_model_name 口径）；
//  4. max 请求（模型名带 -max 后缀且目录标 max_mode）补六件套。
func buildRemoteBody(ctx context.Context, a *auth.Auth, prepared []byte) ([]byte, string, bool, error) {
	var src map[string]any
	if err := json.Unmarshal(prepared, &src); err != nil {
		return nil, "", false, fmt.Errorf("trae remote: 请求体解析失败：%w", err)
	}
	rawMessages, _ := src["messages"].([]any)
	messages := make([]map[string]any, 0, len(rawMessages))
	for _, raw := range rawMessages {
		if m, ok := raw.(map[string]any); ok {
			messages = append(messages, m)
		}
	}
	model, _ := src["model"].(string)
	if model == "" {
		model = "glm-5.2"
	}

	prepared2, withTools := prepareRemoteMessages(messages, src)
	compacted := compactRemoteHistory(prepared2, remoteHistoryMaxMessages, remoteHistoryMaxChars)
	bounded, dropped := boundedRemoteQuery(compacted, remoteQueryMaxChars)
	if dropped > 0 {
		remoteTrimLog(dropped)
	}
	query := flattenQuery(bounded)

	agentType := remoteAgentTypeFor(model, withTools)
	// auto / work 是路由 id；具名模型走 manual。
	strategy := "auto"
	modelName := ""
	if !strings.EqualFold(strings.TrimSpace(model), "auto") &&
		!strings.EqualFold(strings.TrimSpace(model), "work") {
		strategy = "manual"
		modelName = strings.TrimSpace(model)
	}

	msg := remoteInitialMessage{
		ChatSessionID: "",
		Content:       []any{},
		Query:         query,
		ModelName:     modelName,
		AgentType:     agentType,
		AgentID:       agentType,
		Strategy:      strategy,
		CommonParams:  buildCommonParams(""),
	}

	maxApplied := false
	if strategy == "manual" {
		models, cerr := FetchCatalog(ctx, a)
		if cerr == nil {
			if resolved := ResolveFromCatalog(models, modelName); resolved != "" && resolved != modelName {
				modelName = resolved
			}
		} else {
			modelName = ResolveModelName(modelName)
		}
		msg.ModelName = modelName
		if custom, ok := customModelFor(models, modelName, agentType, cerr == nil); ok {
			msg.CustomModel = custom
			source := int64(1)
			if v, ok := asFloat(custom["config_source"]); ok {
				source = int64(v)
			}
			preset := true
			if v, ok := custom["is_preset"].(bool); ok {
				preset = v
			}
			provider, _ := custom["provider"].(string)
			msg.ModelConfigSource = &source
			msg.ModelIsPreset = &preset
			msg.ModelProvider = &provider
			// max 模式：模型名带 -max 后缀且目录标 max_mode（仅 Agent 档）。
			if strings.HasSuffix(strings.ToLower(modelName), "-max") && agentType == remoteDefaultAgentType {
				if enriched, ok := maxModeCustomModel(custom); ok {
					msg.CustomModel = enriched
					fields := maxModeFields(enriched)
					msg.ModelAutoSelection = fields["model_auto_selection"].(map[string]any)
					strategy := int64(defaultMaxModeType)
					msg.ModeType = &strategy
					msg.Strategy = "max"
					if v, ok := fields["mode_type"].(int64); ok {
						msg.ModeType = &v
					}
					if v, ok := fields["context_window_size"].(int64); ok {
						msg.ContextWindowSize = &v
					}
					if v, ok := fields["prompt_max_tokens"].(int64); ok {
						msg.PromptMaxTokens = &v
					}
					if v, ok := fields["max_tokens"].(int64); ok {
						msg.MaxTokens = &v
					}
					maxApplied = true
				}
			}
		}
	}

	body := remoteCreateBody{
		Mode:              "code",
		EnvironmentID:     "default",
		Env:               "remote",
		AutoCreateProject: false,
		Origin:            "web",
		InitialMessage:    msg,
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		return nil, "", false, err
	}
	return encoded, agentType, maxApplied, nil
}

// remoteTrimLog 裁剪日志（单独函数便于测试覆盖）。
func remoteTrimLog(dropped int) {
	logRemoteTrim(dropped)
}

// customModelFor 从目录（或静态别名表）取该模型的 custom_model 对象。
//
// 目录可用时按 config_name 匹配并补 config_name/model_name/config_source/
// provider/multimodal 字段（对齐参考 _build_web_model_config）。
func customModelFor(models []CatalogModel, name string, agentType string, catalogOK bool) (map[string]any, bool) {
	if catalogOK && len(models) > 0 {
		for _, m := range models {
			if strings.EqualFold(m.ConfigName, name) {
				return map[string]any{
					"name":             m.ConfigName,
					"config_name":      m.ConfigName,
					"model_name":       m.ConfigName,
					"display_name":     m.DisplayName,
					"config_source":    1,
					"provider":         "",
					"multimodal":       m.Multimodal,
					"max_mode":         m.MaxMode,
					"custom":           false,
					"is_preset":        true,
					"use_remote_service": true,
				}, true
			}
		}
	}
	// 目录不可用时按静态别名表判断是否已知模型。
	if resolved := ResolveModelName(name); resolved != "" && !strings.EqualFold(resolved, name) {
		return map[string]any{
			"name":          resolved,
			"config_name":   resolved,
			"model_name":    resolved,
			"config_source": 1,
			"is_preset":     true,
			"custom":        false,
			"use_remote_service": true,
		}, true
	}
	if staticModelKnown(name) {
		return map[string]any{
			"name":          name,
			"config_name":   name,
			"model_name":    name,
			"config_source": 1,
			"is_preset":     true,
			"custom":        false,
			"use_remote_service": true,
		}, true
	}
	return nil, false
}

// maxModeCustomModel 把目录模型扩成 max 档 custom_model（对齐参考 _max_mode_custom_model）。
//
// 只在目录标了 max_mode 时生效——绝不凭空造 1M 上限（模型不支持会被上游拒）。
func maxModeCustomModel(custom map[string]any) (map[string]any, bool) {
	if flag, ok := custom["max_mode"].(bool); !ok || !flag {
		return nil, false
	}
	maxContext := int64(defaultMaxContextTokens)
	promptMax := int64(defaultMaxPromptTokens)
	outputMax := int64(defaultMaxOutputTokens)
	enriched := make(map[string]any, len(custom)+8)
	for k, v := range custom {
		enriched[k] = v
	}
	enriched["max_mode"] = true
	enriched["context_window_size"] = map[string]any{"default": 200_000, "max": []any{maxContext}}
	enriched["context_window_tokens"] = map[string]any{"dev": 200_000, "max": maxContext}
	enriched["prompt_max_tokens"] = promptMax
	enriched["max_tokens"] = outputMax
	enriched["max_turn"] = 500
	enriched["max_turns"] = map[string]any{"default": 500, "max": 500}
	enriched["features"] = map[string]any{
		"context_windows": map[string]any{
			"enable": true,
			"data": map[string]any{
				"dev_context":      200_000,
				"max_context":      maxContext,
				"max_context_list": []any{maxContext},
				"dev_turns":        500,
				"max_turns":        500,
			},
		},
	}
	return enriched, true
}

// maxModeFields max 档六件套（对齐参考 _max_mode_fields）。
func maxModeFields(custom map[string]any) map[string]any {
	maxContext := int64(defaultMaxContextTokens)
	promptMax := int64(defaultMaxPromptTokens)
	outputMax := int64(defaultMaxOutputTokens)
	if v, ok := asFloat(custom["context_window_tokens"]); ok {
		_ = v
	}
	return map[string]any{
		"model_auto_selection": map[string]any{
			"strategy":                "max",
			"fallback_to_advance_model": nil,
			"entitlement_id":          nil,
		},
		"model_selection_strategy": "max",
		"mode_type":                int64(defaultMaxModeType),
		"context_window_size":      maxContext,
		"prompt_max_tokens":        promptMax,
		"max_tokens":               outputMax,
	}
}

// createRemoteSession 建会话（两步协议的第 1 步）。
func createRemoteSession(ctx context.Context, a *auth.Auth, body []byte) (remoteSession, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		RemoteBaseVar+remoteCreatePath, bytes.NewReader(body))
	if err != nil {
		return remoteSession{}, err
	}
	for k, vs := range remoteHeaders(a, false) {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	resp, err := remoteHTTPClient().Do(req)
	if err != nil {
		return remoteSession{}, fmt.Errorf("trae remote 建会话：%w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return remoteSession{}, fmt.Errorf("trae remote 建会话读取：%w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return remoteSession{}, &remoteHTTPError{status: resp.StatusCode, body: truncate(string(raw), 200)}
	}
	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil {
		return remoteSession{}, fmt.Errorf("trae remote 建会话响应解析：%w", err)
	}
	if code, ok := asFloat(payload["code"]); ok && code != 0 {
		message, _ := payload["message"].(string)
		return remoteSession{}, &remoteBizError{code: int(code), message: message}
	}
	data, _ := payload["data"].(map[string]any)
	if data == nil {
		data = payload
	}
	sid, _ := data["chat_session_id"].(string)
	mid, _ := data["message_id"].(string)
	if sid == "" || mid == "" {
		return remoteSession{}, fmt.Errorf("trae remote 建会话响应缺 id：%s", truncate(string(raw), 200))
	}
	return remoteSession{id: sid, messageID: mid}, nil
}

// remoteHTTPError / remoteBizError 建会话失败的两类形态。
type remoteHTTPError struct {
	status int
	body   string
}

func (e *remoteHTTPError) Error() string {
	return fmt.Sprintf("trae remote 建会话 HTTP %d：%s", e.status, e.body)
}

type remoteBizError struct {
	code    int
	message string
}

func (e *remoteBizError) Error() string {
	return fmt.Sprintf("trae remote 建会话 code %d：%s", e.code, e.message)
}

// remoteHTTPClient Remote 出站共享一个 client（连接复用）。
func remoteHTTPClient() *http.Client {
	return &http.Client{Transport: chatTransport()}
}

// stopRemoteSession 主动结束会话（尽力而为，失败不影响结果）。
//
// 上游对会话有并发槽位，不结束会占着槽位。
func stopRemoteSession(ctx context.Context, a *auth.Auth, sid string) {
	if sid == "" {
		return
	}
	body, _ := json.Marshal(map[string]string{"chat_session_id": sid})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		fmt.Sprintf(RemoteBaseVar+remoteStopFmt, sid), bytes.NewReader(body))
	if err != nil {
		return
	}
	for k, vs := range remoteHeaders(a, false) {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	client := &http.Client{Timeout: 10 * time.Second, Transport: chatTransport()}
	resp, err := client.Do(req)
	if err != nil {
		return
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	_ = resp.Body.Close()
}

// openRemoteEvents 收流（两步协议的第 2 步）。
//
// 首事件超时在读取侧实现（remoteEventStream 的 first-event 判定），
// 这里只负责建立连接。
func openRemoteEvents(ctx context.Context, a *auth.Auth, session remoteSession) (io.ReadCloser, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		fmt.Sprintf(RemoteBaseVar+remoteEventsFmt, session.id, session.messageID), nil)
	if err != nil {
		return nil, err
	}
	for k, vs := range remoteHeaders(a, true) {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	req.Header.Set("Accept", "text/event-stream")
	resp, err := remoteHTTPClient().Do(req)
	if err != nil {
		return nil, fmt.Errorf("trae remote 收流：%w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		_ = resp.Body.Close()
		return nil, fmt.Errorf("trae remote 收流 HTTP %d：%s", resp.StatusCode, truncate(string(raw), 200))
	}
	return resp.Body, nil
}

// strconv 占位引用（构建未用到的包在这里集中消化，避免 import 悬空）。
var _ = time.Second

// logRemoteTrim 裁剪日志钩子（生产写日志；测试静音）。
var logRemoteTrim = func(dropped int) {
	log.Printf("INFO: [trae] Remote query 超限，已从最早丢弃 %d 条消息", dropped)
}

// ── Remote 事件流 → llm_utils_chat 形态的转译 ───────────────────────────────
//
// 与旧版（HEAD 版本）一致的策略：把 Remote 事件改写成 translator 已认识的
// output/token_usage/done/error 事件，再交给 stream.go 的既有转译器——工具合并、
// think 拆分、终态归一、usage 映射全部复用，不为 Remote 另写一套。
//
// 本次重写吸收了参考实现 translate_web_events 的三个修正：
//  1. plan_item 的正文优先取**显式 content**（公开答案），thought 只是推理快照——
//     旧版把 thought 当正文，会把「The user wants…」式的过程叙述当答案交付；
//  2. tool_call_info.name == "finish" 的 params.summary 是最终答案快照，收尾时
//     若正文为空（或只是它的前缀），用它补齐——纯工具轮次的会话正文全在 summary 里；
//  3. Remote 的工具活动（沙箱工具）仍**不透传**（实测口径，见 remoteEventConverter
//     注释），客户端工具请求已由 opencode_tool_call 文本协议从正文里抽取
//     （stream.go 的 toolBlockPairs + toolAccumulator 天然支持）。

// translateRemoteEvents 把 Remote 事件流改写成 llm_utils_chat 形态的 SSE。
func translateRemoteEvents(upstream io.ReadCloser, model string) io.ReadCloser {
	pr, pw := io.Pipe()
	go func() {
		var err error
		defer func() {
			_ = upstream.Close()
			_ = pw.CloseWithError(err)
		}()
		reader := bufio.NewReaderSize(upstream, 1<<16)
		conv := &remoteEventConverter{}
		var eventName string
		var dataBuf strings.Builder
		flush := func() error {
			name := strings.TrimSpace(eventName)
			payload := strings.TrimSpace(dataBuf.String())
			eventName, dataBuf = "", strings.Builder{}
			if name == "" && payload == "" {
				return nil
			}
			if payload == "[DONE]" {
				_, werr := io.WriteString(pw, "event: done\ndata: {\"finish_reason\":\"stop\"}\n\n")
				return werr
			}
			converted, ok := conv.convert(name, payload)
			if !ok {
				return nil
			}
			_, werr := io.WriteString(pw, converted)
			return werr
		}
		for {
			line, rerr := reader.ReadString('\n')
			if line != "" {
				trimmed := strings.TrimRight(line, "\r\n")
				switch {
				case strings.HasPrefix(trimmed, "event:"):
					eventName = strings.TrimSpace(trimmed[len("event:"):])
				case strings.HasPrefix(trimmed, "data:"):
					if dataBuf.Len() > 0 {
						dataBuf.WriteByte('\n')
					}
					dataBuf.WriteString(strings.TrimSpace(trimmed[len("data:"):]))
				case trimmed == "":
					if werr := flush(); werr != nil {
						err = werr
						return
					}
				}
			}
			if rerr != nil {
				if rerr != io.EOF {
					err = rerr
					return
				}
				if werr := flush(); werr != nil {
					err = werr
				}
				// flush 之后补终态与（若有）finish summary（见 conv.finish 注释）。
				if tail := conv.finish(); tail != "" {
					if _, werr := io.WriteString(pw, tail); werr != nil {
						err = werr
					}
				}
				return
			}
		}
	}()
	return pr
}

// remoteEventConverter 把 Remote 事件转成 llm_utils_chat 形态。
//
// **Remote 的沙箱工具活动一律不透传**（2026-10-15 实测后定的）。
//
// 为什么：Remote 是一个**自带沙箱的 agent**——它有自己的工具（实测 LS /
// EnvironmentSetup / finish / Glob / Read…），参数以增量片段流式给出，然后它
// **自己执行**并继续下一轮。它不是「向客户端请求执行工具」的协议。
// 把它当客户端工具调用透传会同时踩两个坑：
//  1. 参数永远「不完整」——客户端视角下这次调用没有闭合（Remote 不需要客户端回结果），
//     网关的校验器因此报 "upstream ended with incomplete or invalid tool arguments"，
//     整个回答变成失败。
//  2. 语义错位——客户端会去执行一个**已经被 Remote 执行过**的工具（双重执行），
//     而且 Remote 的沙箱工具客户端根本没有。
//
// 例外：opencode_tool_call 文本块是**客户端工具协议**（prompt.go 注入的），模型把它
// 写在正文里、由 API 调用方执行——那种块留在正文里，stream.go 的 translator 会从
// 文本中抽取并转成标准 tool_calls 帧。
type remoteEventConverter struct {
	// toolRounds 观测到的沙箱工具轮次（仅用于日志，不透传）。
	toolRounds int
	// seenTools 已见过的工具名（同一工具重复出现只计一次轮次）。
	seenTools map[string]bool
	// finishSummary tool_call_info.name == "finish" 的 params.summary。
	//
	// 纯工具轮次（或 thought 全是叙述）的会话里，公开答案只出现在 finish 的
	// summary 参数里。收尾时若 thought 累计正文为空或是 summary 的前缀，用它补齐。
	finishSummary string
}

// convert 把一个 Remote 事件改写成 llm_utils_chat 形态的 SSE 文本。
// 返回 ok=false 表示该事件不携带正文，应丢弃。
func (c *remoteEventConverter) convert(name, payload string) (string, bool) {
	switch name {
	case "plan_item":
		var obj map[string]any
		if json.Unmarshal([]byte(payload), &obj) != nil {
			return "", false
		}
		out := map[string]any{}
		// 正文优先取显式 content（公开答案）；没有 content 才用 thought
		// （旧布局：thought 兼作答案快照，如 thought="pong"）。
		content := webContentText(obj["content"])
		if content != "" {
			out["response"] = content
		} else if thought, ok := obj["thought"].(string); ok && thought != "" {
			out["response"] = thought
		}
		if reasoning := webPlanReasoning(obj); reasoning != "" {
			out["reasoning_content"] = reasoning
		}
		// finish 工具的 summary 是最终答案快照，收尾时兜底（见 finishSummary）。
		if summary := webFinishSummary(obj); summary != "" {
			c.finishSummary = summary
		}
		// 沙箱工具活动只计数、不透传（理由见本类型注释）。
		if info, ok := obj["tool_call_info"].(map[string]any); ok {
			if toolName, _ := info["name"].(string); toolName != "" && toolName != "finish" {
				if c.seenTools == nil {
					c.seenTools = map[string]bool{}
				}
				if !c.seenTools[toolName] {
					c.seenTools[toolName] = true
					c.toolRounds++
					log.Printf("INFO: [trae] Remote 沙箱执行了工具 %s（第 %d 轮，不透传给客户端）",
						toolName, c.toolRounds)
				}
			}
		}
		if len(out) == 0 {
			return "", false
		}
		encoded, err := json.Marshal(out)
		if err != nil {
			return "", false
		}
		return "event: output\ndata: " + string(encoded) + "\n\n", true

	case "heartbeat", "keepalive", "ping":
		// 心跳转发（translator 会转成下游保活注释行）：Remote agent 思考期
		// 只有这些帧，静默丢弃会让客户端把流判死（参考实测 zcode 的行为）。
		return ": relay-keepalive\n\n", true

	case "token_usage":
		var obj map[string]any
		if json.Unmarshal([]byte(payload), &obj) != nil {
			return "", false
		}
		// Remote 把 usage 平铺在顶层（prompt_tokens/completion_tokens/...），
		// llm_utils_chat 的 translator 期望嵌套在 usage 字段里。
		usage := map[string]any{}
		for _, key := range []string{"prompt_tokens", "completion_tokens", "total_tokens",
			"cache_read_input_tokens", "cache_creation_input_tokens", "reasoning_tokens"} {
			if v, ok := obj[key]; ok {
				usage[key] = v
			}
		}
		if len(usage) == 0 {
			return "", false
		}
		encoded, err := json.Marshal(map[string]any{"usage": usage})
		if err != nil {
			return "", false
		}
		return "event: token_usage\ndata: " + string(encoded) + "\n\n", true

	case "done":
		// Remote 的 done 给的是 status（completed/…），不是 finish_reason。
		return "event: done\ndata: {\"finish_reason\":\"stop\"}\n\n", true

	case "error":
		return "event: error\ndata: " + payload + "\n\n", true
	}
	// message / assistant_message / response / text / output：显式正文事件。
	switch name {
	case "message", "assistant_message", "response", "text", "output":
		var obj map[string]any
		if json.Unmarshal([]byte(payload), &obj) != nil {
			return "", false
		}
		out := map[string]any{}
		if text := webMessageText(obj); text != "" {
			out["response"] = text
		}
		if len(out) == 0 {
			return "", false
		}
		encoded, err := json.Marshal(out)
		if err != nil {
			return "", false
		}
		return "event: output\ndata: " + string(encoded) + "\n\n", true
	}
	return "", false
}

// finish 流结束时补一帧 finish summary（若有且正文可能没用上）。
//
// 为什么放在收尾而不是 done 时：done 事件的 status 不带 summary，而 summary 在
// 最后一个 plan_item 里；转译层无法预知后续还有没有正文，只能在 EOF 时兜底。
// 输出形态是 output 事件（translator 的累计合并会自动去重——summary 若与已交付
// 正文相同则前缀合并后 delta 为零，不会重复下发）。
func (c *remoteEventConverter) finish() string {
	if c.finishSummary == "" {
		return ""
	}
	encoded, err := json.Marshal(map[string]any{"response": c.finishSummary})
	if err != nil {
		return ""
	}
	return "event: output\ndata: " + string(encoded) + "\n\n"
}

// webContentText 取 plan_item/message 的 content 字段（字符串或块数组）。
//
// 对齐参考 _web_message_text：块数组里 type 为 reasoning/thinking 的块跳过
// （那是思考，不是公开答案）。
func webContentText(value any) string {
	switch typed := value.(type) {
	case string:
		return typed
	case []any:
		var out []string
		for _, raw := range typed {
			switch block := raw.(type) {
			case string:
				out = append(out, block)
			case map[string]any:
				if kind, _ := block["type"].(string); strings.EqualFold(kind, "reasoning") ||
					strings.EqualFold(kind, "thinking") || strings.EqualFold(kind, "reasoning_text") {
					continue
				}
				if text, ok := block["text"].(string); ok && text != "" {
					out = append(out, text)
				} else if text, ok := block["content"].(string); ok && text != "" {
					out = append(out, text)
				}
			}
		}
		return strings.Join(out, "")
	}
	return ""
}

// webMessageText 从 message 形态事件里取可见正文（对齐参考 _web_message_text）。
func webMessageText(obj map[string]any) string {
	for _, key := range []string{"message", "agent_message", "assistant_message"} {
		if nested, ok := obj[key].(map[string]any); ok {
			obj = nested
			break
		}
	}
	if text := webContentText(obj["content"]); text != "" {
		return text
	}
	for _, key := range []string{"text", "response", "answer", "output"} {
		switch typed := obj[key].(type) {
		case string:
			if typed != "" {
				return typed
			}
		case []any:
			if text := webContentText(typed); text != "" {
				return text
			}
		}
	}
	return ""
}

// webPlanReasoning 取 plan_item 的推理文本（reasoning_content 优先，回落 thought）。
//
// 对齐参考 _web_plan_reasoning：带显式 reasoning_content 的布局里 thought 是
// 答案快照；旧布局（无 reasoning_content 键）里 thought 兼作推理轨迹。
func webPlanReasoning(obj map[string]any) string {
	_, hasSplit := obj["reasoning_content"]
	if !hasSplit {
		if _, hasCamel := obj["reasoningContent"]; !hasCamel {
			return ""
		}
	}
	if value, ok := obj["reasoning_content"].(string); ok && value != "" {
		return value
	}
	if value, ok := obj["reasoningContent"].(string); ok && value != "" {
		return value
	}
	return ""
}

// webFinishSummary 取 finish 工具的 summary 参数（最终答案快照）。
func webFinishSummary(obj map[string]any) string {
	info, ok := obj["tool_call_info"].(map[string]any)
	if !ok {
		info, ok = obj["toolCallInfo"].(map[string]any)
	}
	if !ok {
		return ""
	}
	if name, _ := info["name"].(string); name != "finish" {
		return ""
	}
	params, ok := info["params"].(map[string]any)
	if !ok {
		return ""
	}
	summary, _ := params["summary"].(string)
	return summary
}
