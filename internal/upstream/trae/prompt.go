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

// ── 图片：Remote 的 query 也能承载 renderer media 块 ──────────────────────
//
// 移植自参考实现 images.query_parts / flatten_query_parts。
//
// 为什么不能只靠 messageText 的占位符：旧版把图片压成 "[image omitted]" 文本，
// 模型收到的是「这里本来有张图」这句话——它**看不到图**，于是 glm-5.3 明明支持
// 读图却答不出图里有什么（2026-10-16 用户实测报障）。
//
// Remote 的 query 是 JSON 数组，文本部件是 {"type":"text","data":{"content":...}}；
// 图片部件是渲染器媒体形状 {"type":"image_url","data":{"mimeType","data"}}——
// 上游 agent 认这个形状（与原生通道同一套 payload）。图片跟在文本之后，
// 另附一条短占位文本，让非多模态读者也知道「这里附了图」。
//
// 注意：图片**不计入** query 裁剪的字符预算（对齐参考 _bounded_remote_query ——
// 它也只按 flatten_query 的纯文本长度裁剪）。base64 体积大，计入会让长历史被
// 误裁；上游对 query 的上限约束针对的是文本历史部分。

// rendererMediaBlock 把一个图片部件转成渲染器媒体块（返回 nil 表示不是图片）。
func rendererMediaBlock(block map[string]any) map[string]any {
	kind, _ := block["type"].(string)
	switch strings.ToLower(strings.TrimSpace(kind)) {
	case "image":
		// Anthropic 形态：{"type":"image","source":{"type":"base64","media_type","data"}}
		source, _ := block["source"].(map[string]any)
		if source == nil {
			return nil
		}
		switch strings.ToLower(strings.TrimSpace(stringFieldOf(source, "type"))) {
		case "base64":
			data := compactBase64(stringFieldOf(source, "data"))
			if data == "" {
				return nil
			}
			return mediaBlock(data, firstNonEmpty(stringFieldOf(source, "media_type"), defaultImageMIME))
		case "url":
			url := stringFieldOf(source, "url")
			if url == "" {
				return nil
			}
			return mediaBlock(url, "url")
		}
		return nil
	case "image_url", "input_image":
		// OpenAI 形态：{"type":"image_url","image_url":{"url":"data:..."}}
		url := imageURLOf(block)
		if url == "" {
			return nil
		}
		if strings.HasPrefix(url, "data:") {
			mime, data := splitDataURL(url)
			if data == "" {
				return nil
			}
			return mediaBlock(data, firstNonEmpty(mime, defaultImageMIME))
		}
		// 远程 URL 网关不代下：原样转发仍好过静默丢图（对齐参考）。
		return mediaBlock(url, "url")
	}
	return nil
}

const defaultImageMIME = "image/png"

// mediaBlock 构造渲染器媒体块。
func mediaBlock(data, mime string) map[string]any {
	return map[string]any{
		"type":  "image_url",
		"value": map[string]any{"mimeType": mime, "data": data},
	}
}

// imageURLOf 从 OpenAI 图片部件取 URL（三种拼写）。
func imageURLOf(block map[string]any) string {
	if image, ok := block["image_url"].(map[string]any); ok {
		if url, _ := image["url"].(string); url != "" {
			return url
		}
	}
	if image, ok := block["image_url"].(string); ok && image != "" {
		return image
	}
	for _, key := range []string{"url", "image", "image_url"} {
		if url, ok := block[key].(string); ok && url != "" {
			return url
		}
	}
	return ""
}

// splitDataURL 拆 data URL 成 (mime, base64)。
func splitDataURL(url string) (string, string) {
	trimmed := strings.TrimSpace(url)
	if !strings.HasPrefix(trimmed, "data:") {
		return "", ""
	}
	rest := trimmed[len("data:"):]
	semicolon := strings.Index(rest, ";")
	if semicolon < 0 {
		return "", ""
	}
	mime := rest[:semicolon]
	after := rest[semicolon+1:]
	if !strings.HasPrefix(after, "base64,") {
		return "", ""
	}
	return mime, compactBase64(after[len("base64,"):])
}

// compactBase64 去掉 base64 里的空白（换行的 data URL 很常见）。
func compactBase64(value string) string {
	if !strings.ContainsAny(value, " \t\r\n") {
		return value
	}
	var out strings.Builder
	out.Grow(len(value))
	for _, r := range value {
		switch r {
		case ' ', '\t', '\r', '\n':
		default:
			out.WriteRune(r)
		}
	}
	return out.String()
}

// stringFieldOf 取 map 里的字符串字段。
func stringFieldOf(obj map[string]any, key string) string {
	value, _ := obj[key].(string)
	return value
}

// firstNonEmpty 返回第一个非空串。
func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

// queryMediaParts 收集消息里所有图片，转成 query 的媒体部件 + 占位文本。
func queryMediaParts(messages []map[string]any) []any {
	var parts []any
	for _, m := range messages {
		content, ok := m["content"].([]any)
		if !ok {
			continue
		}
		for _, raw := range content {
			block, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			media := rendererMediaBlock(block)
			if media == nil {
				continue
			}
			value, _ := media["value"].(map[string]any)
			parts = append(parts, map[string]any{
				"type": "image_url",
				"data": map[string]any{
					"mimeType": firstNonEmpty(stringFieldOf(value, "mimeType"), defaultImageMIME),
					"data":     stringFieldOf(value, "data"),
				},
			})
			parts = append(parts, map[string]any{
				"type": "text",
				"data": map[string]any{"content": describeMedia(value)},
			})
		}
	}
	return parts
}

// buildRemoteContent 组装 Remote 会话体的 content 数组（结构化半边）。
//
// 形态对齐参考 build_web_content：**文本与图片都放这里**，按消息顺序交替排列。
//   - 文本部件 {"type":"text","data":{"content":...}}
//   - 图片部件 {"type":"image_url","value":{"mimeType","data"}}（渲染器媒体原形）
//
// 2026-10-16 实测：只把图片放进 query（或只放图片、不放文本）上游都读不到图——
// 模型回「没有收到图片」，连目录标了 multimodal:true 的 kimi-k2.6 也一样。
// 参考实现的两个会话创建函数（create_web_session / create_session）都把 content
// 填成完整结构化历史，本函数对齐这个口径。
func buildRemoteContent(messages []map[string]any) []any {
	items := []any{}
	for _, m := range messages {
		content, ok := m["content"].([]any)
		if !ok {
			// 字符串 content：整条当文本部件。
			if text, ok := m["content"].(string); ok && text != "" {
				items = append(items, map[string]any{
					"type": "text",
					"data": map[string]any{"content": text},
				})
			}
			continue
		}
		for _, raw := range content {
			block, ok := raw.(map[string]any)
			if !ok {
				if text, ok := raw.(string); ok && text != "" {
					items = append(items, map[string]any{
						"type": "text",
						"data": map[string]any{"content": text},
					})
				}
				continue
			}
			if renderer := rendererMediaBlock(block); renderer != nil {
				items = append(items, renderer)
				continue
			}
			if text := contentBlockText(block); text != "" {
				items = append(items, map[string]any{
					"type": "text",
					"data": map[string]any{"content": text},
				})
			}
		}
	}
	return items
}

// describeMedia 生成图片的短标签（不泄露 base64 内容）。
func describeMedia(value map[string]any) string {
	mime := firstNonEmpty(stringFieldOf(value, "mimeType"), defaultImageMIME)
	data := stringFieldOf(value, "data")
	if mime == "url" || strings.HasPrefix(data, "http") {
		if len(data) > 120 {
			data = data[:120]
		}
		return "[image: " + data + "]"
	}
	if size := base64DecodedLen(data); size > 0 {
		return fmt.Sprintf("[image: %s, %d bytes]", mime, size)
	}
	return "[image: " + mime + "]"
}

// base64DecodedLen 估算 base64 串解码后的字节数（不实际解码）。
func base64DecodedLen(data string) int {
	if data == "" {
		return 0
	}
	padding := 0
	if strings.HasSuffix(data, "==") {
		padding = 2
	} else if strings.HasSuffix(data, "=") {
		padding = 1
	}
	return len(data)/4*3 - padding
}

// flattenQueryWithImages 扁平化 query，并在文本之后附上图片媒体部件。
//
// 无图片时与 flattenQuery 完全等价（不改变既有行为）。
//
// 有图片时文本半边**去掉图片占位符**（对齐参考 raw_client 的 _without_native_images）：
// 媒体部件已经把图带给模型了，文本里再留一句 "[image omitted]" 只会让它以为图丢了。
func flattenQueryWithImages(messages []map[string]any) string {
	media := queryMediaParts(messages)
	if len(media) == 0 {
		return flattenQuery(messages)
	}
	var textParts []any
	if json.Unmarshal([]byte(flattenQuery(stripImageParts(messages))), &textParts) != nil {
		textParts = nil
	}
	// 文本历史在前（模型先读对话），图片按调用方附加顺序在后。
	combined := append(textParts, media...)
	encoded, err := json.Marshal(combined)
	if err != nil {
		return flattenQuery(messages)
	}
	return string(encoded)
}

// stripImageParts 返回去掉图片块的消息副本（文本半边的扁平化用）。
//
// 只动 content 数组里的图片块，其它字段与块原样保留。
func stripImageParts(messages []map[string]any) []map[string]any {
	out := make([]map[string]any, 0, len(messages))
	for _, m := range messages {
		content, ok := m["content"].([]any)
		if !ok {
			out = append(out, m)
			continue
		}
		kept := make([]any, 0, len(content))
		for _, raw := range content {
			if block, ok := raw.(map[string]any); ok && rendererMediaBlock(block) != nil {
				continue
			}
			kept = append(kept, raw)
		}
		if len(kept) == len(content) {
			out = append(out, m)
			continue
		}
		copied := make(map[string]any, len(m))
		for key, value := range m {
			copied[key] = value
		}
		copied["content"] = kept
		out = append(out, copied)
	}
	return out
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
