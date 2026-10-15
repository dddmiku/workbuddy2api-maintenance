// prompt.go — Remote 会话的查询扁平化与客户端工具运行时提示。
//
// 移植自 autumnsentiment/Trae2api-cn 的 trae_client.flatten_query 与
// raw_client.build_runtime_system_prompt / response_style_instruction。
//
// ═══ 为什么 query 必须扁平化 ═══
//
// Remote 端点（POST /chat_sessions）的 initial_message.query 是**纯文本 JSON 数组**，
// 不是 OpenAI 的 messages。工具调用与结果只能以文本标记出现：
//
//	[Client Tool Call 历史]  → "Client tool history (already handled; do not repeat):"
//	[工具结果]              → "[Client Tool Result: {id} {name} status=succeeded|failed]"
//
// 这正好绕开 llm_utils_chat 的 3004（工具交互轮次限制）：只要请求体里还带
// tool_calls / tool_call_id 结构化字段就会被拒（实测见 remote.go 文件头）。
//
// ═══ 为什么无工具也要注入一段指令 ═══
//
// Remote agent 会把整条推理轨迹当答案返回（参考实现的结论）。注入
// responseStyleInstruction 让它只回结论。带工具时改注入完整的 opencode_tool_call
// 文本协议——客户端工具不在 Remote 沙箱里，模型只能用文本块请求调用，
// 由 API 调用方执行后在下一轮回填结果。
package trae

import (
	"encoding/json"
	"fmt"
	"strings"
)

// responseStyleInstruction 简洁答案指令（对齐参考 raw_client.response_style_instruction）。
//
// Remote agent 的 thought 里是完整推理轨迹；不约束的话它会以
// "The user wants…" 开头把过程当答案交付。
func responseStyleInstruction() string {
	return "Response format: your visible reply must contain only the final " +
		"answer. Start with the result itself. Never begin with a restatement " +
		"of the request or a description of your plan. Do not write sentences " +
		"about the user, the question, or yourself, such as \"The user wants\", " +
		"\"The user is asking\", \"Let me\", \"I need to\", \"This doesn't " +
		"require\", or \"No tools needed\". Keep the reasoning steps that the " +
		"answer genuinely needs (calculations, key facts, code) and drop " +
		"everything else. Emit each code block exactly once."
}

// flattenQuery 把 OpenAI messages 扁平化成 Remote 的 query JSON 字符串。
//
// 形态对齐参考实现 trae_client.flatten_query：
//   - system → "[System]\n{content}"
//   - assistant → "[Assistant]\n{content}"，带 tool_calls 时追加
//     "Client tool history (already handled; do not repeat):\n[{id,name,input}]"
//   - tool → "[Client Tool Result: {id} {name} status={succeeded|failed}]\n{content}"
//   - user → 纯文本
//
// **关键**：tool_calls / tool_call_id 只作为文本出现，不保留结构化字段。
func flattenQuery(messages []map[string]any) string {
	var parts []string
	for _, m := range messages {
		role, _ := m["role"].(string)
		role = strings.ToLower(strings.TrimSpace(role))
		if role == "developer" {
			role = "system"
		}
		text := messageText(m["content"])
		switch role {
		case "system":
			parts = append(parts, "[System]\n"+text)
		case "assistant":
			var seg []string
			if text != "" {
				seg = append(seg, text)
			}
			if history := serializeToolCalls(m["tool_calls"]); history != "" {
				seg = append(seg, history)
			}
			if len(seg) > 0 {
				parts = append(parts, "[Assistant]\n"+strings.Join(seg, "\n\n"))
			}
		case "tool", "function":
			id, _ := m["tool_call_id"].(string)
			if id == "" {
				id, _ = m["toolCallId"].(string)
			}
			if id == "" {
				id = "unknown"
			}
			name, _ := m["name"].(string)
			if name == "" {
				name = "tool"
			}
			status := "succeeded"
			if toolResultFailed(m) {
				status = "failed"
			}
			parts = append(parts, fmt.Sprintf("[Client Tool Result: %s %s status=%s]\n%s", id, name, status, text))
		default:
			if text != "" {
				parts = append(parts, text)
			}
		}
	}
	joined := strings.Join(parts, "\n\n")
	encoded, err := json.Marshal([]any{map[string]any{"type": "text", "data": map[string]any{"content": joined}}})
	if err != nil {
		return `[{"type":"text","data":{"content":""}}]`
	}
	return string(encoded)
}

// flattenQueryChars 返回扁平化 query 的字符数（裁剪判据用，避免重复构造）。
func flattenQueryChars(messages []map[string]any) int {
	return len(strings.Join(flattenedParts(messages), "\n\n"))
}

// flattenedParts 只取文本段（与 flattenQuery 同逻辑，省掉 JSON 编码）。
func flattenedParts(messages []map[string]any) []string {
	var parts []string
	for _, m := range messages {
		role, _ := m["role"].(string)
		role = strings.ToLower(strings.TrimSpace(role))
		if role == "developer" {
			role = "system"
		}
		text := messageText(m["content"])
		switch role {
		case "system":
			parts = append(parts, "[System]\n"+text)
		case "assistant":
			var seg []string
			if text != "" {
				seg = append(seg, text)
			}
			if history := serializeToolCalls(m["tool_calls"]); history != "" {
				seg = append(seg, history)
			}
			if len(seg) > 0 {
				parts = append(parts, "[Assistant]\n"+strings.Join(seg, "\n\n"))
			}
		case "tool", "function":
			id, _ := m["tool_call_id"].(string)
			if id == "" {
				id, _ = m["toolCallId"].(string)
			}
			if id == "" {
				id = "unknown"
			}
			name, _ := m["name"].(string)
			if name == "" {
				name = "tool"
			}
			status := "succeeded"
			if toolResultFailed(m) {
				status = "failed"
			}
			parts = append(parts, fmt.Sprintf("[Client Tool Result: %s %s status=%s]\n%s", id, name, status, text))
		default:
			if text != "" {
				parts = append(parts, text)
			}
		}
	}
	return parts
}

// serializeToolCalls 把 assistant 的 tool_calls 转成惰性历史文本。
//
// 对齐参考 raw_client._serialize_tool_calls：
// "Client tool history (already handled; do not repeat):\n[{\"id\",\"name\",\"input\"}]"
func serializeToolCalls(value any) string {
	calls, ok := value.([]any)
	if !ok || len(calls) == 0 {
		return ""
	}
	out := make([]map[string]any, 0, len(calls))
	for _, raw := range calls {
		call, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		fn, _ := call["function"].(map[string]any)
		if fn == nil {
			fn = call
		}
		name, _ := fn["name"].(string)
		if name == "" {
			continue
		}
		id, _ := call["id"].(string)
		if id == "" {
			id, _ = call["tool_call_id"].(string)
		}
		input := fn["arguments"]
		if input == nil {
			input = fn["input"]
		}
		if input == nil {
			input = map[string]any{}
		}
		if encoded, ok := input.(string); ok {
			var decoded any
			if json.Unmarshal([]byte(encoded), &decoded) == nil {
				input = decoded
			}
		}
		out = append(out, map[string]any{"id": id, "name": name, "input": input})
	}
	if len(out) == 0 {
		return ""
	}
	encoded, err := json.Marshal(out)
	if err != nil {
		return ""
	}
	return "Client tool history (already handled; do not repeat):\n" + string(encoded)
}

// toolResultFailed 判定一条工具结果消息是否显式报告失败。
//
// 对齐参考 cli_client.tool_result_is_failed：is_error/isError/failed 为真、
// success 为假、status 落在失败词表、tool_result 块上带 isError 标记，
// 或正文 JSON 里 status 是失败词。
func toolResultFailed(m map[string]any) bool {
	for _, key := range []string{"is_error", "isError", "failed"} {
		if truthyValue(m[key]) {
			return true
		}
	}
	if v, present := m["success"]; present && !truthyValue(v) && v != nil {
		if b, ok := v.(bool); ok && !b {
			return true
		}
	}
	if status, ok := m["status"].(string); ok && isToolFailureStatus(status) {
		return true
	}
	// Trae 渲染器把失败标记放在 tool_result 块上（{type:"tool_result", isError:true}）。
	if blocks, ok := m["content"].([]any); ok {
		for _, raw := range blocks {
			block, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			for _, key := range []string{"is_error", "isError"} {
				if truthyValue(block[key]) {
					return true
				}
			}
		}
	}
	return false
}

func truthyValue(v any) bool {
	switch t := v.(type) {
	case bool:
		return t
	case string:
		switch strings.ToLower(strings.TrimSpace(t)) {
		case "true", "1", "yes":
			return true
		}
	}
	return false
}

func isToolFailureStatus(status string) bool {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "error", "failed", "failure", "cancelled", "canceled",
		"timeout", "timed_out", "timed-out":
		return true
	}
	return false
}

// messageText 把一条消息的 content 压成纯文本（对齐参考 _content_to_text）。
//
// 块数组逐块取 text/content/value；image 块描述为占位；其它标量转字符串。
func messageText(value any) string {
	switch typed := value.(type) {
	case nil:
		return ""
	case string:
		return typed
	case []any:
		var out []string
		for _, raw := range typed {
			switch block := raw.(type) {
			case string:
				out = append(out, block)
			case map[string]any:
				if text := contentBlockText(block); text != "" {
					out = append(out, text)
				} else if kind, _ := block["type"].(string); strings.EqualFold(kind, "image") {
					out = append(out, "[image omitted]")
				} else if kind, _ := block["type"].(string); kind == "file" {
					out = append(out, "[Unsupported file input omitted]")
				}
			}
		}
		return strings.Join(out, "\n")
	case map[string]any:
		encoded, err := json.Marshal(typed)
		if err != nil {
			return fmt.Sprint(typed)
		}
		return string(encoded)
	default:
		return fmt.Sprint(typed)
	}
}

// contentBlockText 从一个内容块取文本（text/content/value 三种拼写都认）。
func contentBlockText(block map[string]any) string {
	for _, key := range []string{"text", "content"} {
		if text, ok := block[key].(string); ok && text != "" {
			return text
		}
	}
	// Trae 渲染器块用 value 承载文本与工具结果（{type:"text",value:...}）。
	if value, ok := block["value"]; ok {
		switch typed := value.(type) {
		case string:
			return typed
		case []any:
			var out []string
			for _, raw := range typed {
				switch item := raw.(type) {
				case string:
					out = append(out, item)
				case map[string]any:
					for _, key := range []string{"text", "content", "value"} {
						if text, ok := item[key].(string); ok && text != "" {
							out = append(out, text)
							break
						}
					}
				}
			}
			return strings.Join(out, "\n")
		}
	}
	return ""
}

// ── 客户端工具运行时提示 ────────────────────────────────────────────────

// buildRuntimeSystemPrompt 构造客户端工具运行时的 system 提示。
//
// 对齐参考 raw_client.build_runtime_system_prompt 的**文本协议分支**：
// Remote 端点不透传 tools 结构，客户端工具以 opencode_tool_call 文本块协议
// 表达，由 API 调用方执行。无工具时退化为 responseStyleInstruction。
func buildRuntimeSystemPrompt(tools []any, toolChoice any, parallelToolCalls any) string {
	defs := normalizeToolDefinitions(tools)
	var lines []string
	lines = append(lines,
		"You are Trae Code connected to the user's local application.",
		"The external Trae-compatible terminal client application that started this conversation provides the client tools below.",
		"Caller client context: the client executes tool calls and returns results in the next turn.",
		"Discover the local environment through the available client tools when the request requires it.",
		"If a tool can answer the question, call it proactively and wait for the external client result.",
		"Remote or server-side tools cannot write into the caller workspace. Never claim that a client file was downloaded, created, edited, patched, or saved unless a later matching client tool result with the same call id confirms success.",
		"A tool request, emitted tool-call block, plan, or assistant narration is not execution proof. If the matching client result reports failure, state the failure or request a corrective client tool; never report the operation as completed.",
		"Never repeat a completed tool call; do not describe that server's Linux filesystem as the caller workspace.",
	)
	if len(defs) > 0 {
		lines = append(lines,
			"Client tools are NOT native/server functions: never invoke them through function calling or any built-in tool, and never fall back to server tools (LS, Glob, Read, terminal) to reach the caller's files. The only way to call a client tool is the text block below; a native call reports 'not available' because the tool lives on the client.",
			"To use tools, emit only one or more JSON blocks and include no prose or final answer in that turn:",
			`<opencode_tool_call>{"name":"tool_name","input":{}}</opencode_tool_call>`,
			"Do not invent an id field; the client assigns call ids. Keep file paths exactly as the user or tool result gave them.",
			"Fill input according to the selected tool schema and wait for the client result before continuing.",
		)
		payload, err := json.Marshal(defs)
		if err == nil {
			lines = append(lines,
				"Available client tool definitions and input schemas (JSON):",
				string(payload),
				`To call a tool, emit one block per call in this format and no prose or final answer in the same turn: <opencode_tool_call>{"name":"tool_name","input":{}}</opencode_tool_call>. Omit the id field (the client assigns it), use an exact declared tool name, and fill input according to that tool's schema.`,
				"After the client returns a tool result, continue from that result until the user's request is complete.",
			)
		}
	} else {
		lines = append(lines, "No client tools are available for this request; answer directly.")
	}
	switch choice := toolChoice.(type) {
	case string:
		switch strings.ToLower(strings.TrimSpace(choice)) {
		case "none":
			lines = append(lines, "Tool choice is none: do not request a tool.")
		case "required":
			lines = append(lines, "Tool choice is required: request an available client tool before answering.")
		}
	case map[string]any:
		if fn, ok := choice["function"].(map[string]any); ok {
			if name, _ := fn["name"].(string); name != "" {
				lines = append(lines, "Tool choice requires the client tool named "+name+".")
			}
		} else if name, _ := choice["name"].(string); name != "" {
			lines = append(lines, "Tool choice requires the client tool named "+name+".")
		}
	}
	if parallel, ok := toolChoice.(bool); !ok {
		_ = parallel
	}
	if ptc, ok := parallelToolCalls.(bool); ok && !ptc {
		lines = append(lines, "Request at most one tool per turn.")
	}
	lines = append(lines, responseStyleInstruction())
	return strings.Join(lines, "\n")
}

// normalizeToolDefinitions 把 OpenAI tools 归一成紧凑定义清单（name/description/parameters）。
func normalizeToolDefinitions(tools []any) []map[string]any {
	out := make([]map[string]any, 0, len(tools))
	for _, raw := range tools {
		tool, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		fn, _ := tool["function"].(map[string]any)
		if fn == nil {
			// 紧凑形态：顶层就是函数定义。
			fn = tool
		}
		name, _ := fn["name"].(string)
		if name == "" {
			continue
		}
		def := map[string]any{"name": name}
		if desc, ok := fn["description"].(string); ok && desc != "" {
			def["description"] = desc
		}
		params := fn["parameters"]
		if params == nil {
			params = fn["input_schema"]
		}
		if params == nil {
			params = fn["inputSchema"]
		}
		switch p := params.(type) {
		case string:
			var decoded any
			if json.Unmarshal([]byte(p), &decoded) == nil {
				def["parameters"] = decoded
			} else {
				def["parameters"] = map[string]any{}
			}
		case map[string]any:
			def["parameters"] = p
		default:
			def["parameters"] = map[string]any{}
		}
		out = append(out, def)
	}
	return out
}

// toolProtocolRequested 报告请求（或历史）是否携带工具协议。
//
// 对齐参考 _tool_protocol_requested + _message_contains_tool_protocol：
// tools/tool_choice/parallel_tool_calls 任一存在，或历史里有 role=tool /
// assistant.tool_calls / tool_use 块，都算。
func toolProtocolRequested(src map[string]any) bool {
	for _, key := range []string{"tools", "tool_choice", "parallel_tool_calls"} {
		if v, ok := src[key]; ok && v != nil {
			if arr, isArr := v.([]any); !isArr || len(arr) > 0 {
				return true
			}
		}
	}
	messages, _ := src["messages"].([]any)
	for _, raw := range messages {
		m, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		role, _ := m["role"].(string)
		role = strings.ToLower(strings.TrimSpace(role))
		if role == "tool" || role == "function" {
			return true
		}
		if role == "assistant" {
			if calls, ok := m["tool_calls"].([]any); ok && len(calls) > 0 {
				return true
			}
			if _, ok := m["function_call"].(map[string]any); ok {
				return true
			}
		}
		if blocks, ok := m["content"].([]any); ok {
			for _, b := range blocks {
				block, ok := b.(map[string]any)
				if !ok {
					continue
				}
				switch kind, _ := block["type"].(string); strings.ToLower(strings.TrimSpace(kind)) {
				case "tool_use", "tool_call", "function_call", "tool_result":
					return true
				}
			}
		}
	}
	return false
}

// prepareRemoteMessages 组装 Remote 会话的消息序列：注入运行时提示 + 裁剪。
//
// 返回 (注入后的消息, 是否带工具协议)。
func prepareRemoteMessages(messages []map[string]any, src map[string]any) ([]map[string]any, bool) {
	withTools := toolProtocolRequested(src)
	var prompt string
	if withTools {
		var tools []any
		if raw, ok := src["tools"].([]any); ok {
			tools = raw
		}
		prompt = buildRuntimeSystemPrompt(tools, src["tool_choice"], src["parallel_tool_calls"])
	} else {
		prompt = responseStyleInstruction()
	}
	out := make([]map[string]any, 0, len(messages)+1)
	out = append(out, map[string]any{"role": "system", "content": prompt})
	out = append(out, messages...)
	return out, withTools
}

// compactRemoteHistory 有界历史压缩（对齐参考 _compact_raw_history 的语义简化版）。
//
// 保全部 system（去重）+ 最近 maxMessages 条非 system；再按字符预算从最旧往回丢。
// 孤儿工具结果（配对的 assistant 调用被丢掉时）连带处理。
func compactRemoteHistory(messages []map[string]any, maxMessages int, maxChars int) []map[string]any {
	if maxMessages <= 0 && maxChars <= 0 {
		return messages
	}
	var systems, rest []map[string]any
	seenSystem := map[string]bool{}
	for _, m := range messages {
		if role, _ := m["role"].(string); strings.EqualFold(strings.TrimSpace(role), "system") {
			key := messageText(m["content"])
			if !seenSystem[key] {
				seenSystem[key] = true
				systems = append(systems, m)
			}
		} else {
			rest = append(rest, m)
		}
	}
	if maxMessages > 0 && len(rest) > maxMessages {
		rest = rest[len(rest)-maxMessages:]
	}
	if maxChars > 0 {
		systemChars := 0
		for _, m := range systems {
			systemChars += len(messageText(m["content"]))
		}
		remaining := maxChars - systemChars
		if remaining < 0 {
			remaining = 0
		}
		used := 0
		kept := make([]map[string]any, 0, len(rest))
		for i := len(rest) - 1; i >= 0; i-- {
			size := len(messageText(rest[i]["content"])) + len(serializeToolCalls(rest[i]["tool_calls"]))
			if len(kept) > 0 && used+size > remaining {
				break
			}
			kept = append(kept, rest[i])
			used += size
		}
		// 反转回正序。
		for i, j := 0, len(kept)-1; i < j; i, j = i+1, j-1 {
			kept[i], kept[j] = kept[j], kept[i]
		}
		rest = kept
	}
	// 不让保留序列以孤儿工具结果开头（配对的调用是它前面那条时一并保留）。
	if len(rest) > 0 {
		if role, _ := rest[0]["role"].(string); role == "tool" || role == "function" {
			// 找 rest[0] 在原序列里前一条：若是带 tool_calls 的 assistant，插回。
			for i, m := range messages {
				if sameMessage(m, rest[0]) && i > 0 {
					prev := messages[i-1]
					if prole, _ := prev["role"].(string); prole == "assistant" {
						if calls, ok := prev["tool_calls"].([]any); ok && len(calls) > 0 {
							rest = append([]map[string]any{prev}, rest...)
						}
					}
					break
				}
			}
		}
	}
	out := make([]map[string]any, 0, len(systems)+len(rest))
	out = append(out, systems...)
	out = append(out, rest...)
	return out
}

func sameMessage(a, b map[string]any) bool {
	return fmt.Sprint(a["tool_call_id"]) == fmt.Sprint(b["tool_call_id"]) &&
		fmt.Sprint(a["role"]) == fmt.Sprint(b["role"]) &&
		messageText(a["content"]) == messageText(b["content"])
}

// boundedRemoteQuery 把扁平化 query 裁到上限内（对齐参考 _bounded_remote_query）。
//
// 从最旧的非 system 消息开始丢（成对丢 assistant(tool_calls)+tool），
// 直到 flattenQuery 落到上限内。system 始终保留。
func boundedRemoteQuery(messages []map[string]any, maxChars int) ([]map[string]any, int) {
	if maxChars <= 0 {
		return messages, 0
	}
	bounded := messages
	removed := 0
	for flattenQueryChars(bounded) > maxChars {
		var nonSystem []int
		for i, m := range bounded {
			if role, _ := m["role"].(string); !strings.EqualFold(strings.TrimSpace(role), "system") {
				nonSystem = append(nonSystem, i)
			}
		}
		if len(nonSystem) <= 1 {
			return bounded, removed
		}
		head := nonSystem[0]
		drop := map[int]bool{head: true}
		if role, _ := bounded[head]["role"].(string); role == "assistant" {
			if calls, ok := bounded[head]["tool_calls"].([]any); ok && len(calls) > 0 && head+1 < len(bounded) {
				if nextRole, _ := bounded[head+1]["role"].(string); nextRole == "tool" {
					drop[head+1] = true
				}
			}
		}
		next := make([]map[string]any, 0, len(bounded))
		for i, m := range bounded {
			if !drop[i] {
				next = append(next, m)
			}
		}
		removed += len(drop)
		bounded = next
	}
	return bounded, removed
}
