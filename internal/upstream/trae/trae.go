// Package trae 把 Trae CN 的模型通道（llm_utils_chat）包装成 OpenAI 兼容出站传输。
//
// 移植自 autumnsentiment/Trae2api-cn（Python）的账号与调用形态，输出侧适配
// 本网关的既有协议栈：ChatStream 返回的 SSE 已是 OpenAI chat.completions 形状，
// 上层（Anthropic/Responses/面板/池/粘性）零改动复用。
//
// 协议要点（全部来自 Trae2api-cn 的逆向与实测）：
//   - 上游端点：POST {gateway}/api/agent/v3/llm_utils_chat，SSE 流式
//   - 鉴权：`Authorization: Cloud-IDE-JWT <token>` + x-uid + 设备指纹头族
//   - 请求体：messages（OpenAI 形状，工具转 Trae 原生）+ config_name + function=solo_work_lite
//   - 事件：event: output / data: {"response":"增量文本"|"tool_calls":[...]}，
//     event: token_usage / data: {"input_token":n,"output_token":n}，
//     event: done / data: {"finish_reason":"stop"|"tool_calls"}
//
// ═══ 更新日志 ═══
// 2026-10-10：初版——llm_utils_chat 传输 + 事件→OpenAI SSE 转译 + ExchangeToken 刷新。
package trae

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/jsonutil"
)

// 出站端点与客户端指纹（对齐 Trae CN 稳定客户端，来自 Trae2api-cn 实测默认值）。
const (
	// Realm 是 Trae 账号在网关里的域标识。模型名写作 "trae:<model>" 时选号只在
	// 该域内进行（池的 realm 谓词是通用的），出站则走本包的 llm_utils_chat 传输。
	Realm = "trae"

	// GatewayBase llm_utils_chat 所在的模型网关。账号 OAuth host（api.trae.cn）不暴露
	// 模型路由，必须走这个网关（Trae2api-cn _model_gateway_base 的结论）。
	GatewayBase = "https://trae-api-cn.mchost.guru"

	// ChatPath llm_utils_chat 端点。工具能力最完整（Trae2api-cn 实测 glm-5.3 7/7、
	// deepseek-v4-pro 7/7），失败时上层可回落 remote 会话端点（暂未实现）。
	ChatPath = "/api/agent/v3/llm_utils_chat"

	// exchangePath Cloud-IDE-JWT 刷新端点（api.trae.cn，非模型网关）。
	exchangeHost = "https://api.trae.cn"
	exchangePath = "/cloudide/api/v3/trae/oauth/ExchangeToken"

	// TraeClientID 刷新用的 ClientID（Trae 官网内置值）。
	TraeClientID = "ono9krqynydwx5"

	// SOLO 客户端指纹段（llm_utils_chat 的网关只认这一族版本头）。
	soloGatewayVersion     = "0.1.52"
	soloGatewayVersionCode = "20260811"
	soloDeviceBrand        = "83DG"
	soloOSVersion          = "Windows 11 Pro"
	soloDeviceType         = "windows"
	soloPackageType        = "stable_cn"
	soloPlatform           = "windows"
	soloLSCBDAID           = "787976"
)

// chatBody llm_utils_chat 的原生请求体（TraeWork 的 solo_work_lite 形态）。
type chatBody struct {
	Messages          []map[string]any `json:"messages"`
	ConfigName        string           `json:"config_name"`
	Model             string           `json:"model"`
	Function          string           `json:"function"`
	Stream            bool             `json:"stream"`
	RequestID         string           `json:"request_id"`
	SessionID         string           `json:"session_id"`
	MaxTokens         *int64           `json:"max_tokens,omitempty"`
	Tools             []map[string]any `json:"tools,omitempty"`
	ToolChoice        any              `json:"tool_choice,omitempty"`
	ParallelToolCalls *bool            `json:"parallel_tool_calls,omitempty"`
}

// exchangeRequest ExchangeToken 的请求体（与 Trae 官网实现一致）。
type exchangeRequest struct {
	ClientID     string `json:"ClientID"`
	RefreshToken string `json:"RefreshToken"`
	ClientSecret string `json:"ClientSecret"`
	UserID       string `json:"UserID"`
}

// exchangeResponse ExchangeToken 的响应（Result 包 Token/RefreshToken/UserInfo）。
type exchangeResponse struct {
	Result struct {
		Token           string          `json:"Token"`
		RefreshToken    string          `json:"RefreshToken"`
		TokenExpireAt   string          `json:"TokenExpireAt"`
		RefreshExpireAt string          `json:"RefreshExpireAt"`
		UserInfo        json.RawMessage `json:"UserInfo"`
	} `json:"Result"`
}

// buildHeaders 构造 llm_utils_chat 的完整头族。
//
// 形态对齐 TraeWork 原生客户端（Trae2api-cn send_chat_request 的 SOLO 头组）：
// 基础 IDE 头（Cloud-IDE-JWT 鉴权 + x-uid + 设备指纹）之上叠加 SOLO 路由头。
// 网关只认这一族版本头——用账号 OAuth host 的旧 IDE 版本头会得到
// "param is invalid"（Trae2api-cn 实测结论）。
// BuildHeaders 导出出站头族（排障与目录探测共用；生产走 ChatStream 内部调用）。
func BuildHeaders(a *auth.Auth) http.Header { return buildHeaders(a) }

func buildHeaders(a *auth.Auth) http.Header {
	h := http.Header{}
	token := a.AccessToken
	set := func(k, v string) { h.Set(k, v) }
	set("Content-Type", "application/json")
	set("Accept", "text/event-stream")
	set("Authorization", "Cloud-IDE-JWT "+token)
	set("X-Cloudide-Token", token)
	set("x-uid", a.UID)
	set("x-app-id", "6eefa01c-1036-4c7e-9ca5-d891f63bfcd8")
	set("x-device-id", stableDeviceID(a.UID, "device"))
	set("x-machine-id", stableDeviceID(a.UID, "machine"))
	set("x-request-id", stableDeviceID(a.UID, "request"))
	set("x-ide-version", soloGatewayVersion)
	set("x-ide-version-code", soloGatewayVersionCode)
	set("x-ide-version-type", "stable")
	set("x-device-cpu", "AMD")
	set("x-device-brand", soloDeviceBrand)
	set("x-device-type", soloDeviceType)
	set("x-ide-token", token)
	set("x-os-version", soloOSVersion)
	set("x-system-type", "Windows")
	set("x-app-version", "default")
	set("x-app-version-code", soloGatewayVersionCode)
	set("x-device-platform", soloPlatform)
	set("x-version-code", soloGatewayVersionCode)
	set("package-type", soloPackageType)
	set("x-lscbd-aid", soloLSCBDAID)
	set("x-lscbd-platform", soloPlatform)
	set("x-ss-dp", soloLSCBDAID)
	set("x-plugin-channel", "icube-ai")
	set("app-version", soloGatewayVersion)
	set("x-bridge-transport", "aha")
	set("x-ahanet-timeout", "86400")
	set("x-lgw-req-sdk-type", "3")
	set("x-net-sdk-domain-dispatch", "1")
	set("x-ttnet-bypass-decompression", "1")
	set("x-ttnet-bypass-cookie", "0")
	set("request-traffic-type", "prod")
	set("User-Agent", "Trae/"+soloGatewayVersion)
	set("Referer", GatewayBase+ChatPath)
	return h
}

// BuildChatBody 把（已由上层整理过的）OpenAI 出站请求体转成 llm_utils_chat 原生体。
//
// messages：OpenAI 形状直接透传（role/content/tool_calls 结构上游原样接受），
// 只有 tools 声明需要转 Trae 原生（parameters 是 JSON 字符串而非对象）。
// 上层 prepareBody 已做过 effort 降级与预算补齐，这里不再重复处理。
func BuildChatBody(prepared []byte, requestID string) ([]byte, error) {
	var src map[string]any
	if err := jsonutil.Decode(prepared, &src); err != nil || src == nil {
		return nil, fmt.Errorf("trae chat body parse: %w", err)
	}
	body := chatBody{
		Function:  "solo_work_lite",
		Stream:    true,
		RequestID: requestID,
		SessionID: requestID,
	}
	// 上游按 config_name 精确匹配（大小写敏感），必须先把客户端写的宽松名
	// （gpt-4o / claude-sonnet-4-5 之类）映射到 Trae 的真实模型名，否则会被拒。
	// 优先用**实时目录**匹配（客户端可能按显示名请求，如 "DeepSeek-V4.1-Flash"）；
	// 目录不可用时回落静态别名表。都查不到则原样透传——上游的报错比网关自己
	// 猜一个替代模型更可用。
	name := ResolveModelName(modelFromSource(src))
	if name == "" {
		name = "glm-5.2"
	}
	body.ConfigName = name
	body.Model = name
	if msgs, ok := src["messages"].([]any); ok {
		for _, m := range msgs {
			if mm, ok := m.(map[string]any); ok {
				body.Messages = append(body.Messages, nativeMessage(mm))
			}
		}
	}
	if body.Messages == nil {
		body.Messages = []map[string]any{}
	}
	if mt, ok := src["max_tokens"].(json.Number); ok {
		if n, err := mt.Int64(); err == nil && n > 0 {
			body.MaxTokens = &n
		}
	}
	if tools, ok := src["tools"].([]any); ok && len(tools) > 0 {
		native := nativeToolDefinitions(tools)
		if len(native) > 0 {
			body.Tools = native
			// tool_choice 只有在带 tools 时才有意义（对齐 Trae2api-cn 的取舍）。
			if tc, present := src["tool_choice"]; present && tc != nil {
				if choice := nativeToolChoice(tc); choice != nil {
					body.ToolChoice = choice
				}
			}
			if ptc, ok := src["parallel_tool_calls"].(bool); ok {
				body.ParallelToolCalls = &ptc
			}
		}
	}
	out, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	return out, nil
}

// nativeMessage 把一条 OpenAI 消息转成 Trae 原生形状。
//
// 2026-10-11 实测：直接透传 OpenAI 消息会被上游拒掉——
//
//	{"code":4001,"message":"bad request: json: cannot unmarshal string into Go struct
//	 field LLMRawMessage.messages.content of type []*idecopilot.LLMRawMessageContent"}
//
// 即 `content` 必须是**内容块数组**而不是字符串。块形如
// `{"type":"text","text":"..."}`（对齐 Trae2api-cn 的 _ide_native_content）。
// 助手消息里的 tool_calls 要转成上游的 `function_call` 形状。
func nativeMessage(message map[string]any) map[string]any {
	out := map[string]any{}
	role, _ := message["role"].(string)
	role = strings.ToLower(strings.TrimSpace(role))
	if role == "developer" {
		role = "system"
	}
	switch role {
	case "system", "user", "assistant", "tool", "function":
	default:
		role = "user"
	}
	out["role"] = role

	if content, present := message["content"]; present {
		out["content"] = nativeContent(content)
	}

	if role == "assistant" {
		if calls := nativeAssistantToolCalls(message["tool_calls"]); len(calls) > 0 {
			out["tool_calls"] = calls
		}
		if legacy, ok := message["function_call"].(map[string]any); ok {
			if converted := nativeFunctionCall(legacy); converted != nil {
				out["function_call"] = converted
			}
		}
	}
	if role == "tool" || role == "function" {
		for _, key := range []string{"tool_call_id", "name"} {
			if value, ok := message[key]; ok && value != nil {
				out[key] = fmt.Sprint(value)
			}
		}
	}
	return out
}

// nativeContent 把 content 归一成 Trae 要的内容块数组。
//
// 字符串 → 单个 text 块；已经是块数组 → 逐块归一（text/input_text 统一成 text，
// 并把文本提到 text 字段）；其它标量 → 转字符串后包成 text 块。
func nativeContent(content any) any {
	switch value := content.(type) {
	case nil:
		return nil
	case string:
		return []any{map[string]any{"type": "text", "text": value}}
	case []any:
		blocks := make([]any, 0, len(value))
		for _, raw := range value {
			switch block := raw.(type) {
			case string:
				blocks = append(blocks, map[string]any{"type": "text", "text": block})
			case map[string]any:
				blocks = append(blocks, nativeContentBlock(block))
			default:
				blocks = append(blocks, raw)
			}
		}
		return blocks
	case map[string]any:
		return []any{nativeContentBlock(value)}
	default:
		return []any{map[string]any{"type": "text", "text": fmt.Sprint(value)}}
	}
}

// nativeContentBlock 归一单个内容块：文本类统一成 {"type":"text","text":...}。
func nativeContentBlock(block map[string]any) map[string]any {
	out := map[string]any{}
	for key, value := range block {
		out[key] = value
	}
	kind, _ := out["type"].(string)
	switch strings.ToLower(kind) {
	case "text", "input_text":
		out["type"] = "text"
		text, _ := out["text"].(string)
		if text == "" {
			text, _ = out["content"].(string)
		}
		if text == "" {
			text, _ = out["value"].(string)
		}
		out["text"] = text
		delete(out, "value")
	}
	return out
}

// nativeAssistantToolCalls 把 OpenAI 的 tool_calls 转成 Trae 的 function_call 形状。
//
// 上游助手消息用的是 `{"id","type","function_call":{"name","arguments"}}`，
// 不是 OpenAI 的 `{"function":{"name","arguments"}}`（对齐 Trae2api-cn
// 的 _native_assistant_tool_calls）。arguments 必须是 JSON 字符串。
func nativeAssistantToolCalls(value any) []map[string]any {
	calls, ok := value.([]any)
	if !ok || len(calls) == 0 {
		return nil
	}
	out := make([]map[string]any, 0, len(calls))
	for _, raw := range calls {
		call, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		fn := call["function"]
		if fn == nil {
			fn = call["function_call"]
		}
		converted := nativeFunctionCall(fn)
		if converted == nil {
			continue
		}
		id, _ := call["id"].(string)
		if id == "" {
			id, _ = call["tool_call_id"].(string)
		}
		typ, _ := call["type"].(string)
		if typ == "" {
			typ = "function"
		}
		out = append(out, map[string]any{"id": id, "type": typ, "function_call": converted})
	}
	return out
}

// nativeFunctionCall 归一成 {"name","arguments"}，arguments 保证是 JSON 字符串。

func nativeFunctionCall(value any) map[string]any {
	fn, ok := value.(map[string]any)
	if !ok {
		return nil
	}
	name, _ := fn["name"].(string)
	if name == "" {
		return nil
	}
	arguments := fn["arguments"]
	if arguments == nil {
		arguments = fn["input"]
	}
	if arguments == nil {
		arguments = map[string]any{}
	}
	encoded, ok := arguments.(string)
	if !ok {
		raw, err := json.Marshal(arguments)
		if err != nil {
			raw = []byte("{}")
		}
		encoded = string(raw)
	}
	return map[string]any{"name": name, "arguments": encoded}
}

// 唯一实质差异：Trae 的 FunctionDefinition.parameters 是 **JSON 字符串**，
// 不是对象（上游的 protobuf/JSON 适配层要求）。其余字段（name/description/strict）
// 原样保留。
// nativeToolDefinitions 把 OpenAI tools 声明转成 Trae 原生形态。
//
// 唯一实质差异：Trae 的 FunctionDefinition.parameters 是 **JSON 字符串**，
// 不是对象（上游的 protobuf/JSON 适配层要求）。其余字段（name/description/strict）
// 原样保留。
func nativeToolDefinitions(tools []any) []map[string]any {
	out := make([]map[string]any, 0, len(tools))
	for _, t := range tools {
		tool, ok := t.(map[string]any)
		if !ok {
			continue
		}
		result := map[string]any{}
		for k, v := range tool {
			result[k] = v
		}
		fn, ok := tool["function"].(map[string]any)
		if !ok {
			// 紧凑形态（少数终端适配器）：从顶层取字段组成 function。
			compact := map[string]any{}
			for _, k := range []string{"name", "description", "parameters", "strict"} {
				if v, ok := tool[k]; ok {
					compact[k] = v
					delete(result, k)
				}
			}
			if compact["name"] == nil {
				continue
			}
			result = map[string]any{"type": "function", "function": compact}
			fn = compact
		} else {
			fnCopy := map[string]any{}
			for k, v := range fn {
				fnCopy[k] = v
			}
			result["function"] = fnCopy
			fn = fnCopy
		}
		if name, _ := fn["name"].(string); name == "" {
			continue
		}
		params := fn["parameters"]
		if params == nil {
			params = firstNonNil(fn["input_schema"], fn["inputSchema"])
		}
		switch p := params.(type) {
		case string:
			fn["parameters"] = p
		default:
			if params == nil {
				params = map[string]any{}
			}
			encoded, err := json.Marshal(params)
			if err != nil {
				continue
			}
			fn["parameters"] = string(encoded)
		}
		delete(fn, "input_schema")
		delete(fn, "inputSchema")
		if t, _ := result["type"].(string); t == "" {
			result["type"] = "function"
		}
		out = append(out, result)
	}
	return out
}

// nativeToolChoice 把 OpenAI tool_choice 归一成 Trae 接受的形态：
// "auto"/"none"/"required" 字符串或具名函数名字符串；其余（含对象）返回 nil。
func nativeToolChoice(choice any) any {
	switch v := choice.(type) {
	case string:
		low := strings.ToLower(strings.TrimSpace(v))
		if low == "auto" || low == "none" || low == "required" {
			return low
		}
		return v
	case map[string]any:
		if t, _ := v["type"].(string); t != "" {
			low := strings.ToLower(t)
			if low == "auto" || low == "none" || low == "required" {
				return low
			}
			if low == "function" {
				if fn, ok := v["function"].(map[string]any); ok {
					if name, _ := fn["name"].(string); name != "" {
						return name
					}
				}
				return "auto"
			}
		}
		if name, _ := v["name"].(string); name != "" {
			return name
		}
	}
	return nil
}

func firstNonNil(values ...any) any {
	for _, v := range values {
		if v != nil {
			return v
		}
	}
	return nil
}

// stableDeviceID 从 UID 派生稳定的设备标识（同一账号恒同值，跨请求不漂移）。
// Trae2api-cn 用随机值生成一次后存全局；我们按 UID 哈希派生，效果等价且无状态。
func stableDeviceID(uid, purpose string) string {
	return deviceID(uid, purpose)
}

// Refresh 执行 ExchangeToken 刷新，返回可写回 auth.Auth 的更新。
//
// POST api.trae.cn/cloudide/api/v3/trae/oauth/ExchangeToken
// {"ClientID","RefreshToken","ClientSecret":"-","UserID"}。
// 响应 Result.Token 为新 JWT、Result.RefreshToken 为新 refresh token（可能轮换）。
// TokenExpireAt 是秒或毫秒时间戳字符串，统一归一成 Unix 秒。
func Refresh(a *auth.Auth) (accessToken, refreshToken string, expiresAt int64, err error) {
	rt := strings.TrimSpace(a.RefreshToken)
	if rt == "" {
		return "", "", 0, fmt.Errorf("trae refresh: no refresh token")
	}
	payload, _ := json.Marshal(exchangeRequest{
		ClientID:     TraeClientID,
		RefreshToken: rt,
		ClientSecret: "-",
		UserID:       a.UID,
	})
	resp, err := http.Post(exchangeHost+exchangePath, "application/json", strings.NewReader(string(payload)))
	if err != nil {
		return "", "", 0, fmt.Errorf("trae refresh: %w", err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", "", 0, fmt.Errorf("trae refresh read: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return "", "", 0, fmt.Errorf("trae refresh: HTTP %d: %s", resp.StatusCode, truncate(string(data), 300))
	}
	var out exchangeResponse
	if err := json.Unmarshal(data, &out); err != nil {
		return "", "", 0, fmt.Errorf("trae refresh parse: %w", err)
	}
	if out.Result.Token == "" {
		return "", "", 0, fmt.Errorf("trae refresh: no Token in response (re-login required): %s", truncate(string(data), 300))
	}
	newRefresh := out.Result.RefreshToken
	if newRefresh == "" {
		newRefresh = rt
	}
	return out.Result.Token, newRefresh, parseExpire(out.Result.TokenExpireAt), nil
}

// parseExpire 把 TokenExpireAt（秒/毫秒时间戳字符串）归一成 Unix 秒；无法解析返回 0。
func parseExpire(raw string) int64 {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0
	}
	var num float64
	if _, err := fmt.Sscanf(raw, "%g", &num); err != nil {
		return 0
	}
	if num > 1e12 { // 毫秒
		num /= 1000
	}
	return int64(num)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
