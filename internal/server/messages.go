// ═══ 更新日志 ═══
// 2026-09-28：messages 里的 role=system/developer 折进系统提示、role=tool/function 按 user 处理，不再整条 400。
// 2026-09-26：顶层不支持块同样占位（不再整条 400）；工具调用 id 允许跨轮复用；空工具结果回退空串。
// 2026-09-26：历史里上游无法承载的内容块（document/tool_reference 等）改为文字占位，不再让整段会话永久 400。
// 2026-09-26：thinking.type=adaptive 映射为上游可识别的 enabled（不带预算），不再原样转发。
// 2026-09-25：Anthropic messages 输入复用现有请求执行模块，保留工具配对、图片、思考与模型权限。
// 2026-09-25：按 messages 契约校验正数预算、采样范围、签名与工具结果顺序，避免无效参数调用上游。
// 2026-09-25：精确接受 NF 保留全部思考的无裁剪请求，并将 xhigh 交给既有上游 effort 归一化。
package server

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"

	"workbuddy2api/internal/jsonutil"
	"workbuddy2api/internal/requestlog"
	"workbuddy2api/internal/upstream"
)

func (h *Handler) messagesEntry(w http.ResponseWriter, r *http.Request) {
	mw := newMessagesWriter(w)
	// Standard Anthropic clients use x-api-key. An explicit Authorization
	// header takes precedence, including when it is invalid.
	if r.Header.Get("Authorization") == "" && r.Header.Get("X-API-Key") != "" {
		r = r.Clone(r.Context())
		r.Header.Set("Authorization", "Bearer "+r.Header.Get("X-API-Key"))
	}
	h.withAuth(h.withGeneration(requestlog.ProtocolMessages, h.withDecodedRequest(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/messages/count_tokens" {
			writeOpenAIError(w, http.StatusNotImplemented, "not_supported", "the upstream does not provide an exact token-count endpoint; use reported response usage")
			return
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, h.cfg.MaxBodyBytes+1))
		if err != nil {
			writeBodyReadError(w, err)
			return
		}
		if int64(len(body)) > h.cfg.MaxBodyBytes {
			writeOpenAIError(w, http.StatusRequestEntityTooLarge, "request_body_too_large", "request body exceeds the configured size limit")
			return
		}
		chat, model, stream, err := messagesToChat(body)
		if err != nil {
			writeOpenAIError(w, 400, "invalid_request", err.Error())
			return
		}
		mw.model, mw.stream = model, stream
		// 首帧 message_start 的临时 input_tokens：用与输出预算同一套估算口径，
		// 让客户端在轮次进行中就能看到这一轮读了多少上下文（终态仍以上游实报为准）。
		mw.promptEstimate = upstream.EstimateInputTokens(chat)
		sub := r.Clone(r.Context())
		sub.Body = io.NopCloser(bytes.NewReader(chat))
		sub.ContentLength = int64(len(chat))
		h.chatCompletions(w, sub)
	})))(mw, r)
	mw.finish()
}

func messagesToChat(body []byte) ([]byte, string, bool, error) {
	var source map[string]any
	if err := jsonutil.Decode(body, &source); err != nil || source == nil {
		return nil, "", false, fmt.Errorf("request body must be a JSON object")
	}
	model, _ := source["model"].(string)
	if strings.TrimSpace(model) == "" {
		return nil, "", false, fmt.Errorf("model must be a nonempty string")
	}
	if err := requestValidationOptionalBools(source, "request", "stream"); err != nil {
		return nil, "", false, err
	}
	stream, _ := source["stream"].(bool)
	if err := requestValidationOptionalNumbers(source, "request", true, "max_tokens", "top_k"); err != nil {
		return nil, "", false, err
	}
	maxTokens, ok := source["max_tokens"].(json.Number)
	maximum, err := maxTokens.Int64()
	if !ok || err != nil || maximum <= 0 {
		return nil, "", false, fmt.Errorf("max_tokens must be a positive integer")
	}
	if err := requestValidationOptionalNumbers(source, "request", false, "temperature", "top_p"); err != nil {
		return nil, "", false, err
	}
	for _, key := range []string{"temperature", "top_p"} {
		if number, ok := source[key].(json.Number); ok {
			value, _ := number.Float64()
			if value < 0 || value > 1 {
				return nil, "", false, fmt.Errorf("%s must be between 0 and 1", key)
			}
		}
	}
	if metadata := source["metadata"]; metadata != nil {
		object, err := requestValidationObject(metadata, "metadata")
		if err != nil {
			return nil, "", false, err
		}
		if err := requestValidationOptionalStrings(object, "metadata", "user_id"); err != nil {
			return nil, "", false, err
		}
	}
	if value := source["context_management"]; value != nil && !messagesKeepsAllContext(value) {
		return nil, "", false, fmt.Errorf("context_management only supports clear_thinking_20251015 with keep all; active context edits are unavailable")
	}
	for _, key := range []string{"container", "mcp_servers"} {
		if value := source[key]; value != nil {
			return nil, "", false, fmt.Errorf("%s is not supported by this stateless gateway", key)
		}
	}
	chat := map[string]any{"model": model, "stream": stream, "max_tokens": maxTokens}
	for _, key := range []string{"temperature", "top_p", "top_k", "metadata"} {
		if value, ok := source[key]; ok {
			chat[key] = value
		}
	}
	if stop, ok := source["stop_sequences"]; ok && stop != nil {
		if err := requestValidationStringArray(stop, "stop_sequences"); err != nil {
			return nil, "", false, err
		}
		chat["stop"] = stop
	}
	if thinking, present := source["thinking"]; present && thinking != nil {
		object, err := requestValidationObject(thinking, "thinking")
		if err != nil {
			return nil, "", false, err
		}
		kind, _ := object["type"].(string)
		if kind != "enabled" && kind != "disabled" && kind != "adaptive" {
			return nil, "", false, fmt.Errorf("unsupported thinking.type")
		}
		if err := requestValidationOptionalNumbers(object, "thinking", true, "budget_tokens"); err != nil {
			return nil, "", false, err
		}
		if kind == "enabled" {
			budget, ok := object["budget_tokens"].(json.Number)
			count, err := budget.Int64()
			if !ok || err != nil || count < 1024 || count >= maximum {
				return nil, "", false, fmt.Errorf("thinking.budget_tokens must be at least 1024 and less than max_tokens")
			}
		} else if object["budget_tokens"] != nil {
			return nil, "", false, fmt.Errorf("thinking.budget_tokens is only valid with thinking.type enabled")
		}
		// Retain the actual budget hint instead of replacing it with a made-up
		// multiplier. Its enforcement remains a capability of the chosen model.
		if kind == "adaptive" {
			// Anthropic adaptive = 由模型决定是否思考；上游只认 enabled/disabled，原样转发
			// 会被忽略或拒绝。映射为不带预算的 enabled，保留其余字段。
			adapted := make(map[string]any, len(object))
			for key, value := range object {
				adapted[key] = value
			}
			adapted["type"] = "enabled"
			object = adapted
		}
		chat["thinking"] = object
		if kind == "enabled" || kind == "adaptive" {
			chat["reasoning_effort"] = "high"
		}
	}
	if output, present := source["output_config"]; present && output != nil {
		object, err := requestValidationObject(output, "output_config")
		if err != nil {
			return nil, "", false, err
		}
		if effort, ok := object["effort"]; ok {
			if err := requestValidationString(effort, "output_config.effort", true); err != nil {
				return nil, "", false, err
			}
			switch effort {
			case "low", "medium", "high", "xhigh", "max":
			default:
				return nil, "", false, fmt.Errorf("output_config.effort must be low, medium, high, xhigh or max")
			}
			chat["reasoning_effort"] = effort
		}
		if format, ok := object["format"]; ok && format != nil {
			f, err := requestValidationObject(format, "output_config.format")
			if err != nil {
				return nil, "", false, err
			}
			if f["type"] != "json_schema" {
				return nil, "", false, fmt.Errorf("output_config.format must use json_schema")
			}
			if _, err := requestValidationObject(f["schema"], "output_config.format.schema"); err != nil {
				return nil, "", false, err
			}
			chat["response_format"] = map[string]any{"type": "json_schema", "json_schema": map[string]any{"name": "response", "schema": f["schema"], "strict": true}}
		}
	}
	var messages []any
	var systemEntry map[string]any
	if system, present := source["system"]; present && system != nil {
		parts, err := anthropicParts(system, "system", false)
		if err != nil {
			return nil, "", false, err
		}
		systemEntry = map[string]any{"role": "system", "content": parts}
		messages = append(messages, systemEntry)
	}
	// 2026-09-28：把系统提示整段放进 messages 的客户端确实存在——Claude Code 2.1.283 遇到
	// 它不认识的模型名时就会额外发一条 role=system 的消息（内容是 Environment 段）。官方
	// 端点容忍这种写法，这里硬拒等于让这类客户端每一轮都 400，所以改为并回系统提示。合并
	// 放在循环之后，与这条消息出现的位置无关。
	var systemParts []any
	turns := 0
	input, ok := source["messages"].([]any)
	if !ok || len(input) == 0 {
		return nil, "", false, fmt.Errorf("messages must be a nonempty array")
	}
	pending := map[string]bool{}
	for index, raw := range input {
		path := fmt.Sprintf("messages[%d]", index)
		message, err := requestValidationObject(raw, path)
		if err != nil {
			return nil, "", false, err
		}
		role := strings.ToLower(strings.TrimSpace(stringField(message, "role")))
		switch role {
		case "user", "assistant":
		case "system", "developer":
			blocks, err := anthropicBlocks(message["content"], path+".content")
			if err != nil {
				return nil, "", false, err
			}
			for bi, block := range blocks {
				bp := fmt.Sprintf("%s.content[%d]", path, bi)
				switch block["type"] {
				case "text", "image":
					part, err := anthropicPart(block, bp, false)
					if err != nil {
						return nil, "", false, err
					}
					systemParts = append(systemParts, part)
				default:
					part, err := unsupportedPlaceholder(block)
					if err != nil {
						return nil, "", false, err
					}
					systemParts = append(systemParts, part)
				}
			}
			log.Printf("INFO: [server] merged %s (role=%s) into the system prompt", path, role)
			continue
		case "tool", "function":
			// Chat 协议用 tool/function 角色承载工具结果；Anthropic 语义里工具结果属于
			// user 轮，直接按 user 处理，别让客户端因为换了个端点就整条被拒。
			role = "user"
		default:
			return nil, "", false, fmt.Errorf("%s.role must be user, assistant, system or tool", path)
		}
		blocks, err := anthropicBlocks(message["content"], path+".content")
		if err != nil {
			return nil, "", false, err
		}
		var parts, calls, results []any
		var reasoning strings.Builder
		// seen 只在单条 assistant 消息内查重：跨轮复用同一个工具调用 id（很多客户端
		// 每轮重新编号）是合法的，只有「同一轮内重复」和「上一轮尚未配对又出现」才是错。
		seen := map[string]bool{}
		for bi, block := range blocks {
			bp := fmt.Sprintf("%s.content[%d]", path, bi)
			switch block["type"] {
			case "text", "image":
				part, err := anthropicPart(block, bp, role == "user")
				if err != nil {
					return nil, "", false, err
				}
				parts = append(parts, part)
			case "thinking":
				if role != "assistant" {
					return nil, "", false, fmt.Errorf("%s thinking is only valid for assistant history", bp)
				}
				if err := requestValidationString(block["thinking"], bp+".thinking", false); err != nil {
					return nil, "", false, err
				}
				if err := requestValidationOptionalStrings(block, bp, "signature"); err != nil {
					return nil, "", false, err
				}
				if block["thinking"] == "" && stringField(block, "signature") != "" {
					return nil, "", false, fmt.Errorf("%s signature-only thinking cannot be restored by this upstream", bp)
				}
				reasoning.WriteString(block["thinking"].(string))
			case "redacted_thinking":
				return nil, "", false, fmt.Errorf("%s redacted thinking cannot be restored by this upstream", bp)
			case "tool_use":
				if role != "assistant" {
					return nil, "", false, fmt.Errorf("%s tool_use requires assistant role", bp)
				}
				id, _ := block["id"].(string)
				name, _ := block["name"].(string)
				if strings.TrimSpace(id) == "" || strings.TrimSpace(name) == "" || seen[id] || pending[id] {
					return nil, "", false, fmt.Errorf("%s requires a unique tool id and name", bp)
				}
				params, err := requestValidationObject(block["input"], bp+".input")
				if err != nil {
					return nil, "", false, err
				}
				args, _ := json.Marshal(params)
				calls = append(calls, map[string]any{"id": id, "type": "function", "function": map[string]any{"name": name, "arguments": string(args)}})
				seen[id] = true
			case "tool_result":
				id, _ := block["tool_use_id"].(string)
				if role != "user" || id == "" || !pending[id] {
					return nil, "", false, fmt.Errorf("%s has no matching pending tool_use", bp)
				}
				if len(parts) > 0 {
					return nil, "", false, fmt.Errorf("%s tool_result blocks must precede other user content", bp)
				}
				if err := requestValidationOptionalBools(block, bp, "is_error"); err != nil {
					return nil, "", false, err
				}
				content := block["content"]
				if content == nil {
					content = ""
				}
				parts, err := anthropicParts(content, bp+".content", true)
				if err != nil {
					return nil, "", false, err
				}
				if block["is_error"] == true {
					// 先剔掉正文为空的 text part 再插入错误标记：标记本身是非空文本，
					// 会让下面的 toolResultContent 认为「有内容」而把空 text part 一起
					// 留下，于是上游收到 `[{text:"[tool execution error]"},{text:""}]`
					// ——正是本文件 toolResultContent 注释里点名要避免的 400 形状
					// （2026-09-30 深度体检发现）。
					parts = append([]any{map[string]any{"type": "text", "text": "[tool execution error]"}}, stripEmptyTextParts(parts)...)
				}
				results = append(results, map[string]any{"role": "tool", "tool_call_id": id, "content": toolResultContent(parts)})
				delete(pending, id)
			default:
				// 顶层的不支持块（document、search_result、web_search_tool_result、
				// server_tool_use、tool_reference 等）同样用文字占位：这些块客户端
				// 一旦用过就会一直留在历史里，硬拒等于让整个会话永久 400。
				part, err := unsupportedPlaceholder(block)
				if err != nil {
					return nil, "", false, err
				}
				parts = append(parts, part)
			}
		}
		if len(pending) > 0 {
			return nil, "", false, fmt.Errorf("%s is missing results for previous tool_use blocks", path)
		}
		if len(parts) == 0 && len(calls) == 0 && len(results) == 0 && reasoning.Len() == 0 {
			return nil, "", false, fmt.Errorf("%s.content must not be empty", path)
		}
		messages = append(messages, results...)
		if len(parts) > 0 || len(calls) > 0 || reasoning.Len() > 0 {
			m := map[string]any{"role": role, "content": parts}
			if len(calls) > 0 {
				m["tool_calls"] = calls
				for _, raw := range calls {
					pending[raw.(map[string]any)["id"].(string)] = true
				}
			}
			if reasoning.Len() > 0 {
				m["reasoning_content"] = reasoning.String()
			}
			messages = append(messages, m)
		}
		turns++
	}
	if len(pending) > 0 {
		return nil, "", false, fmt.Errorf("tool_use history requires matching tool_result blocks")
	}
	if len(systemParts) > 0 {
		if systemEntry != nil {
			existing, _ := systemEntry["content"].([]any)
			systemEntry["content"] = append(existing, systemParts...)
		} else {
			messages = append([]any{map[string]any{"role": "system", "content": systemParts}}, messages...)
		}
	}
	// 只有 system 消息的请求在上游同样无法成立，早点给客户端一个能看懂的错。
	if turns == 0 {
		return nil, "", false, fmt.Errorf("messages must contain at least one user or assistant message")
	}
	chat["messages"] = messages
	if value, exists := source["tools"]; exists && value != nil {
		tools, ok := value.([]any)
		if !ok {
			return nil, "", false, fmt.Errorf("tools must be an array")
		}
		functions := make([]any, 0, len(tools))
		for index, raw := range tools {
			tool, err := requestValidationObject(raw, fmt.Sprintf("tools[%d]", index))
			if err != nil {
				return nil, "", false, err
			}
			// Anthropic 的服务端内置工具（web_search_20250305 等）网关侧没有实现，
			// 但客户端默认就可能带上它。与 Chat / Responses 两条路径保持一致：
			// **接受声明、丢弃不转发**，而不是整条请求 400——否则客户端一开联网搜索
			// 就整个会话不可用（2026-09-29 实测：Claude Code 的 WebSearch 直接 400）。
			if kind, _ := tool["type"].(string); isUnimplementedBuiltinTool(kind) {
				continue
			}
			if kind := tool["type"]; kind != nil && kind != "custom" {
				return nil, "", false, fmt.Errorf("server tools are not supported; provide client tools with input_schema")
			}
			function := map[string]any{"name": tool["name"], "parameters": tool["input_schema"]}
			if tool["description"] != nil {
				function["description"] = tool["description"]
			}
			if tool["strict"] != nil {
				function["strict"] = tool["strict"]
			}
			if _, err := requestValidationObject(tool["input_schema"], "tools.input_schema"); err != nil {
				return nil, "", false, err
			}
			if err := requestValidationFunction(function, "tools"); err != nil {
				return nil, "", false, err
			}
			functions = append(functions, map[string]any{"type": "function", "function": function})
		}
		// 全部声明都是网关不实现的内置工具时不要留空数组：上游对空 tools 的处理
		// 没有保证，而「没有可调用工具」用省略字段表达最清楚（与 Responses 路径一致）。
		if len(functions) > 0 {
			chat["tools"] = functions
		}
	}
	if value := source["tool_choice"]; value != nil {
		choice, err := requestValidationObject(value, "tool_choice")
		if err != nil {
			return nil, "", false, err
		}
		switch choice["type"] {
		case "auto", "none":
			chat["tool_choice"] = choice["type"]
		case "any":
			chat["tool_choice"] = "required"
		case "tool":
			if err := requestValidationString(choice["name"], "tool_choice.name", true); err != nil {
				return nil, "", false, err
			}
			chat["tool_choice"] = map[string]any{"type": "function", "function": map[string]any{"name": choice["name"]}}
		default:
			return nil, "", false, fmt.Errorf("unsupported tool_choice.type")
		}
		if err := requestValidationOptionalBools(choice, "tool_choice", "disable_parallel_tool_use"); err != nil {
			return nil, "", false, err
		}
		if disabled, ok := choice["disable_parallel_tool_use"].(bool); ok {
			chat["parallel_tool_calls"] = !disabled
		}
	}
	encoded, err := json.Marshal(chat)
	return encoded, model, stream, err
}

// NarraFork's official profile asks to keep every thinking block. This exact
// shape needs no server-side state and is already honored by retaining history.
// Extra fields or edits may request real mutations and must not be discarded.
func messagesKeepsAllContext(value any) bool {
	control, ok := value.(map[string]any)
	if !ok || len(control) != 1 {
		return false
	}
	edits, ok := control["edits"].([]any)
	if !ok || len(edits) != 1 {
		return false
	}
	edit, ok := edits[0].(map[string]any)
	return ok && len(edit) == 2 && edit["type"] == "clear_thinking_20251015" && edit["keep"] == "all"
}

func anthropicBlocks(value any, path string) ([]map[string]any, error) {
	if s, ok := value.(string); ok {
		return []map[string]any{{"type": "text", "text": s}}, nil
	}
	array, ok := value.([]any)
	if !ok {
		return nil, fmt.Errorf("%s must be a string or content array", path)
	}
	result := make([]map[string]any, 0, len(array))
	for i, item := range array {
		block, err := requestValidationObject(item, fmt.Sprintf("%s[%d]", path, i))
		if err != nil {
			return nil, err
		}
		result = append(result, block)
	}
	return result, nil
}

func anthropicParts(value any, path string, images bool) ([]any, error) {
	blocks, err := anthropicBlocks(value, path)
	if err != nil {
		return nil, err
	}
	parts := make([]any, 0, len(blocks))
	for i, block := range blocks {
		part, err := anthropicPart(block, fmt.Sprintf("%s[%d]", path, i), images)
		if err != nil {
			return nil, err
		}
		parts = append(parts, part)
	}
	return parts, nil
}

// toolResultContent 归一化工具结果正文：全是空文本时退回空串。
// 空 text part（`[{"type":"text","text":""}]`）是上游常见的 400 触发形状，
// 而"没有输出"本身是合法结果，用空串表达即可。
func toolResultContent(parts []any) any {
	for _, raw := range parts {
		part, ok := raw.(map[string]any)
		if !ok {
			return parts
		}
		if text, _ := part["text"].(string); strings.TrimSpace(text) != "" {
			return parts
		}
		if part["type"] != "text" {
			return parts
		}
	}
	return ""
}

// stripEmptyTextParts 去掉正文为空的 text part，保留其余块。
// 供 is_error 的 tool_result 使用：错误标记插入后整体不再「全空」，
// toolResultContent 不会折叠，此时必须先把空 text part 摘掉。
func stripEmptyTextParts(parts []any) []any {
	kept := make([]any, 0, len(parts))
	for _, raw := range parts {
		if part, ok := raw.(map[string]any); ok {
			if kind, _ := part["type"].(string); kind == "text" {
				if text, _ := part["text"].(string); strings.TrimSpace(text) == "" {
					continue
				}
			}
		}
		kept = append(kept, raw)
	}
	return kept
}

// unsupportedPlaceholder 把上游无法承载的内容块替换成文字占位。
func unsupportedPlaceholder(block map[string]any) (map[string]any, error) {
	kind, _ := block["type"].(string)
	kind = strings.TrimSpace(kind)
	if kind == "" {
		kind = "unknown"
	}
	return map[string]any{
		"type": "text",
		"text": fmt.Sprintf("[unsupported content block: %s — content omitted by the gateway]", kind),
	}, nil
}

func anthropicPart(block map[string]any, path string, images bool) (map[string]any, error) {
	if block["type"] == "text" {
		if err := requestValidationString(block["text"], path+".text", false); err != nil {
			return nil, err
		}
		return map[string]any{"type": "text", "text": block["text"]}, nil
	}
	if block["type"] != "image" {
		// 上游没有对应形态的块（document、tool_reference、search_result 等）用文字占位代替：
		// 客户端读到一次 PDF 或引用后，历史里会一直带着这种块，硬拒会让整段会话永久 400。
		// 明确告诉模型「这里原本有一块内容被省略」，不假装它不存在。
		return unsupportedPlaceholder(block)
	}
	if !images {
		return nil, fmt.Errorf("%s.type is not supported in this content position", path)
	}
	source, err := requestValidationObject(block["source"], path+".source")
	if err != nil {
		return nil, err
	}
	var url string
	switch source["type"] {
	case "url":
		url, _ = source["url"].(string)
		if !strings.HasPrefix(url, "https://") && !strings.HasPrefix(url, "http://") {
			return nil, fmt.Errorf("%s image URL must be HTTP or HTTPS", path)
		}
	case "base64":
		mime, _ := source["media_type"].(string)
		data, _ := source["data"].(string)
		if mime != "image/png" && mime != "image/jpeg" && mime != "image/gif" && mime != "image/webp" {
			return nil, fmt.Errorf("%s unsupported image media_type", path)
		}
		if data == "" {
			return nil, fmt.Errorf("%s image data is empty", path)
		}
		if _, err := io.Copy(io.Discard, base64.NewDecoder(base64.StdEncoding, strings.NewReader(data))); err != nil {
			return nil, fmt.Errorf("%s invalid base64 image", path)
		}
		url = "data:" + mime + ";base64," + data
	default:
		return nil, fmt.Errorf("%s image source must use url or base64", path)
	}
	return map[string]any{"type": "image_url", "image_url": map[string]any{"url": url}}, nil
}
