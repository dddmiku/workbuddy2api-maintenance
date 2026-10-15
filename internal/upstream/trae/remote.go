// remote.go — Trae Remote 会话端点（两步协议）的回落实现。
//
// ═══ 为什么必须有这条路（2026-10-15 实测结论）═══
//
// llm_utils_chat（本包主链路）对**工具交互轮次**有硬限制，超限后上游返回
//
//	{"code":3004,"message":"We're sorry, your requests have exceeded the rate limit..."}
//
// 注意这不是体积问题，也不是调用频次问题。同一份真实会话（1047 条消息）实测：
//
//	最近 100 条 / 293KB → 3004      最近 100 条 → Remote 成功，正文 794 字符
//	最近 120 条 / 311KB → 3004      最近 120 条 → Remote 成功，正文 627 字符
//	最近 150 条 / 332KB → 3004      最近 150 条 → Remote 成功，正文 588 字符
//	最近 200 条 / 396KB → 3004      最近 200 条 → Remote 成功，正文 310 字符
//
// 而对照组说明触发维度就是工具轮次：
//
//	2 条消息 / 1600KB（纯文本）        → 成功   （体积不是触发条件）
//	300 条消息 / 336KB（合成纯文本）    → 成功   （条数本身不是触发条件）
//	120 条真实消息，去掉 tools 定义     → 3004
//	120 条真实消息，只丢 tool_calls     → 3004
//	120 条真实消息，只丢 tool_call_id   → 3004
//	120 条真实消息，两者都转成纯文本     → 成功   ← 唯一通过的变体
//
// 即：**只要请求体里还带 tool_calls 或 tool_call_id，且工具轮次足够多，就被拒**。
// 参考实现（autumnsentiment/Trae2api-cn）把这条链路列为默认端点（UPSTREAM_MODE=remote），
// 并且它的 query 是**扁平化文本**——工具调用被转成 `[Client Tool Result: ...]` 标记，
// 不带 tool_calls/tool_call_id 字段，正好绕开这个限制。
//
// ═══ 为什么不能靠「裁剪历史」绕过（同样实测）═══
//
// 真实 Claude Code 会话的**尾部全是工具轮次**（前 120 条里 50 条带 tool_calls、
// 65 条 role=tool，占 96%），所以裁到多小都没用：
//
//	最近 60 条 / 381KB → 3004
//	最近 80 条 / 409KB → 3004
//	最近 100 条 / 424KB → 3004
//	最近 110 条 / 436KB → 3004
//
// 裁剪只在「旧历史多、新历史是纯对话」的会话上有效；工具驱动型会话必须换端点。
//
// ═══ 代价（必须如实记录）═══
//
// Remote 是**两步协议**（POST 建会话 + GET 收流），且 query 是扁平化文本，因此：
//
//  1. **工具轮次由 Remote 的沙箱完成，客户端看不到也无法参与**。Remote 是一个
//     自带沙箱的 agent，它有自己的工具（实测 LS / EnvironmentSetup / finish /
//     Glob / Read…），会自行执行并继续下一轮——它**不是**「向客户端请求执行工具」
//     的协议。本实现因此**不透传**它的工具调用（理由见 remoteEventConverter 注释），
//     客户端只拿到最终正文。参考实现 README 也实测 glm-5.3 在 Remote 上只有 6/7
//     工具能力（不支持并行调用），而 llm_utils_chat 是 7/7。
//  2. **query 有长度上限**：上游把 query 当历史处理，超过约 500K 字符（实测 533K
//     字符起）会**静默结束事件流**（无正文、无 done），必须裁剪。
//
// 所以它是**回落**而不是替换：短会话/工具不密集时仍走 llm_utils_chat（工具能力完整）。
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

	"workbuddy2api/internal/auth"
)

const (
	// RemoteBase 是 Remote 会话 API 的基址（与模型网关同主机，路径不同）。
	RemoteBase = GatewayBase + "/api/remote/v1"

	// remoteCreatePath 建会话；remoteEventsFmt 收流（路径含 sid）。
	remoteCreatePath = "/chat_sessions"
	remoteEventsFmt  = "/chat_sessions/%s/events?reply_to_message_id=%s"
	remoteStopFmt    = "/chat_sessions/%s/stop"

	// remoteDefaultOrigin Remote 头族要求的 Origin/Referer（SOLO 网页端）。
	remoteDefaultOrigin = "https://solo.trae.cn"

	// remoteDefaultUserAgent SOLO 网页端的 UA。
	remoteDefaultUserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 " +
		"(KHTML, like Gecko) Chrome/149.0.0.0 Safari/537.36"

	// remoteQueryMaxChars 扁平化 query 的字符上限。
	//
	// 上游在 query 超过约 500K 字符时**静默结束事件流**（不报错、不给 done），
	// 所以必须主动裁剪。实测边界（2026-10-15，逐档 50K→716K）：
	//
	//	512K 字符 → 正常（done + 正文）
	//	533K 字符 → 静默断流（3 帧、无 done）
	//
	// 取 460K 留约 10% 余量：query 之外还有 common_params、tools 描述等字段，
	// 且上游阈值可能随版本微调。参考实现默认 TRAE_REMOTE_QUERY_MAX_CHARS=480000
	// （其注释记的边界是「约 500K 字符」），这里更保守一档。
	remoteQueryMaxChars = 460_000
)

// RemoteBaseVar 是出站实际使用的基址。
//
// 与 TraeAPIHost 同一口径：测试用 httptest 临时改写它来覆盖完整链路
// （URL 拼接、两步协议、头族），生产代码从不写它。
var RemoteBaseVar = RemoteBase

// RemoteEnabled 报告是否允许 Remote 回落（默认开）。
//
// 关掉它等于「长会话直接失败」——保留开关是为了排障（怀疑 Remote 引入问题时
// 能一键回到只有 llm_utils_chat 的行为），而不是因为它有风险。
func RemoteEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("WB2API_TRAE_REMOTE"))) {
	case "0", "false", "no", "off":
		return false
	}
	return true
}

// remoteHeaders 构造 Remote 头族。
//
// 与 llm_utils_chat 的头族**完全不同**：那套 SOLO/IDE 指纹头（x-ide-version 等）
// 在 Remote 端点上没有意义，这里要的是网页端形态（X-Trae-Client-Type / Origin /
// 浏览器 UA）。混用会得到 401/404。
func remoteHeaders(a *auth.Auth) http.Header {
	h := http.Header{}
	h.Set("Authorization", "Cloud-IDE-JWT "+a.AccessToken)
	h.Set("Content-Type", "application/json")
	h.Set("X-Trae-Client-Type", "web")
	h.Set("X-Preferenced-Language", "zh-CN")
	h.Set("x-user-region", "CN")
	h.Set("Origin", remoteDefaultOrigin)
	h.Set("Referer", remoteDefaultOrigin+"/")
	h.Set("User-Agent", remoteDefaultUserAgent)
	h.Set("Accept", "application/json")
	return h
}

// remoteInitialMessage Remote 建会话体里的 initial_message。
type remoteInitialMessage struct {
	ChatSessionID string `json:"chat_session_id"`
	Content       []any  `json:"content"`
	Query         string `json:"query"`
	ModelName     string `json:"model_name"`
	AgentType     string `json:"agent_type"`
	AgentID       string `json:"agent_id"`
	// ModelSelectionStrategy "auto" 时不带 model_* 字段（上游按 auto 选模型）。
	ModelSelectionStrategy string `json:"model_selection_strategy"`
	CommonParams           string `json:"common_params"`
}

type remoteCreateBody struct {
	Mode              string               `json:"mode"`
	EnvironmentID     string               `json:"environment_id"`
	InitialMessage    remoteInitialMessage `json:"initial_message"`
	Env               string               `json:"env"`
	AutoCreateProject bool                 `json:"auto_create_project"`
	Origin            string               `json:"origin"`
}

// buildRemoteQuery 把 llm_utils_chat 形态的请求体扁平化成 Remote 的 query 字符串。
//
// 形态对齐参考实现 trae_client.flatten_query：
//   - system → "[System]\n<text>"
//   - assistant → "[Assistant]\n<text>"，工具调用追加 "[tool_call id=... name=...] <args>"
//   - tool 角色 → "[Client Tool Result]\n<text>"，追加 "[tool_result id=...]"
//
// **关键**：tool_calls / tool_call_id 只作为**文本**出现，不保留为结构化字段。
// 这正是绕开 3004 的原因（见本文件头部实测记录）。
func buildRemoteQuery(messages []map[string]any) string {
	var parts []string
	for _, m := range messages {
		role, _ := m["role"].(string)
		text := contentToText(m["content"])
		if calls, ok := m["tool_calls"].([]any); ok && len(calls) > 0 {
			var seg []string
			for _, raw := range calls {
				call, _ := raw.(map[string]any)
				if call == nil {
					continue
				}
				fn, _ := call["function"].(map[string]any)
				id, _ := call["id"].(string)
				name, _ := fn["name"].(string)
				args, _ := fn["arguments"].(string)
				if len(args) > 1500 {
					args = args[:1500]
				}
				seg = append(seg, fmt.Sprintf("[tool_call id=%s name=%s] %s", id, name, args))
			}
			if len(seg) > 0 {
				text = strings.TrimRight(text+"\n"+strings.Join(seg, "\n"), "\n")
			}
		}
		if id, _ := m["tool_call_id"].(string); id != "" {
			text = strings.TrimRight(text+"\n[tool_result id="+id+"]", "\n")
		}
		switch role {
		case "system":
			parts = append(parts, "[System]\n"+text)
		case "assistant":
			parts = append(parts, "[Assistant]\n"+text)
		case "tool":
			parts = append(parts, "[Client Tool Result]\n"+text)
		default:
			parts = append(parts, text)
		}
	}
	payload := []map[string]any{{"type": "text", "data": map[string]any{"content": strings.Join(parts, "\n\n")}}}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return `[{"type":"text","data":{"content":""}}]`
	}
	return string(encoded)
}

// contentToText 把一条消息的 content 压成纯文本（块数组取 text 块）。
func contentToText(value any) string {
	switch typed := value.(type) {
	case string:
		return typed
	case []any:
		var out []string
		for _, raw := range typed {
			block, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			if text, ok := block["text"].(string); ok && text != "" {
				out = append(out, text)
			}
		}
		return strings.Join(out, "\n")
	}
	return ""
}

// buildRemoteBody 组装 Remote 建会话请求体，并按 remoteQueryMaxChars 裁剪历史。
//
// 返回 (body, 保留的消息数, 丢弃的消息数)。
func buildRemoteBody(prepared []byte, requestID string) ([]byte, int, int, error) {
	var src map[string]any
	if err := json.Unmarshal(prepared, &src); err != nil {
		return nil, 0, 0, fmt.Errorf("trae remote: 请求体解析失败：%w", err)
	}
	model, _ := src["model"].(string)
	if model == "" {
		model = "glm-5.3"
	}
	rawMessages, _ := src["messages"].([]any)
	messages := make([]map[string]any, 0, len(rawMessages))
	for _, raw := range rawMessages {
		if m, ok := raw.(map[string]any); ok {
			messages = append(messages, m)
		}
	}

	kept, dropped := trimRemoteHistory(messages, remoteQueryMaxChars)
	query := buildRemoteQuery(kept)

	common := map[string]any{
		"language": "zh-cn", "app_language": "zh-CN", "quality": "stable",
		"app_version": "1.0.0.1229", "web_id": "", "user_identity": "Free",
		"is_freshman": "0", "biz_user_id": "", "user_unique_id": "",
		"scope": "marscode-cn", "tenant": "marscode", "region": "cn",
		"aiRegion": "cn", "is_privacy_mode": 0, "privacy_mode": "off",
		"solo_chat_mode": "code", "biz_session_id": requestID,
	}
	commonJSON, err := json.Marshal(common)
	if err != nil {
		return nil, 0, 0, err
	}
	body := remoteCreateBody{
		Mode:          "code",
		EnvironmentID: "default",
		Env:           "remote",
		Origin:        "web",
		InitialMessage: remoteInitialMessage{
			ChatSessionID:          "",
			Content:                []any{},
			Query:                  query,
			ModelName:              model,
			AgentType:              "solo_agent_remote",
			AgentID:                "solo_agent_remote",
			ModelSelectionStrategy: "auto",
			CommonParams:           string(commonJSON),
		},
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		return nil, 0, 0, err
	}
	return encoded, len(kept), dropped, nil
}

// trimRemoteHistory 从最早的**非 system** 消息开始丢，直到扁平化 query 落到上限内。
//
// system 始终保留（它是行为约束，丢了会改变回答质量）。
// 成对丢弃：带 tool_calls 的 assistant 与紧随其后的 tool 消息一起丢——只丢一半会
// 留下孤立的工具结果，上游对「工具调用与结果不配对」是直接报错的。
func trimRemoteHistory(messages []map[string]any, maxChars int) ([]map[string]any, int) {
	var systems, rest []map[string]any
	for _, m := range messages {
		if role, _ := m["role"].(string); role == "system" {
			systems = append(systems, m)
		} else {
			rest = append(rest, m)
		}
	}
	total := len(messages)
	if len(buildRemoteQuery(append(append([]map[string]any{}, systems...), rest...))) <= maxChars {
		return messages, 0
	}
	for len(rest) > 0 {
		// 成对丢：assistant(tool_calls) + 紧随的 tool 消息。
		if role, _ := rest[0]["role"].(string); role == "assistant" {
			if calls, ok := rest[0]["tool_calls"].([]any); ok && len(calls) > 0 {
				rest = rest[1:]
				for len(rest) > 0 {
					if r, _ := rest[0]["role"].(string); r != "tool" {
						break
					}
					rest = rest[1:]
				}
				continue
			}
		}
		rest = rest[1:]
	}
	kept := append(append([]map[string]any{}, systems...), rest...)
	return kept, total - len(kept)
}

// remoteSession 一次 Remote 会话的标识。
type remoteSession struct {
	id        string
	messageID string
}

// createRemoteSession 建会话（两步协议的第 1 步）。
func createRemoteSession(ctx context.Context, a *auth.Auth, body []byte) (remoteSession, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		RemoteBaseVar+remoteCreatePath, bytes.NewReader(body))
	if err != nil {
		return remoteSession{}, err
	}
	for k, vs := range remoteHeaders(a) {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	client := &http.Client{Transport: chatTransport()}
	resp, err := client.Do(req)
	if err != nil {
		return remoteSession{}, fmt.Errorf("trae remote 建会话：%w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return remoteSession{}, fmt.Errorf("trae remote 建会话读取：%w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return remoteSession{}, fmt.Errorf("trae remote 建会话 HTTP %d：%s",
			resp.StatusCode, truncate(string(raw), 200))
	}
	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil {
		return remoteSession{}, fmt.Errorf("trae remote 建会话响应解析：%w", err)
	}
	if code, ok := asFloat(payload["code"]); ok && code != 0 {
		message, _ := payload["message"].(string)
		return remoteSession{}, fmt.Errorf("trae remote 建会话 code %g：%s", code, message)
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

// stopRemoteSession 主动结束会话（尽力而为，失败不影响结果）。
//
// 上游对会话有并发槽位，不结束会占着槽位；调用方在收流结束后调用。
func stopRemoteSession(ctx context.Context, a *auth.Auth, sid string) {
	if sid == "" {
		return
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		fmt.Sprintf(RemoteBaseVar+remoteStopFmt, sid), bytes.NewReader([]byte("{}")))
	if err != nil {
		return
	}
	for k, vs := range remoteHeaders(a) {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	resp, err := (&http.Client{Transport: chatTransport()}).Do(req)
	if err != nil {
		return
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	_ = resp.Body.Close()
}

// openRemoteEvents 收流（两步协议的第 2 步）。
func openRemoteEvents(ctx context.Context, a *auth.Auth, session remoteSession) (io.ReadCloser, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		fmt.Sprintf(RemoteBaseVar+remoteEventsFmt, session.id, session.messageID), nil)
	if err != nil {
		return nil, err
	}
	for k, vs := range remoteHeaders(a) {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	req.Header.Set("Accept", "text/event-stream")
	resp, err := (&http.Client{Transport: chatTransport()}).Do(req)
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

// ── Remote 事件 → llm_utils_chat 事件的转译 ─────────────────────────────
//
// 转译在**事件层**做，把 Remote 的事件名与字段改写成 llm_utils_chat 的形态，
// 再交给既有的 translator。这样做的理由：translator 已经处理了累计快照、
// 工具跨帧合并、think 块拆分、终态归一、usage 映射等全部细节（并有回归测试），
// 为 Remote 另写一套转译等于把这些细节再实现一遍、两份必然漂移。
//
// 字段对应关系（2026-10-15 从真实事件 dump 确认）：
//
//	Remote plan_item.thought            → output.response（正文累计快照）
//	Remote plan_item.reasoning_content  → output.reasoning_content
//	Remote token_usage                  → token_usage（字段名不同，见下）
//	Remote done                         → done（finish_reason 由 translator 归一）
//
// Remote 的 status_changed / metadata / timing_events / platform_timing /
// session_title_message / session_icon_message / model_config 在 llm_utils_chat 里
// 没有对应物，直接丢弃——它们不携带正文。
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
				return
			}
		}
	}()
	return pr
}

// remoteEventConverter 把 Remote 事件转成 llm_utils_chat 形态。
//
// **Remote 的工具调用一律不透传**（2026-10-15 实测后定的）。
//
// 为什么：Remote 是一个**自带沙箱的 agent**——它有自己的工具（实测 LS /
// EnvironmentSetup / finish / Glob / Read…），参数以增量片段流式给出，然后它
// **自己执行**并继续下一轮。它不是「向客户端请求执行工具」的协议。
//
// 把它当客户端工具调用透传会同时踩两个坑：
//  1. 参数永远「不完整」——客户端视角下这次调用没有闭合（Remote 不需要客户端回结果），
//     网关的校验器因此报 "upstream ended with incomplete or invalid tool arguments"，
//     整个回答变成失败。实测就是这么炸的（工具帧里 arguments 只到
//     `"internal/panel/actions.go"` 就停住，JSON 不闭合）。
//  2. 语义错位——客户端会去执行一个**已经被 Remote 执行过**的工具（双重执行），
//     而且 Remote 的沙箱工具客户端根本没有。
//
// 所以这里只取正文（thought / reasoning_content）与 usage，工具活动仅用于日志计数。
// 客户端拿到的是最终回答文本。这是回落到 Remote 时的**能力边界**，见文件头「代价」。
type remoteEventConverter struct {
	// toolRounds 观测到的工具调用轮次（仅用于日志，不透传）。
	toolRounds int
	// seenTools 已见过的工具名（同一工具重复出现只计一次轮次）。
	seenTools map[string]bool
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
		if thought, ok := obj["thought"].(string); ok && thought != "" {
			out["response"] = thought
		}
		if reasoning, ok := obj["reasoning_content"].(string); ok && reasoning != "" {
			out["reasoning_content"] = reasoning
		}
		// 工具活动只计数、不透传（理由见本类型注释）。
		if info, ok := obj["tool_call_info"].(map[string]any); ok {
			if toolName, _ := info["name"].(string); toolName != "" {
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
	return "", false
}
