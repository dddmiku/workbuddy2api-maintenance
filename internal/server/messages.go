// ═══ 更新日志 ═══
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
	"net/http"
	"strings"

	"workbuddy2api/internal/jsonutil"
	"workbuddy2api/internal/requestlog"
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
	if system, present := source["system"]; present && system != nil {
		parts, err := anthropicParts(system, "system", false)
		if err != nil {
			return nil, "", false, err
		}
		messages = append(messages, map[string]any{"role": "system", "content": parts})
	}
	input, ok := source["messages"].([]any)
	if !ok || len(input) == 0 {
		return nil, "", false, fmt.Errorf("messages must be a nonempty array")
	}
	pending := map[string]bool{}
	seen := map[string]bool{}
	for index, raw := range input {
		path := fmt.Sprintf("messages[%d]", index)
		message, err := requestValidationObject(raw, path)
		if err != nil {
			return nil, "", false, err
		}
		role, _ := message["role"].(string)
		if role != "user" && role != "assistant" {
			return nil, "", false, fmt.Errorf("%s.role must be user or assistant", path)
		}
		blocks, err := anthropicBlocks(message["content"], path+".content")
		if err != nil {
			return nil, "", false, err
		}
		var parts, calls, results []any
		var reasoning strings.Builder
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
				if strings.TrimSpace(id) == "" || strings.TrimSpace(name) == "" || seen[id] {
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
					parts = append([]any{map[string]any{"type": "text", "text": "[tool execution error]"}}, parts...)
				}
				results = append(results, map[string]any{"role": "tool", "tool_call_id": id, "content": parts})
				delete(pending, id)
			default:
				return nil, "", false, fmt.Errorf("%s.type %q is not supported", bp, block["type"])
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
	}
	if len(pending) > 0 {
		return nil, "", false, fmt.Errorf("tool_use history requires matching tool_result blocks")
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
		chat["tools"] = functions
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

func anthropicPart(block map[string]any, path string, images bool) (map[string]any, error) {
	if block["type"] == "text" {
		if err := requestValidationString(block["text"], path+".text", false); err != nil {
			return nil, err
		}
		return map[string]any{"type": "text", "text": block["text"]}, nil
	}
	if block["type"] != "image" || !images {
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
