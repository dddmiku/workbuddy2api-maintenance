// ═══ 更新日志 ═══
// 2026-09-26：已开流时的失败在流内交付（StreamFailure）。
// 2026-09-26：新增 HoldProgress：压制期发心跳。
// 2026-09-26：结束校验只缓存工具调用等精简帧，长回答不再因整条流进 16MB 缓冲而在约 7.4 万分片处被截断。
// 2026-09-25：所有 Chat 请求共享工具声明边界；整组校验前隐藏身份和参数，缓冲有界并在生成期间保活。
// 2026-09-25：保留实际网络 flush 失败，让终态检查与失败用量一致。
// 2026-09-18：直接 Chat 复用工具/结构化输出契约；正文增量保留，成功 finish 与 DONE 在完整校验后发出。
// 2026-09-18：重复终态检查仍返回客户端写失败，避免清理路径把断开误报为成功。
// 2026-09-18：过滤无法执行的内置 Chat 工具声明，无可用工具时去掉仅用于工具的空控制字段。
// 2026-09-18：直接 Chat 的公开长名同步到声明/历史/choice，并在流与JSON响应中恢复原名。
package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"workbuddy2api/internal/jsonutil"
	"workbuddy2api/internal/upstream"
)

func newChatOutputContract(fields map[string]json.RawMessage) (*responsesRequest, error) {
	req := &responsesRequest{ToolChoice: fields["tool_choice"]}
	for key, target := range map[string]any{"model": &req.Model, "tools": &req.Tools, "parallel_tool_calls": &req.ParallelToolCalls} {
		if raw := fields[key]; len(raw) > 0 {
			if err := jsonutil.Decode(raw, target); err != nil {
				return nil, fmt.Errorf("invalid %s: %w", key, err)
			}
		}
	}
	if len(req.Tools) == 0 && len(fields["functions"]) > 0 {
		var functions []any
		if err := jsonutil.Decode(fields["functions"], &functions); err != nil {
			return nil, err
		}
		for _, function := range functions {
			req.Tools = append(req.Tools, map[string]any{"type": "function", "function": function})
		}
	}
	if len(req.ToolChoice) == 0 {
		req.ToolChoice = fields["function_call"]
	}
	if len(req.ToolChoice) > 0 {
		var choice any
		if err := jsonutil.Decode(req.ToolChoice, &choice); err != nil {
			return nil, err
		}
		if object, ok := choice.(map[string]any); ok {
			switch object["type"] {
			case "auto", "none", "required":
				choice = object["type"]
			case "function":
				if function, ok := object["function"].(map[string]any); ok {
					choice = map[string]any{"type": "function", "name": function["name"]}
				}
			case nil:
				// Legacy function_call={name:...} has no type discriminator.
				choice = map[string]any{"type": "function", "name": object["name"]}
			}
		}
		req.ToolChoice, _ = json.Marshal(choice)
	}
	if _, _, err := req.prepareToolPolicy(responsesTools(req.Tools, req)); err != nil {
		return nil, err
	}
	if raw := fields["response_format"]; len(raw) > 0 && string(bytes.TrimSpace(raw)) != "null" {
		var format map[string]any
		if err := jsonutil.Decode(raw, &format); err != nil || format == nil {
			return nil, fmt.Errorf("response_format must be an object")
		}
		if format["type"] == "json_schema" {
			nested, ok := format["json_schema"].(map[string]any)
			if !ok {
				return nil, fmt.Errorf("response_format.json_schema must be an object")
			}
			copy := make(map[string]any, len(nested)+1)
			for key, value := range nested {
				copy[key] = value
			}
			copy["type"] = "json_schema"
			format = copy
		}
		text, _ := json.Marshal(map[string]any{"format": format})
		var err error
		req.output, err = parseOutputContract(text)
		if err != nil {
			return nil, err
		}
	}
	active := req.toolPolicy.mode != "auto" || req.toolPolicy.allowed != nil || len(req.toolPolicy.schemas) > 0 || len(req.toolAliases) > 0
	active = active || (req.ParallelToolCalls != nil && !*req.ParallelToolCalls) || (req.output != nil && req.output.chatFormat != nil)
	if !active {
		return nil, nil
	}
	return req, nil
}

func normalizeChatToolDeclarations(fields map[string]json.RawMessage, req *responsesRequest) (bool, error) {
	changed := false
	names := req.toolAliasIndex()
	rename := func(function map[string]any, declaration bool) bool {
		name, _ := function["name"].(string)
		alias, found := names["\x00"+name]
		if !found || alias == name {
			return false
		}
		function["name"] = alias
		if declaration {
			describeToolAlias(function, name)
		}
		return true
	}
	var tools []any
	if raw := fields["tools"]; len(raw) > 0 {
		if err := jsonutil.Decode(raw, &tools); err != nil {
			return false, err
		}
	}
	kept := make([]any, 0, len(tools))
	for _, raw := range tools {
		tool, _ := raw.(map[string]any)
		kind, _ := tool["type"].(string)
		if isUnimplementedBuiltinTool(kind) {
			changed = true
			continue
		}
		if function, ok := tool["function"].(map[string]any); ok && rename(function, true) {
			changed = true
		}
		kept = append(kept, raw)
	}
	if changed {
		if len(kept) == 0 {
			delete(fields, "tools")
		} else {
			fields["tools"], _ = json.Marshal(kept)
		}
	}
	var legacy []any
	if raw := fields["functions"]; len(raw) > 0 {
		if err := jsonutil.Decode(raw, &legacy); err != nil {
			return false, err
		}
	}
	legacyChanged := false
	for _, raw := range legacy {
		if function, ok := raw.(map[string]any); ok && rename(function, true) {
			legacyChanged = true
		}
	}
	if legacyChanged {
		fields["functions"], _ = json.Marshal(legacy)
		changed = true
	}
	if len(names) > 0 {
		for _, key := range []string{"tool_choice", "function_call"} {
			if raw := fields[key]; len(raw) > 0 {
				var choice any
				if err := jsonutil.Decode(raw, &choice); err != nil {
					return false, err
				}
				choiceChanged := false
				if name, ok := choice.(string); ok {
					if alias, found := names["\x00"+name]; found {
						choice, choiceChanged = alias, true
					}
				} else if object, ok := choice.(map[string]any); ok {
					if function, ok := object["function"].(map[string]any); ok {
						choiceChanged = rename(function, false)
					} else {
						choiceChanged = rename(object, false)
					}
				}
				if choiceChanged {
					fields[key], _ = json.Marshal(choice)
					changed = true
				}
			}
		}
		var messages []any
		if raw := fields["messages"]; len(raw) > 0 {
			if err := jsonutil.Decode(raw, &messages); err != nil {
				return false, err
			}
		}
		historyChanged := false
		for _, raw := range messages {
			message, _ := raw.(map[string]any)
			for _, call := range responseArray(message["tool_calls"]) {
				item, _ := call.(map[string]any)
				if function, ok := item["function"].(map[string]any); ok && rename(function, false) {
					historyChanged = true
				}
			}
			if function, ok := message["function_call"].(map[string]any); ok && rename(function, false) {
				historyChanged = true
			}
			for _, role := range []string{"function", "tool"} {
				if message["role"] == role && rename(message, false) {
					historyChanged = true
				}
			}
		}
		if historyChanged {
			fields["messages"], _ = json.Marshal(messages)
			changed = true
		}
	}
	if len(kept) == 0 && len(legacy) == 0 {
		if _, exists := fields["parallel_tool_calls"]; exists {
			delete(fields, "parallel_tool_calls")
			changed = true
		}
		for _, key := range []string{"tool_choice", "function_call"} {
			if raw := fields[key]; len(raw) > 0 {
				var choice any
				if err := jsonutil.Decode(raw, &choice); err != nil {
					return false, err
				}
				if object, ok := choice.(map[string]any); ok {
					choice = object["type"]
				}
				if choice == nil || choice == "auto" || choice == "none" {
					delete(fields, key)
					changed = true
				}
			}
		}
	}
	return changed, nil
}

// Eager clients execute as soon as argument JSON parses, before a finish marker.
// Stream text/reasoning immediately; release tools only after validating every choice.
type chatContractWriter struct {
	inner         http.ResponseWriter
	req           *responsesRequest
	raw           bytes.Buffer // 只保存校验所需的精简帧（工具调用、结束原因、拒答等），不含整段正文/思考
	marked        map[any]bool // 已为该 choice 记录过「有输出」标记
	buffer        []byte
	usageEnvelope map[string]any // 用量帧的信封字段（id/object/created/model），发送时复用
	usage         map[string]any // 合并后的用量：上游可能分多帧上报，只发一条最终快照
	errorFrame    []byte
	streaming     bool
	checked       bool
	err           error
	writeErr      error
	lastWrite     time.Time
}

func (w *chatContractWriter) Header() http.Header         { return w.inner.Header() }
func (w *chatContractWriter) WriteHeader(status int)      { w.inner.WriteHeader(status) }
func (w *chatContractWriter) Unwrap() http.ResponseWriter { return w.inner }
func (w *chatContractWriter) FinishResponse() error       { return w.CompletionError() }

func (w *chatContractWriter) writeRaw(raw []byte) {
	if w.writeErr == nil {
		_, w.writeErr = w.inner.Write(raw)
		w.lastWrite = time.Now()
	}
}

func (w *chatContractWriter) Write(raw []byte) (int, error) {
	if w.writeErr != nil {
		return 0, w.writeErr
	}
	if !strings.HasPrefix(w.Header().Get("Content-Type"), "text/event-stream") {
		return w.inner.Write(raw)
	}
	w.streaming = true
	if w.err != nil {
		return 0, w.err
	}
	w.buffer = append(w.buffer, raw...)
	if len(w.buffer) > protocolBufferLimit {
		w.err = fmt.Errorf("upstream event exceeds the adapter buffer limit")
		return 0, w.err
	}
	for {
		index := bytes.Index(w.buffer, []byte("\n\n"))
		if index < 0 {
			break
		}
		frame := append([]byte{}, w.buffer[:index+2]...)
		w.buffer = w.buffer[index+2:]
		w.frame(frame)
	}
	if w.writeErr != nil {
		return 0, w.writeErr
	}
	return len(raw), nil
}

func (w *chatContractWriter) frame(raw []byte) {
	line := strings.TrimSpace(string(raw))
	if !strings.HasPrefix(line, "data: ") {
		w.writeRaw(raw)
		return
	}
	payload := strings.TrimPrefix(line, "data: ")
	if payload == "[DONE]" {
		w.keep([]byte("data: [DONE]\n\n"))
		return
	}
	var frame map[string]any
	if err := jsonutil.Decode([]byte(payload), &frame); err != nil {
		w.err = err
		return
	}
	if frame["error"] != nil {
		w.errorFrame = raw
		w.keep(raw)
		return
	}
	w.keepSlim(frame)
	changed := false
	if frameUsage, ok := frame["usage"].(map[string]any); ok {
		// 上游可能把用量拆在多个帧里（首帧只有 prompt_tokens 等）。逐帧原样转发会让
		// 客户端重复计数、或只拿到半截快照；这里合并成一条，收尾时只发一次。
		w.usage = upstream.MergeUsage(w.usage, frameUsage)
		if w.usageEnvelope == nil {
			envelope := make(map[string]any, 4)
			for _, key := range []string{"id", "object", "created", "model"} {
				if value, present := frame[key]; present {
					envelope[key] = value
				}
			}
			w.usageEnvelope = envelope
		}
		frame["usage"] = nil
		changed = true
	}
	hasDelta := false
	choices, _ := frame["choices"].([]any)
	for _, raw := range choices {
		choice, _ := raw.(map[string]any)
		if finish, _ := choice["finish_reason"].(string); finish != "" {
			choice["finish_reason"] = nil
			changed = true
		}
		if delta, _ := choice["delta"].(map[string]any); len(delta) > 0 {
			for _, key := range []string{"tool_calls", "function_call"} {
				if _, exists := delta[key]; exists {
					delete(delta, key)
					changed = true
				}
			}
			hasDelta = hasDelta || len(delta) > 0
		}
	}
	if !hasDelta {
		if time.Since(w.lastWrite) >= protocolProgressInterval {
			w.writeRaw([]byte(": keepalive\n\n"))
			w.Flush()
		}
		return
	}
	if changed {
		encoded, _ := json.Marshal(frame)
		raw = append(append([]byte("data: "), encoded...), '\n', '\n')
	}
	w.writeRaw(raw)
}

// keep 追加一段校验用帧；超过缓冲上限时记录错误（此时只可能是工具参数本身过大）。
func (w *chatContractWriter) keep(frame []byte) {
	if w.err != nil {
		return
	}
	if len(frame) > protocolBufferLimit-w.raw.Len() {
		w.err = fmt.Errorf("upstream tool output exceeds the adapter buffer limit")
		return
	}
	w.raw.Write(frame)
}

// keepSlim 只保留结束校验需要的字段：工具调用、结束原因、拒答、角色和用量。
// 正文与思考不再整段缓存（此前整条流都进缓冲，长回答约 7.4 万分片即被截断）；
// 结构化输出需要校验正文时才保留正文。每个 choice 首次出现输出时留一个占位字符，
// 让聚合仍能区分「有输出但被截断」与「空响应」。
func (w *chatContractWriter) keepSlim(frame map[string]any) {
	keepText := w.req != nil && w.req.output != nil
	slim := map[string]any{}
	for _, key := range []string{"id", "object", "created", "model", "usage"} {
		if value, ok := frame[key]; ok {
			slim[key] = value
		}
	}
	informative := frame["usage"] != nil || w.raw.Len() == 0
	choices := []any{}
	for _, raw := range responseArray(frame["choices"]) {
		choice, _ := raw.(map[string]any)
		out := map[string]any{"index": choice["index"]}
		if finish, ok := choice["finish_reason"]; ok && finish != nil {
			out["finish_reason"] = finish
			informative = true
		}
		delta, _ := choice["delta"].(map[string]any)
		slimDelta := map[string]any{}
		for _, key := range []string{"role", "tool_calls", "function_call", "refusal"} {
			if value, ok := delta[key]; ok && value != nil {
				slimDelta[key] = value
				informative = informative || key != "role"
			}
		}
		for _, key := range []string{"content", "reasoning_content"} {
			text, _ := delta[key].(string)
			if text == "" {
				continue
			}
			if keepText {
				slimDelta[key] = text
				informative = true
			} else if !w.marked[choice["index"]] {
				if w.marked == nil {
					w.marked = map[any]bool{}
				}
				w.marked[choice["index"]] = true
				slimDelta[key] = " "
				informative = true
			}
		}
		out["delta"] = slimDelta
		choices = append(choices, out)
	}
	if !informative {
		return
	}
	slim["choices"] = choices
	encoded, err := json.Marshal(slim)
	if err != nil {
		w.err = err
		return
	}
	w.keep(append(append([]byte("data: "), encoded...), '\n', '\n'))
}

// StreamFailure 在流已经开始时把失败交付在流内（SSE error 帧 + [DONE]），返回 true。
func (w *chatContractWriter) StreamFailure(code, message string) bool {
	if !w.streaming || w.writeErr != nil {
		return false
	}
	encoded, err := json.Marshal(map[string]any{"error": map[string]any{"code": code, "message": message, "type": "api_error"}})
	if err != nil {
		return false
	}
	w.writeRaw(append(append([]byte("data: "), encoded...), '\n', '\n'))
	w.writeRaw([]byte("data: [DONE]\n\n"))
	w.Flush()
	return true
}

// HoldProgress 由 handler 在循环保护压制期调用：已开流时按间隔发一条 SSE 注释心跳，
// 工具调用被整组缓冲时客户端不至于长时间收不到任何字节。
func (w *chatContractWriter) HoldProgress() {
	if w.err != nil || w.writeErr != nil {
		return
	}
	// 仅在流已经开始后发心跳：此前没有任何帧时提前写注释，
	// 会把随后的 JSON 错误响应体污染成不可解析。
	if !w.streaming || time.Since(w.lastWrite) < protocolProgressInterval {
		return
	}
	w.writeRaw([]byte(": keepalive\n\n"))
	w.Flush()
}

func (w *chatContractWriter) restoreToolNames(message map[string]any) bool {
	changed := false
	restore := func(function map[string]any) {
		name, _ := function["name"].(string)
		original, _, _ := w.req.responsesToolName(name)
		if original != name {
			function["name"] = original
			changed = true
		}
	}
	for _, raw := range responseArray(message["tool_calls"]) {
		call, _ := raw.(map[string]any)
		if function, ok := call["function"].(map[string]any); ok {
			restore(function)
		}
	}
	if function, ok := message["function_call"].(map[string]any); ok {
		restore(function)
	}
	return changed
}

func (w *chatContractWriter) PrepareCompletion(chat map[string]any) {
	for _, raw := range responseArray(chat["choices"]) {
		choice, _ := raw.(map[string]any)
		message, _ := choice["message"].(map[string]any)
		if choice["finish_reason"] == "length" || choice["finish_reason"] == "content_filter" {
			delete(message, "tool_calls")
			delete(message, "function_call")
		}
		w.restoreToolNames(message)
	}
}

func (w *chatContractWriter) Flush() { _ = w.FlushError() }

func (w *chatContractWriter) FlushError() error {
	if w.writeErr == nil {
		w.writeErr = flushHTTPResponse(w.inner)
	}
	return w.writeErr
}

func (w *chatContractWriter) ValidateCompletion(chat map[string]any) error {
	choices := responseArray(chat["choices"])
	if len(choices) == 0 {
		return fmt.Errorf("upstream response contains no choices")
	}
	checker := newResponsesWriter(nil, w.req)
	for _, choice := range choices {
		if err := checker.ValidateCompletion(map[string]any{"choices": []any{choice}}); err != nil {
			return err
		}
	}
	return nil
}

func (w *chatContractWriter) CompletionError() error {
	if w.writeErr != nil {
		return w.writeErr
	}
	if w.checked || !w.streaming {
		return w.err
	}
	w.checked = true
	chat, err := upstream.Aggregate(bytes.NewReader(w.raw.Bytes()))
	if err == nil && w.err == nil {
		err = w.ValidateCompletion(chat)
	}
	if w.err == nil {
		w.err = err
	}
	if w.err != nil {
		if len(w.errorFrame) > 0 {
			w.writeRaw(w.errorFrame)
		} else {
			encoded, _ := json.Marshal(map[string]any{"error": map[string]any{"code": "response_contract_violation", "message": w.err.Error(), "type": "upstream_error"}})
			w.writeRaw(append(append([]byte("data: "), encoded...), '\n', '\n'))
		}
	} else {
		w.emitValidatedTools(chat)
		choices := []any{}
		for _, raw := range responseArray(chat["choices"]) {
			choice, _ := raw.(map[string]any)
			choices = append(choices, map[string]any{"index": choice["index"], "delta": map[string]any{}, "finish_reason": choice["finish_reason"]})
		}
		// 交付工具调用时，finish_reason 按 OpenAI 约定归一为 tool_calls：
		// 上游报 stop 时，按 finish_reason 分支的客户端会丢掉工具调用。
		if w.deliversToolCalls(chat) {
			for _, raw := range choices {
				choice, _ := raw.(map[string]any)
				if finish, _ := choice["finish_reason"].(string); finish == "" || finish == "stop" {
					choice["finish_reason"] = "tool_calls"
				}
			}
		}
		terminal := map[string]any{"id": chat["id"], "object": "chat.completion.chunk", "created": chat["created"], "model": chat["model"], "choices": choices, "usage": nil}
		encoded, _ := json.Marshal(terminal)
		w.writeRaw(append(append([]byte("data: "), encoded...), '\n', '\n'))
		if len(w.usage) > 0 {
			usageFrame := map[string]any{"id": chat["id"], "object": "chat.completion.chunk", "created": chat["created"], "model": chat["model"], "choices": []any{}, "usage": w.usage}
			for key, value := range w.usageEnvelope {
				if current, present := usageFrame[key]; !present || current == nil {
					usageFrame[key] = value
				}
			}
			encoded, err := json.Marshal(usageFrame)
			if err == nil {
				w.writeRaw(append(append([]byte("data: "), encoded...), '\n', '\n'))
			}
		}
	}
	w.writeRaw([]byte("data: [DONE]\n\n"))
	w.Flush()
	if w.writeErr != nil {
		return w.writeErr
	}
	return w.err
}

func (w *chatContractWriter) finish() { _ = w.CompletionError() }

// deliversToolCalls 报告这次聚合结果里是否包含可交付的工具调用。
func (w *chatContractWriter) deliversToolCalls(chat map[string]any) bool {
	for _, raw := range responseArray(chat["choices"]) {
		choice, _ := raw.(map[string]any)
		if finish, _ := choice["finish_reason"].(string); finish == "length" || finish == "content_filter" {
			continue
		}
		message, _ := choice["message"].(map[string]any)
		if message == nil {
			continue
		}
		if len(responseArray(message["tool_calls"])) > 0 || legacyResponseFunction(message) != nil {
			return true
		}
	}
	return false
}

func (w *chatContractWriter) emitValidatedTools(chat map[string]any) {
	choices := []any{}
	for _, raw := range responseArray(chat["choices"]) {
		choice, _ := raw.(map[string]any)
		if choice["finish_reason"] == "length" || choice["finish_reason"] == "content_filter" {
			return
		}
		message, _ := choice["message"].(map[string]any)
		delta := map[string]any{}
		if calls := responseArray(message["tool_calls"]); len(calls) > 0 {
			for index, value := range calls {
				call, _ := value.(map[string]any)
				call["index"] = index
				if id, _ := call["id"].(string); id == "" {
					call["id"] = newRespID("call_")
				}
			}
			delta["tool_calls"] = calls
		} else if fn := legacyResponseFunction(message); fn != nil {
			delta["function_call"] = fn
		}
		if len(delta) == 0 {
			continue
		}
		for _, raw := range responseArray(delta["tool_calls"]) {
			call, _ := raw.(map[string]any)
			function, _ := call["function"].(map[string]any)
			if function == nil {
				continue
			}
			if args, _ := function["arguments"].(string); strings.TrimSpace(args) == "" {
				// 无参数工具的合法空串：交给客户端时写成 {}，避免严格客户端 JSON.parse("") 抛错。
				function["arguments"] = "{}"
			}
		}
		w.restoreToolNames(delta)
		choices = append(choices, map[string]any{"index": choice["index"], "delta": delta, "finish_reason": nil})
	}
	if len(choices) == 0 {
		return
	}
	frame := map[string]any{"id": chat["id"], "object": "chat.completion.chunk", "created": chat["created"], "model": chat["model"], "choices": choices, "usage": nil}
	encoded, err := json.Marshal(frame)
	if err != nil {
		w.err = err
		return
	}
	w.writeRaw(append(append([]byte("data: "), encoded...), '\n', '\n'))
}
