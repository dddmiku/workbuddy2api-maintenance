// ═══ 更新日志 ═══
// 2026-09-26：已开流时的失败在流内交付（StreamFailure）。
// 2026-09-26：新增 HoldProgress：压制期先开流或 ping。
// 2026-09-26：上游已发出但首帧迟到时先开流（message_start + ping），并在静默期周期性 ping；writer 加锁。
// 2026-09-25：保留逐密钥频率与并发响应头，客户端可读取共享额度与重试提示。
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
	"sync"
	"time"

	"workbuddy2api/internal/jsonutil"
	"workbuddy2api/internal/upstream"
)

// anthropicFirstFrameGrace 上游请求已发出、却迟迟没有首帧时，先把流开起来的等待时长。
// 循环保护最多会压住 60 秒推理输出，期间客户端一个字节都收不到；先发
// message_start + ping 让客户端的空闲计时器有东西可吃。
//
// 用变量而非常量：测试里缩短等待，避免每个用例多花 5 秒。
var anthropicFirstFrameGrace = 5 * time.Second

// anthropicIdlePing 静默期的 ping 间隔（Anthropic 官方流也会周期性发 ping）。
var anthropicIdlePing = 10 * time.Second

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
	delivered              bool
	// promptEstimate 出站请求体的输入量估算（由 messagesEntry 在转换后填入）。
	// 只用于 message_start 的**临时** input_tokens：没有它，首帧只能报 0，
	// Claude Code 的 workflow 会把每个 agent 的 token 计数一直显示成 0 tok，
	// 直到轮次结束才跳变。终态（message_delta）永远用上游实报值，与它无关。
	promptEstimate int64
	mu             sync.Mutex
	keepAlive      bool
	done           chan struct{}
	// 构造时快照的保活时序：goroutine 只读字段，不去读包级变量（否则测试改动
	// 包级变量时会与 goroutine 竞争）。
	firstFrameGrace time.Duration
	idlePing        time.Duration
}

func newMessagesWriter(w http.ResponseWriter) *messagesWriter {
	return &messagesWriter{inner: w, hdr: make(http.Header), status: 200, id: "msg_" + rand.Text(), tools: make(map[int]*messagesTool), index: -1, done: make(chan struct{}),
		firstFrameGrace: anthropicFirstFrameGrace, idlePing: anthropicIdlePing}
}

// UpstreamStarted 由 handler 在上游请求真正发出时调用（前置拒绝如 429/503 不会走到这里，
// 因此那些路径仍保留真实状态码）。此后若长时间没有首帧，就先把流开起来并周期性 ping。
func (m *messagesWriter) UpstreamStarted() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.stream || m.started || m.ended || m.keepAlive {
		return
	}
	m.keepAlive = true
	go m.keepAliveLoop()
}

func (m *messagesWriter) keepAliveLoop() {
	timer := time.NewTimer(m.firstFrameGrace)
	defer timer.Stop()
	for {
		select {
		case <-m.done:
			return
		case <-timer.C:
		}
		m.mu.Lock()
		if m.ended || m.err != nil {
			m.mu.Unlock()
			return
		}
		if !m.started {
			m.beginLocked()
		}
		if time.Since(m.lastEvent) >= m.idlePing {
			m.eventLocked("ping", map[string]any{})
		}
		m.mu.Unlock()
		timer.Reset(m.idlePing)
	}
}

// DeliveredContent 报告是否已把客户端可见的内容推下去。
// 只发过 ping（保活）不算：那种情况下换号重试不会造成重复输出。
// message_start 也不算——它只是流已开启的声明，正文/思考/工具都还没到。
func (m *messagesWriter) DeliveredContent() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.delivered
}

func (m *messagesWriter) Header() http.Header  { return m.hdr }
func (m *messagesWriter) WriteHeader(code int) { m.status = code }
func (m *messagesWriter) Flush()               { _ = m.FlushError() }
func (m *messagesWriter) FlushError() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.flushLocked()
}

// flushLocked 是加锁后的刷新实现：调用方须持 m.mu（eventLocked 等内部路径使用，
// 避免非重入锁自锁）。
func (m *messagesWriter) flushLocked() error {
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

// Write 全程持锁：上游已发出、首帧迟到时 keepAliveLoop 会与读上游的 goroutine
// 并发跑，两者碰的是同一批字段（m.err / m.ended / m.usage / m.lastEvent）。
// 不加锁是真实数据竞争——m.err 是接口值，撕裂读可能拿到损坏的类型指针；
// m.usage 的并发 map 读写会让 Go 运行时直接以 fatal error 终止进程
// （2026-09-30 深度体检发现，-race 可复现）。因此本函数只调用 *Locked 变体，
// 不再经过会自行加锁的公开包装，避免非重入锁自锁。
func (m *messagesWriter) Write(data []byte) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.writeLocked(data)
}

func (m *messagesWriter) writeLocked(data []byte) (int, error) {
	if m.err != nil {
		return 0, m.err
	}
	if m.ended {
		return len(data), nil
	}
	m.buffer = append(m.buffer, data...)
	if len(m.buffer) > messagesBufferLimit {
		m.failureLocked(502, "upstream_response_too_large", "upstream response exceeds the adapter limit")
		return 0, m.err
	}
	if !strings.HasPrefix(m.hdr.Get("Content-Type"), "text/event-stream") {
		var object map[string]any
		if !json.Valid(m.buffer) {
			return len(data), nil
		}
		if err := jsonutil.Decode(m.buffer, &object); err != nil {
			m.failureLocked(502, "upstream_parse", "invalid upstream response")
			return 0, m.err
		}
		m.buffer = nil
		if problem, ok := object["error"].(map[string]any); ok {
			m.upstreamFailureLocked(m.status, problem)
			return len(data), nil
		}
		converted, err := m.convert(object)
		if err != nil {
			m.failureLocked(502, "response_contract_violation", err.Error())
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
				m.beginLocked()
				m.eventLocked("ping", map[string]any{})
			}
			continue
		}
		payload := strings.Join(lines, "\n")
		if payload == "[DONE]" {
			m.completeLocked()
			break
		}
		var object map[string]any
		if err := jsonutil.Decode([]byte(payload), &object); err != nil {
			m.failureLocked(502, "upstream_parse", "invalid stream event")
			break
		}
		if usage, ok := object["usage"].(map[string]any); ok {
			m.usage = upstream.MergeUsage(m.usage, usage)
		}
		if problem, ok := object["error"].(map[string]any); ok {
			m.upstreamFailureLocked(502, problem)
			break
		}
		choices := responseArray(object["choices"])
		if len(choices) > 1 {
			m.failureLocked(502, "response_contract_violation", "messages expects exactly one output choice")
			break
		}
		m.beginLocked()
		for _, raw := range choices {
			choice, _ := raw.(map[string]any)
			if value := choice["index"]; value != nil {
				index, ok := upstream.UsageCount(value)
				if !ok || index != 0 {
					m.failureLocked(502, "response_contract_violation", "messages expects output choice index zero")
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
				m.textLocked(kind, text)
			}
			for _, raw := range responseArray(delta["tool_calls"]) {
				call, _ := raw.(map[string]any)
				if err := m.toolDeltaLocked(call); err != nil {
					m.failureLocked(502, "response_contract_violation", err.Error())
					break
				}
			}
			if delta["function_call"] != nil {
				m.failureLocked(502, "response_contract_violation", "messages requires tool_calls; legacy function_call cannot be replayed with a tool id")
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

func (m *messagesWriter) toolDeltaLocked(call map[string]any) error {
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
		m.eventLocked("ping", map[string]any{})
	}
	return m.err
}

func stringField(object map[string]any, key string) string {
	value, _ := object[key].(string)
	return value
}

func (m *messagesWriter) copyHeaders() {
	for _, key := range []string{"Retry-After", "X-Request-ID", "X-RateLimit-Limit", "X-RateLimit-Remaining", "X-Concurrency-Limit"} {
		if value := m.hdr.Get(key); value != "" {
			m.inner.Header().Set(key, value)
		}
	}
	if id := m.inner.Header().Get("X-Request-ID"); id != "" {
		m.inner.Header().Set("Request-ID", id)
	}
}

// eventLocked 是加锁后的发帧实现：调用方须持 m.mu。
// 原先另有一个自行加锁的 event() 包装，Write 改为全程持锁后它没有调用者
// （所有路径都已在锁内），已移除。
func (m *messagesWriter) eventLocked(kind string, payload map[string]any) {
	if m.err != nil {
		return
	}
	payload["type"] = kind
	// ping 只是保活、message_start 只是开流声明，都不算客户端可见内容。
	if kind != "ping" && kind != "message_start" {
		m.delivered = true
	}
	data, err := json.Marshal(payload)
	if err == nil {
		_, err = fmt.Fprintf(m.inner, "event: %s\ndata: %s\n\n", kind, data)
		if err == nil {
			m.lastEvent = time.Now()
		}
	}
	m.err = err
	if flushErr := m.flushLocked(); flushErr != nil {
		m.err = flushErr
	}
}

func (m *messagesWriter) begin() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.beginLocked()
}

func (m *messagesWriter) beginLocked() {
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
	//
	// 2026-10-08：报 0 会让**首帧完全没有输入量**，客户端只能等到 message_delta
	// 才知道这一轮读了多少上下文——Claude Code 的 workflow 每个 agent 的 tok 计数
	// 正是读这里，于是整轮显示 0 tok（2026-09-29 记录过这个现象，当时判为不可修）。
	// 现在填入出站请求体的估算值：它是**临时值**，message_delta 一到就被上游实报
	// 覆盖，既不伪造缓存拆分，也不再让计数停在 0。
	if _, known := upstream.CachedInputTokens(m.usage); !known {
		usage["input_tokens"] = m.promptEstimate
	}
	// Initial counters are provisional. Official SDKs update standard counters
	// from message_delta but do not merge extension fields, so an early true
	// flag would incorrectly survive even after complete usage was reported.
	delete(usage, "gateway_usage_incomplete")
	m.eventLocked("message_start", map[string]any{"message": map[string]any{"id": m.id, "type": "message", "role": "assistant", "model": m.model, "content": []any{}, "stop_reason": nil, "stop_sequence": nil, "usage": usage}})
}

// StreamFailure 在流已经开始时把失败交付在流内（error 事件），返回 true 表示已交付。
func (m *messagesWriter) StreamFailure(code, message string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.started || m.ended {
		return false
	}
	m.failureLocked(http.StatusBadRequest, code, message)
	return true
}

// HoldProgress 由 handler 在循环保护压制期调用：先发 message_start，已开流则 ping。
func (m *messagesWriter) HoldProgress() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.ended || m.err != nil || !m.stream {
		return
	}
	if !m.started {
		m.beginLocked()
		return
	}
	if time.Since(m.lastEvent) >= m.idlePing {
		m.eventLocked("ping", map[string]any{})
	}
}

func (m *messagesWriter) closeBlockLocked() {
	if m.block != "" {
		m.eventLocked("content_block_stop", map[string]any{"index": m.index})
		m.block = ""
	}
}

func (m *messagesWriter) textLocked(kind, text string) {
	if m.block != kind {
		m.closeBlockLocked()
		m.index++
		m.block = kind
		block := map[string]any{"type": kind, kind: ""}
		if kind == "thinking" {
			block["signature"] = ""
		}
		m.eventLocked("content_block_start", map[string]any{"index": m.index, "content_block": block})
	}
	m.eventLocked("content_block_delta", map[string]any{"index": m.index, "delta": map[string]any{"type": kind + "_delta", kind: text}})
}

func (m *messagesWriter) completeLocked() {
	if m.ended || m.err != nil {
		return
	}
	if m.stop == "" {
		m.failureLocked(502, "upstream_incomplete", "upstream did not provide a finish reason")
		return
	}
	stop := anthropicStop(m.stop, len(m.tools) > 0)
	if stop == "" || (stop == "tool_use" && len(m.tools) == 0) {
		m.failureLocked(502, "response_contract_violation", "upstream finish reason does not describe a valid message")
		return
	}
	indices := make([]int, 0, len(m.tools))
	for index := range m.tools {
		indices = append(indices, index)
	}
	sort.Ints(indices)
	for _, index := range indices {
		if _, err := m.tools[index].input(); err != nil {
			m.failureLocked(502, "invalid_tool_arguments", err.Error())
			return
		}
	}
	m.beginLocked()
	m.closeBlockLocked()
	for _, index := range indices {
		tool := m.tools[index]
		m.index++
		m.eventLocked("content_block_start", map[string]any{"index": m.index, "content_block": map[string]any{"type": "tool_use", "id": tool.id, "name": tool.name, "input": map[string]any{}}})
		m.eventLocked("content_block_delta", map[string]any{"index": m.index, "delta": map[string]any{"type": "input_json_delta", "partial_json": tool.arguments.String()}})
		m.eventLocked("content_block_stop", map[string]any{"index": m.index})
	}
	m.eventLocked("message_delta", map[string]any{"delta": map[string]any{"stop_reason": stop, "stop_sequence": nil}, "usage": anthropicUsage(m.usage)})
	m.eventLocked("message_stop", map[string]any{})
	m.ended = true
}

func (t *messagesTool) input() (map[string]any, error) {
	var input map[string]any
	// 无参数工具的空串是上游层认可的合法形状（见 chat_output_contract.go 交付时归一为
	// {}、responses.go 的 validateResponseToolCall）。jsonutil.Decode("") 返回 EOF，
	// 直接判死会让「模型调用了一个无参数工具」整轮失败——Chat 与 Responses 早已接受，
	// Anthropic 漏了（2026-09-30 深度体检发现）。空串按空对象处理，语义等价。
	arguments := t.arguments.String()
	if strings.TrimSpace(arguments) == "" {
		arguments = "{}"
	}
	if strings.TrimSpace(t.id) == "" || strings.TrimSpace(t.name) == "" || jsonutil.Decode([]byte(arguments), &input) != nil || input == nil {
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

func (m *messagesWriter) upstreamFailureLocked(status int, problem map[string]any) {
	if detail, ok := upstream.ContextTooLongErrorDetail(problem); ok {
		m.failureLocked(http.StatusBadRequest, "context_length_exceeded", detail)
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
	m.failureLocked(status, code, message)
}

func (m *messagesWriter) failure(status int, code, message string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.failureLocked(status, code, message)
}

func (m *messagesWriter) failureLocked(status int, code, message string) {
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
		m.eventLocked("error", payload)
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
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.done != nil && m.keepAlive {
		close(m.done)
		m.keepAlive = false
	}
	if m.ended {
		return
	}
	if m.err != nil {
		m.ended = true
		return
	}
	if m.stream && m.started {
		m.failureLocked(502, "upstream_incomplete", "upstream stream ended without a complete response")
		return
	}
	m.failureLocked(502, "upstream_parse", "upstream returned an incomplete response")
}
