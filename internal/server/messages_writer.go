// ═══ 更新日志 ═══
// 2026-09-25：将已校验的Chat结果转为Anthropic消息与流事件，错误不产生成功终态，缓存用量避免重复相加。
// 2026-09-25：流式工具保留参数增量并延迟完成，透传心跳与写失败，保留上下文错误类别和缓存创建用量。
// 2026-09-25：按官方 SDK 合同在全量校验后顺序交付工具块，避免并行完成回调错位；首帧不写会残留的临时用量扩展标记。
// 2026-09-25：缓冲长工具期间按实际片段进展补标准心跳，防止下游空闲超时，不提前交付未验证工具。
// 2026-09-25：首帧缓存拆分未知时不抢报普通输入，真实终态保持原值；提供幂等收尾供公共计费路径确认。
package server

import (
	"bytes"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"workbuddy2api/internal/jsonutil"
	"workbuddy2api/internal/upstream"
)

const messagesBufferLimit = 16 << 20

type messagesTool struct {
	id, name  string
	arguments strings.Builder
}

type messagesWriter struct {
	inner                  http.ResponseWriter
	hdr                    http.Header
	status                 int
	model, id              string
	stream, started, ended bool
	buffer                 []byte
	usage                  map[string]any
	tools                  map[int]*messagesTool
	toolBytes              int
	index                  int
	block                  string
	stop                   string
	err                    error
	lastEvent              time.Time
}

func newMessagesWriter(w http.ResponseWriter) *messagesWriter {
	return &messagesWriter{inner: w, hdr: make(http.Header), status: 200, id: "msg_" + rand.Text(), tools: make(map[int]*messagesTool), index: -1}
}

func (m *messagesWriter) Header() http.Header  { return m.hdr }
func (m *messagesWriter) WriteHeader(code int) { m.status = code }
func (m *messagesWriter) Flush()               { _ = m.FlushError() }
func (m *messagesWriter) FlushError() error {
	if !m.started || m.err != nil {
		return m.err
	}
	if err := http.NewResponseController(m.inner).Flush(); err != nil && !errors.Is(err, http.ErrNotSupported) {
		m.err = err
	}
	if checker, ok := m.inner.(interface{ CompletionError() error }); ok && m.err == nil {
		m.err = checker.CompletionError()
	}
	return m.err
}
func (m *messagesWriter) CompletionError() error      { return m.err }
func (m *messagesWriter) Unwrap() http.ResponseWriter { return m.inner }
func (m *messagesWriter) FinishResponse() error {
	m.finish()
	return m.CompletionError()
}

func (m *messagesWriter) Write(data []byte) (int, error) {
	if m.err != nil {
		return 0, m.err
	}
	if m.ended {
		return len(data), nil
	}
	m.buffer = append(m.buffer, data...)
	if len(m.buffer) > messagesBufferLimit {
		m.failure(502, "upstream_response_too_large", "upstream response exceeds the adapter limit")
		return 0, m.err
	}
	if !strings.HasPrefix(m.hdr.Get("Content-Type"), "text/event-stream") {
		var object map[string]any
		if !json.Valid(m.buffer) {
			return len(data), nil
		}
		if err := jsonutil.Decode(m.buffer, &object); err != nil {
			m.failure(502, "upstream_parse", "invalid upstream response")
			return 0, m.err
		}
		m.buffer = nil
		if problem, ok := object["error"].(map[string]any); ok {
			m.upstreamFailure(m.status, problem)
			return len(data), nil
		}
		converted, err := m.convert(object)
		if err != nil {
			m.failure(502, "response_contract_violation", err.Error())
			return 0, m.err
		}
		m.copyHeaders()
		m.ended = true
		m.err = writeJSON(m.inner, m.status, converted)
		return len(data), m.err
	}
	for !m.ended && m.err == nil {
		index, delimiter := messagesFrameEnd(m.buffer)
		if index < 0 {
			break
		}
		frame := string(m.buffer[:index])
		m.buffer = m.buffer[index+delimiter:]
		var lines []string
		comment := false
		frame = strings.ReplaceAll(strings.ReplaceAll(frame, "\r\n", "\n"), "\r", "\n")
		for _, line := range strings.Split(frame, "\n") {
			if strings.HasPrefix(line, "data:") {
				lines = append(lines, strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
			} else if strings.HasPrefix(line, ":") {
				comment = true
			}
		}
		if len(lines) == 0 {
			if comment {
				m.begin()
				m.event("ping", map[string]any{})
			}
			continue
		}
		payload := strings.Join(lines, "\n")
		if payload == "[DONE]" {
			m.complete()
			break
		}
		var object map[string]any
		if err := jsonutil.Decode([]byte(payload), &object); err != nil {
			m.failure(502, "upstream_parse", "invalid stream event")
			break
		}
		if usage, ok := object["usage"].(map[string]any); ok {
			m.usage = upstream.MergeUsage(m.usage, usage)
		}
		if problem, ok := object["error"].(map[string]any); ok {
			m.upstreamFailure(502, problem)
			break
		}
		choices := responseArray(object["choices"])
		if len(choices) > 1 {
			m.failure(502, "response_contract_violation", "messages expects exactly one output choice")
			break
		}
		m.begin()
		for _, raw := range choices {
			choice, _ := raw.(map[string]any)
			if value := choice["index"]; value != nil {
				index, ok := upstream.UsageCount(value)
				if !ok || index != 0 {
					m.failure(502, "response_contract_violation", "messages expects output choice index zero")
					break
				}
			}
			if reason := stringField(choice, "finish_reason"); reason != "" {
				m.stop = reason
			}
			delta, _ := choice["delta"].(map[string]any)
			for _, field := range []string{"reasoning_content", "content", "refusal"} {
				text := stringField(delta, field)
				if text == "" {
					continue
				}
				kind := "text"
				if field == "reasoning_content" {
					kind = "thinking"
				}
				m.text(kind, text)
			}
			for _, raw := range responseArray(delta["tool_calls"]) {
				call, _ := raw.(map[string]any)
				if err := m.toolDelta(call); err != nil {
					m.failure(502, "response_contract_violation", err.Error())
					break
				}
			}
			if delta["function_call"] != nil {
				m.failure(502, "response_contract_violation", "messages requires tool_calls; legacy function_call cannot be replayed with a tool id")
			}
		}
	}
	if m.err != nil {
		return 0, m.err
	}
	return len(data), nil
}

// The common stream writer uses LF, but keeping CRLF and split delimiters
// valid makes this boundary safe for different HTTP writer chunk sizes.
func messagesFrameEnd(buffer []byte) (int, int) {
	index, length := -1, 0
	for _, delimiter := range []string{"\n\n", "\n\r\n", "\r\r"} {
		if at := bytes.Index(buffer, []byte(delimiter)); at >= 0 && (index < 0 || at < index) {
			index, length = at, len(delimiter)
		}
	}
	return index, length
}

func (m *messagesWriter) toolDelta(call map[string]any) error {
	index, ok := upstream.UsageCount(call["index"])
	if !ok {
		return fmt.Errorf("upstream tool index must be a nonnegative integer")
	}
	tool := m.tools[index]
	if tool == nil {
		if len(m.tools) >= 1024 {
			return fmt.Errorf("upstream exceeded the adapter tool count limit")
		}
		tool = &messagesTool{}
		m.tools[index] = tool
	}
	if id := stringField(call, "id"); id != "" {
		if tool.id != "" && id != tool.id {
			return fmt.Errorf("upstream changed a tool call id")
		}
		for previousIndex, previous := range m.tools {
			if previousIndex != index && previous.id == id {
				return fmt.Errorf("upstream reused a tool call id")
			}
		}
		if tool.id == "" {
			m.toolBytes += len(id)
			tool.id = id
		}
	}
	function, _ := call["function"].(map[string]any)
	if name := stringField(function, "name"); name != "" {
		if tool.name != "" && name != tool.name {
			return fmt.Errorf("upstream changed a tool function name")
		}
		if tool.name == "" {
			m.toolBytes += len(name)
			tool.name = name
		}
	}
	if raw, exists := function["arguments"]; exists {
		arguments, ok := raw.(string)
		if !ok {
			return fmt.Errorf("upstream tool arguments must be a JSON string")
		}
		m.toolBytes += len(arguments)
		if m.toolBytes > messagesBufferLimit {
			return fmt.Errorf("tool arguments exceed the adapter limit")
		}
		tool.arguments.WriteString(arguments)
	}
	if m.toolBytes > messagesBufferLimit {
		return fmt.Errorf("tool metadata exceeds the adapter limit")
	}
	// The official SDK's contentBlock callback assumes one open block at a
	// time. Buffer tools until all arguments validate, then deliver complete
	// blocks sequentially; early stops could make an invalid tool executable.
	if m.started && time.Since(m.lastEvent) >= 10*time.Second {
		m.event("ping", map[string]any{})
	}
	return m.err
}

func stringField(object map[string]any, key string) string {
	value, _ := object[key].(string)
	return value
}

func (m *messagesWriter) copyHeaders() {
	for _, key := range []string{"Retry-After", "X-Request-ID"} {
		if value := m.hdr.Get(key); value != "" {
			m.inner.Header().Set(key, value)
		}
	}
	if id := m.inner.Header().Get("X-Request-ID"); id != "" {
		m.inner.Header().Set("Request-ID", id)
	}
}

func (m *messagesWriter) event(kind string, payload map[string]any) {
	if m.err != nil {
		return
	}
	payload["type"] = kind
	data, err := json.Marshal(payload)
	if err == nil {
		_, err = fmt.Fprintf(m.inner, "event: %s\ndata: %s\n\n", kind, data)
		if err == nil {
			m.lastEvent = time.Now()
		}
	}
	m.err = err
	m.Flush()
}

func (m *messagesWriter) begin() {
	if m.started || m.ended || m.err != nil {
		return
	}
	m.copyHeaders()
	m.inner.Header().Set("Content-Type", "text/event-stream")
	m.inner.Header().Set("Cache-Control", "no-cache")
	m.inner.Header().Set("X-Accel-Buffering", "no")
	m.inner.WriteHeader(200)
	m.started = true
	usage := anthropicUsage(m.usage)
	// A gross prompt count without any reported cache split is not a measured
	// noncached count. Keep this initial field provisional until the terminal
	// snapshot instead of later correcting a fabricated split down to zero.
	// An explicitly reported split remains visible; final counters are untouched.
	if _, known := upstream.CachedInputTokens(m.usage); !known {
		usage["input_tokens"] = 0
	}
	// Initial counters are provisional. Official SDKs update standard counters
	// from message_delta but do not merge extension fields, so an early true
	// flag would incorrectly survive even after complete usage was reported.
	delete(usage, "gateway_usage_incomplete")
	m.event("message_start", map[string]any{"message": map[string]any{"id": m.id, "type": "message", "role": "assistant", "model": m.model, "content": []any{}, "stop_reason": nil, "stop_sequence": nil, "usage": usage}})
}

func (m *messagesWriter) closeBlock() {
	if m.block != "" {
		m.event("content_block_stop", map[string]any{"index": m.index})
		m.block = ""
	}
}

func (m *messagesWriter) text(kind, text string) {
	if m.block != kind {
		m.closeBlock()
		m.index++
		m.block = kind
		block := map[string]any{"type": kind, kind: ""}
		if kind == "thinking" {
			block["signature"] = ""
		}
		m.event("content_block_start", map[string]any{"index": m.index, "content_block": block})
	}
	m.event("content_block_delta", map[string]any{"index": m.index, "delta": map[string]any{"type": kind + "_delta", kind: text}})
}

func (m *messagesWriter) complete() {
	if m.ended || m.err != nil {
		return
	}
	if m.stop == "" {
		m.failure(502, "upstream_incomplete", "upstream did not provide a finish reason")
		return
	}
	stop := anthropicStop(m.stop, len(m.tools) > 0)
	if stop == "" || (stop == "tool_use" && len(m.tools) == 0) {
		m.failure(502, "response_contract_violation", "upstream finish reason does not describe a valid message")
		return
	}
	indices := make([]int, 0, len(m.tools))
	for index := range m.tools {
		indices = append(indices, index)
	}
	sort.Ints(indices)
	for _, index := range indices {
		if _, err := m.tools[index].input(); err != nil {
			m.failure(502, "invalid_tool_arguments", err.Error())
			return
		}
	}
	m.begin()
	m.closeBlock()
	for _, index := range indices {
		tool := m.tools[index]
		m.index++
		m.event("content_block_start", map[string]any{"index": m.index, "content_block": map[string]any{"type": "tool_use", "id": tool.id, "name": tool.name, "input": map[string]any{}}})
		m.event("content_block_delta", map[string]any{"index": m.index, "delta": map[string]any{"type": "input_json_delta", "partial_json": tool.arguments.String()}})
		m.event("content_block_stop", map[string]any{"index": m.index})
	}
	m.event("message_delta", map[string]any{"delta": map[string]any{"stop_reason": stop, "stop_sequence": nil}, "usage": anthropicUsage(m.usage)})
	m.event("message_stop", map[string]any{})
	m.ended = true
}

func (t *messagesTool) input() (map[string]any, error) {
	var input map[string]any
	if strings.TrimSpace(t.id) == "" || strings.TrimSpace(t.name) == "" || jsonutil.Decode([]byte(t.arguments.String()), &input) != nil || input == nil {
		return nil, fmt.Errorf("upstream ended with incomplete or invalid tool arguments")
	}
	return input, nil
}

func (m *messagesWriter) convert(chat map[string]any) (map[string]any, error) {
	choices := responseArray(chat["choices"])
	if len(choices) != 1 {
		return nil, fmt.Errorf("messages expects exactly one output choice")
	}
	choice, _ := choices[0].(map[string]any)
	message, _ := choice["message"].(map[string]any)
	if message["function_call"] != nil {
		return nil, fmt.Errorf("messages requires tool_calls; legacy function_call cannot be replayed with a tool id")
	}
	content := make([]any, 0)
	if text := stringField(message, "reasoning_content"); text != "" {
		content = append(content, map[string]any{"type": "thinking", "thinking": text, "signature": ""})
	}
	for _, field := range []string{"content", "refusal"} {
		if text := stringField(message, field); text != "" {
			content = append(content, map[string]any{"type": "text", "text": text})
		}
	}
	calls := responseArray(message["tool_calls"])
	seen := map[string]bool{}
	for _, raw := range calls {
		call, _ := raw.(map[string]any)
		fn, _ := call["function"].(map[string]any)
		tool := messagesTool{id: stringField(call, "id"), name: stringField(fn, "name")}
		tool.arguments.WriteString(stringField(fn, "arguments"))
		if seen[tool.id] {
			return nil, fmt.Errorf("upstream reused a tool call id")
		}
		seen[tool.id] = true
		input, err := tool.input()
		if err != nil {
			return nil, err
		}
		content = append(content, map[string]any{"type": "tool_use", "id": tool.id, "name": tool.name, "input": input})
	}
	stop := anthropicStop(stringField(choice, "finish_reason"), len(calls) > 0)
	if stop == "" || (stop == "tool_use" && len(calls) == 0) {
		return nil, fmt.Errorf("upstream finish reason does not describe a valid message")
	}
	usage, _ := chat["usage"].(map[string]any)
	return map[string]any{"id": m.id, "type": "message", "role": "assistant", "model": m.model, "content": content, "stop_reason": stop, "stop_sequence": nil, "usage": anthropicUsage(usage)}, nil
}

func anthropicUsage(usage map[string]any) map[string]any {
	input, hasInput := upstream.UsageCount(usage["prompt_tokens"])
	output, hasOutput := upstream.UsageCount(usage["completion_tokens"])
	result := map[string]any{"input_tokens": input, "output_tokens": output}
	if cached, ok := upstream.CachedInputTokens(usage); ok {
		if hasInput {
			cached = min(cached, input)
			input -= cached
		}
		result["cache_read_input_tokens"] = cached
	}
	details, _ := usage["prompt_tokens_details"].(map[string]any)
	// These are alternate names for the same measured cache creation count;
	// a cache miss is not evidence that a cache entry was written.
	for _, candidate := range []any{details["cache_write_tokens"], details["cache_creation_tokens"], usage["cache_creation_input_tokens"]} {
		if created, ok := upstream.UsageCount(candidate); ok {
			if hasInput {
				created = min(created, input)
				input -= created
			}
			result["cache_creation_input_tokens"] = created
			break
		}
	}
	result["input_tokens"] = input
	if !hasInput || !hasOutput {
		result["gateway_usage_incomplete"] = true
	}
	return result
}

func anthropicStop(reason string, hasTools bool) string {
	switch reason {
	case "tool_calls", "function_call":
		return "tool_use"
	case "length":
		return "max_tokens"
	case "content_filter":
		return "refusal"
	case "stop":
		if hasTools {
			return "tool_use"
		}
		return "end_turn"
	}
	return ""
}

func (m *messagesWriter) upstreamFailure(status int, problem map[string]any) {
	if detail, ok := upstream.ContextTooLongErrorDetail(problem); ok {
		m.failure(http.StatusBadRequest, "context_length_exceeded", detail)
		return
	}
	if status < 400 || status == http.StatusBadGateway {
		switch stringField(problem, "type") {
		case "invalid_request_error":
			status = http.StatusBadRequest
		case "authentication_error":
			status = http.StatusUnauthorized
		case "permission_error":
			status = http.StatusForbidden
		case "not_found_error":
			status = http.StatusNotFound
		case "rate_limit_error":
			status = http.StatusTooManyRequests
		case "request_too_large":
			status = http.StatusRequestEntityTooLarge
		case "overloaded_error":
			status = http.StatusServiceUnavailable
		}
	}
	message := stringField(problem, "message")
	if message == "" {
		message = stringField(problem, "msg")
	}
	code := stringField(problem, "code")
	if number, ok := problem["code"].(json.Number); ok {
		code = number.String()
	}
	m.failure(status, code, message)
}

func (m *messagesWriter) failure(status int, code, message string) {
	if m.ended {
		return
	}
	if status < 400 || status > 599 {
		status = http.StatusBadGateway
	}
	kind := "api_error"
	switch status {
	case 400, 415, 422, 501:
		kind = "invalid_request_error"
	case 413:
		kind = "request_too_large"
	case 401:
		kind = "authentication_error"
	case 403:
		kind = "permission_error"
	case 404:
		kind = "not_found_error"
	case 429:
		kind = "rate_limit_error"
	case 503, 529:
		kind = "overloaded_error"
	}
	if message == "" {
		message = "request failed"
	}
	payload := map[string]any{"type": "error", "error": map[string]any{"type": kind, "message": message, "code": code}}
	if m.started {
		m.event("error", payload)
	} else {
		m.copyHeaders()
		m.err = writeJSON(m.inner, status, payload)
	}
	m.ended = true
	if m.err == nil {
		m.err = errors.New(message)
	}
}

func (m *messagesWriter) finish() {
	if m.ended {
		return
	}
	if m.err != nil {
		m.ended = true
		return
	}
	if m.stream && m.started {
		m.failure(502, "upstream_incomplete", "upstream stream ended without a complete response")
		return
	}
	m.failure(502, "upstream_parse", "upstream returned an incomplete response")
}
