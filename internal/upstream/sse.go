// sse.go 处理上游 SSE 流：聚合成单个 OpenAI 响应，或透传给客户端。
// ═══ 更新日志 ═══
// 2026-09-26：空工具参数归一为 {}；choice 级 logprobs 透传（缺失补 null）。
// 2026-09-25：逐段读取并限制 SSE 单行/单事件为 64MiB，防止异常上游持续分配内存；超限返回明确失败。
// 2026-09-25：厂商错误事件包装成标准 error 时保留同帧顶层 usage，避免下游漏掉真实用量更新。
// 2026-09-25：移除裁剪记录对用量的覆盖，流式和聚合仅合并上游实际报告的累计值。
// 2026-09-19：工具参数允许在早到的结束标记后补齐；传输收尾统一校验，成功终态延迟到校验通过后发送。
// 2026-09-19：可选重复推理保护共享流/聚合入口，保留已观察帧并以明确错误终止，真实正文/工具进展重置窗口。
// 2026-09-16：统一 SSE 事件解析与结束校验，保留上游错误并防止断流和残缺工具参数伪装成功。
// 2026-09-16：将完整消息快照转成缺失增量并核对已有输出，区分工具参数暂缺、显式空串和类型错误。
// 2026-09-17：合并 fork 的错误信封透传，保留完整诊断字段与数字字面量，同时维持 typed 失败终态。
// 2026-09-18：拒绝错误形状的工具列表，并在流结束时校验工具终态确有调用，避免预告正文静默收尾。
// 2026-09-18：逐 choice 隔离工具名称与聚合输出，拒绝被静默忽略的非法正文、delta 和 choices 形状。
// 2026-09-19：累计用量按已出现字段合并，末尾 credit-only 或详细字段不再抹掉前帧 token。
package upstream

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"
)

// StreamError 表示上游数据或传输失败；与客户端写出失败区分，供 handler 记录真实结果。
// Upstream 保留上游 error 对象（包括厂商 code/details）；Cause 可由 errors.Is/As 检查。
// Stream 已经发出 ErrorObject 及 [DONE] 时仍返回此错误，调用方不得再合成成功终态。
type StreamError struct {
	Code     string
	Message  string
	Upstream map[string]any
	Cause    error
	// 合法 error 对象的原始 JSON 信封，仅 Stream 使用；保留信封外的 requestId 等诊断字段。
	rawFrame json.RawMessage
	// Retryable 表示这次失败发生时客户端**还没有收到任何字节**（仅 Stream 设置）。
	// 调用方据此可以在同一账号上整段重发，而不会让用户看到半截输出或重复内容。
	Retryable bool
}

func (e *StreamError) Error() string { return e.Message }
func (e *StreamError) Unwrap() error { return e.Cause }

// ErrorObject 返回 OpenAI SSE error 字段的值；上游对象不做字段删改。
func (e *StreamError) ErrorObject() map[string]any {
	if e.Upstream != nil {
		return e.Upstream
	}
	return map[string]any{"code": e.Code, "message": e.Message, "type": "upstream_error"}
}

type sseEvent struct {
	name string
	data string
}

// MaxSSEEventBytes is the shared upper bound for a single SSE line/event in
// protocol parsing and the raw usage observer that runs before it.
const MaxSSEEventBytes = 64 << 20

// readSSE 按空行分隔事件，data: 可无空格、多行 data 按 SSE 规范用换行连接。
// 保留最后一个没有空行但数据完整的 EOF 事件，兼容只发 finish_reason 的上游。
// 非 EOF 读错误始终保留；done 由 consume 显式返回，之后不再消费任何数据。
func readSSE(r io.Reader, consume func(sseEvent) (bool, error), comment func(string) error) error {
	return readSSEWithLimit(r, consume, comment, MaxSSEEventBytes)
}

// readSSEWithLimit shares the production parser with small-limit regression
// tests. The limit bounds an individual line and accumulated event, not the
// stream duration or total amount of generated output.
func readSSEWithLimit(r io.Reader, consume func(sseEvent) (bool, error), comment func(string) error, limit int) error {
	if limit <= 0 {
		limit = MaxSSEEventBytes
	}
	br := bufio.NewReaderSize(r, min(64*1024, limit))
	var data strings.Builder
	hasData := false
	eventName := ""
	dispatch := func() (bool, error) {
		if !hasData {
			eventName = ""
			return false, nil
		}
		ev := sseEvent{name: eventName, data: data.String()}
		data.Reset()
		hasData = false
		eventName = ""
		return consume(ev)
	}
	for {
		line, err := ReadSSELine(br, limit)
		var parseErr *StreamError
		if errors.As(err, &parseErr) {
			return err
		}
		line = strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r")
		if line == "" && err == nil {
			if stop, consumeErr := dispatch(); consumeErr != nil || stop {
				return consumeErr
			}
		} else if strings.HasPrefix(line, ":") {
			if comment != nil {
				if writeErr := comment(line); writeErr != nil {
					return writeErr
				}
			}
		} else if line != "" {
			field, value, found := strings.Cut(line, ":")
			if found {
				value = strings.TrimPrefix(value, " ")
			}
			switch field {
			case "data":
				extra := len(value)
				if hasData {
					extra++ // The SSE newline between data fields also occupies memory.
				}
				if extra > limit-data.Len()-len(eventName) {
					return oversizedSSEEvent()
				}
				if hasData {
					data.WriteByte('\n')
				}
				data.WriteString(value)
				hasData = true
			case "event":
				if len(value) > limit-data.Len() {
					return oversizedSSEEvent()
				}
				eventName = value
			}
		}
		if err == io.EOF {
			_, consumeErr := dispatch()
			return consumeErr
		}
		if err != nil {
			return &StreamError{Code: "upstream_read_error", Message: "upstream stream read failed", Cause: err}
		}
	}
}

func oversizedSSEEvent() *StreamError {
	return &StreamError{Code: "upstream_event_too_large", Message: "upstream stream line or event exceeded the size limit"}
}

// ReadSSELine bounds each allocation with ReadSlice before assembling a line.
// A non-positive limit selects MaxSSEEventBytes. Checking the
// length only after ReadString returns would still allow unbounded allocation
// when an upstream never sends a newline.
func ReadSSELine(br *bufio.Reader, limit int) (string, error) {
	if limit <= 0 {
		limit = MaxSSEEventBytes
	}
	var line strings.Builder
	for {
		part, err := br.ReadSlice('\n')
		if len(part) > limit-line.Len() {
			return "", oversizedSSEEvent()
		}
		line.Write(part)
		if err != bufio.ErrBufferFull {
			return line.String(), err
		}
	}
}

func upstreamEventError(value any) *StreamError {
	obj, ok := value.(map[string]any)
	if !ok {
		message, _ := value.(string)
		if message == "" {
			message = "upstream returned an error"
		}
		obj = map[string]any{"message": message, "type": "upstream_error"}
	}
	message, _ := obj["message"].(string)
	if message == "" {
		message = "upstream returned an error"
	}
	return &StreamError{Code: "upstream_error", Message: message, Upstream: obj}
}

func decodeSSEEvent(ev sseEvent) (obj map[string]any, done bool, err error) {
	if ev.name != "error" && strings.TrimSpace(ev.data) == "[DONE]" {
		return nil, true, nil
	}
	decodeErr := json.Unmarshal([]byte(ev.data), &obj)
	if ev.name == "error" {
		if decodeErr == nil && obj != nil {
			if value, ok := obj["error"]; ok && value != nil {
				failure := upstreamEventError(value)
				if _, ok := value.(map[string]any); ok {
					failure.rawFrame = json.RawMessage(ev.data)
				}
				return nil, false, failure
			}
			failure := upstreamEventError(obj)
			if _, ok := obj["usage"].(map[string]any); ok {
				// The vendor event has no error envelope. Keep the whole error
				// object and expose its observed usage beside it, as in ordinary
				// Chat frames. Raw JSON preserves the original numeric values.
				var fields map[string]json.RawMessage
				if json.Unmarshal([]byte(ev.data), &fields) == nil {
					frame, err := json.Marshal(map[string]json.RawMessage{
						"error": json.RawMessage(ev.data), "usage": fields["usage"],
					})
					if err == nil {
						failure.rawFrame = frame
					}
				}
			}
			return nil, false, failure
		}
		return nil, false, upstreamEventError(ev.data)
	}
	if decodeErr != nil || obj == nil {
		return nil, false, &StreamError{Code: "upstream_parse", Message: "upstream stream contained invalid JSON data", Cause: decodeErr}
	}
	if value, ok := obj["error"]; ok && value != nil {
		failure := upstreamEventError(value)
		if _, ok := value.(map[string]any); ok {
			failure.rawFrame = json.RawMessage(ev.data)
		}
		return nil, false, failure
	}
	return obj, false, nil
}

type streamText struct {
	value strings.Builder
	seen  bool
}

// append 返回尚未发送的内容；完整快照不能覆盖已经发送过的不同前缀。
func (s *streamText) append(value string, snapshot bool) (string, error) {
	addition := value
	if snapshot {
		previous := s.value.String()
		if !strings.HasPrefix(value, previous) {
			return "", &StreamError{Code: "upstream_parse", Message: "upstream message snapshot conflicts with streamed data"}
		}
		addition = value[len(previous):]
	}
	s.value.WriteString(addition)
	s.seen = true
	return addition, nil
}

type streamFunction struct {
	name      string
	arguments streamText
}

func (f *streamFunction) observe(raw map[string]any, snapshot bool) (map[string]any, error) {
	out := map[string]any{}
	if name, ok := raw["name"].(string); ok && name != "" {
		if f.name != "" && name != f.name {
			return nil, &StreamError{Code: "upstream_parse", Message: "upstream changed a streamed function name"}
		}
		if !snapshot || f.name == "" {
			out["name"] = name
		}
		f.name = name
	}
	if value, exists := raw["arguments"]; exists {
		args, ok := value.(string)
		if !ok {
			return nil, &StreamError{Code: "invalid_tool_arguments", Message: "upstream tool arguments must be a JSON string"}
		}
		seen := f.arguments.seen
		addition, err := f.arguments.append(args, snapshot)
		if err != nil {
			return nil, err
		}
		if !snapshot || addition != "" || !seen {
			out["arguments"] = addition
		}
	}
	return out, nil
}

type streamToolCall struct {
	id       string
	typ      string
	function streamFunction
}

type streamChoice struct {
	output       bool
	finishReason string
	role         string
	text         map[string]*streamText
	tools        map[int]*streamToolCall
	function     *streamFunction
	progress     uint64
	// nonContentProgress 只统计 refusal 与工具侧进展（新工具 ID/名称、非空参数增量）。
	// 正文保护需要把「正文」与「其它可见进展」分开：正文本身是被监控的载荷，不能自我
	// 重置窗口，而 refusal 或一次工具调用说明这轮输出换了阶段，应当重置正文窗口。
	nonContentProgress     uint64
	reasoningGuardProgress uint64
	outputGuardProgress    uint64
	reasoningGuard         *reasoningLoopGuard
	outputGuard            *reasoningLoopGuard
}

type streamState struct {
	choices          map[int]*streamChoice
	done             bool
	loopGuardEnabled bool
	// loopRetryAvailable 由 StreamOptions 传入：调用方还会重发时才压制错误帧。
	loopRetryAvailable bool
}

func validFinishReason(reason string) bool {
	switch reason {
	case "stop", "length", "tool_calls", "content_filter", "function_call":
		return true
	}
	return false
}

// hasVisibleProgress reports whether any choice has produced content, refusal or
// tool activity. Reasoning-only output deliberately does not count: the loop
// guard watches reasoning, so a run that never produces content must stay
// eligible for detection.
func (s *streamState) hasVisibleProgress() bool {
	for _, choice := range s.choices {
		if choice.progress > 0 || choice.finishReason != "" {
			return true
		}
	}
	return false
}

// hasNonContentProgress reports whether any choice has produced refusal text or
// tool activity. 这类进展不是被监控的载荷，一出现就必须放行：不能因为另一个
// choice 的正文还在压制区里，就把用户可见的 refusal 或工具调用也一起压住。
func (s *streamState) hasNonContentProgress() bool {
	for _, choice := range s.choices {
		if choice.nonContentProgress > 0 {
			return true
		}
	}
	return false
}

func (c *streamChoice) observeOutput(output map[string]any, wholeMessage bool) (map[string]any, error) {
	normalized := map[string]any{}
	progress := false
	nonContent := false
	if role, _ := output["role"].(string); role != "" {
		if !wholeMessage || c.role == "" {
			normalized["role"] = role
		}
		c.role = role
	}
	for _, key := range []string{"content", "reasoning_content", "refusal"} {
		if value := output[key]; value != nil {
			if _, ok := value.(string); !ok {
				return nil, &StreamError{Code: "upstream_parse", Message: "upstream " + key + " must be a string or null"}
			}
		}
		if value, ok := output[key].(string); ok {
			if c.text == nil {
				c.text = map[string]*streamText{}
			}
			if c.text[key] == nil {
				c.text[key] = &streamText{}
			}
			addition, err := c.text[key].append(value, wholeMessage)
			if err != nil {
				return nil, err
			}
			normalized[key] = addition
			if key != "reasoning_content" && strings.TrimSpace(addition) != "" {
				progress = true
				if key != "content" {
					nonContent = true
				}
			}
			if value != "" {
				c.output = true
			}
		}
	}
	if calls, ok := output["tool_calls"].([]any); ok {
		var normalizedCalls []any
		for i, item := range calls {
			call, ok := item.(map[string]any)
			if !ok {
				return nil, &StreamError{Code: "upstream_parse", Message: "upstream stream contained an invalid tool call"}
			}
			c.output = true
			idx := 0
			if wholeMessage {
				idx = i
			}
			if value, ok := call["index"].(float64); ok {
				idx = int(value)
			}
			if c.tools == nil {
				c.tools = map[int]*streamToolCall{}
			}
			if wholeMessage {
				if id, _ := call["id"].(string); id != "" {
					for candidate, previous := range c.tools {
						if previous.id == id {
							idx = candidate
							break
						}
					}
				}
			}
			state := c.tools[idx]
			if state == nil {
				state = &streamToolCall{}
				c.tools[idx] = state
			}
			next := map[string]any{"index": float64(idx)}
			for _, field := range []struct {
				key      string
				previous *string
			}{{"id", &state.id}, {"type", &state.typ}} {
				if value, _ := call[field.key].(string); value != "" {
					if *field.previous != "" && value != *field.previous {
						return nil, &StreamError{Code: "upstream_parse", Message: "upstream changed a streamed tool call identity"}
					}
					if !wholeMessage || *field.previous == "" {
						next[field.key] = value
					}
					if field.key == "id" && value != *field.previous && strings.TrimSpace(value) != "" {
						progress = true
						nonContent = true
					}
					*field.previous = value
				}
			}
			if fn, ok := call["function"].(map[string]any); ok {
				previousName := state.function.name
				nextFn, err := state.function.observe(fn, wholeMessage)
				if err != nil {
					return nil, err
				}
				if len(nextFn) > 0 {
					next["function"] = nextFn
				}
				arguments, _ := nextFn["arguments"].(string)
				if (state.function.name != previousName && strings.TrimSpace(state.function.name) != "") || arguments != "" {
					progress = true
					nonContent = true
				}
			} else if value, exists := call["function"]; exists && value != nil {
				return nil, &StreamError{Code: "upstream_parse", Message: "upstream stream contained an invalid tool function"}
			}
			if len(next) > 1 {
				normalizedCalls = append(normalizedCalls, next)
			}
		}
		if len(normalizedCalls) > 0 {
			normalized["tool_calls"] = normalizedCalls
		}
	} else if value, exists := output["tool_calls"]; exists && value != nil {
		return nil, &StreamError{Code: "upstream_parse", Message: "upstream tool_calls must be an array"}
	}
	if fn, ok := output["function_call"].(map[string]any); ok {
		name, _ := fn["name"].(string)
		args, _ := fn["arguments"].(string)
		if value, exists := fn["arguments"]; exists {
			if _, ok := value.(string); !ok {
				return nil, &StreamError{Code: "invalid_tool_arguments", Message: "upstream function arguments must be a JSON string"}
			}
		}
		// WorkBuddy 的空占位 function_call 不是一次工具调用，沿用原有剥除语义。
		if c.function != nil || name != "" || args != "" {
			if c.function == nil {
				c.function = &streamFunction{}
			}
			c.output = true
			previousName := c.function.name
			next, err := c.function.observe(fn, wholeMessage)
			if err != nil {
				return nil, err
			}
			if len(next) > 0 {
				normalized["function_call"] = next
			}
			arguments, _ := next["arguments"].(string)
			if (c.function.name != previousName && strings.TrimSpace(c.function.name) != "") || arguments != "" {
				progress = true
				nonContent = true
			}
		}
	} else if value, exists := output["function_call"]; exists && value != nil {
		return nil, &StreamError{Code: "upstream_parse", Message: "upstream stream contained an invalid function call"}
	}
	if progress {
		c.progress++
	}
	if nonContent {
		c.nonContentProgress++
	}
	return normalized, nil
}

func (c *streamChoice) validateArguments() error {
	// length/content_filter 是明确的不完整响应，由协议转换层保留 incomplete 语义。
	if c.finishReason == "length" || c.finishReason == "content_filter" {
		return nil
	}
	for _, call := range c.tools {
		if !call.function.arguments.seen || isTruncatedArguments(call.function.arguments.value.String()) {
			return &StreamError{Code: "invalid_tool_arguments", Message: "upstream ended with incomplete or invalid tool arguments"}
		}
	}
	if c.function != nil && (!c.function.arguments.seen || isTruncatedArguments(c.function.arguments.value.String())) {
		return &StreamError{Code: "invalid_tool_arguments", Message: "upstream ended with incomplete or invalid function arguments"}
	}
	return nil
}

func (s *streamState) observe(obj map[string]any) error {
	if value := obj["choices"]; value != nil {
		if _, ok := value.([]any); !ok {
			return &StreamError{Code: "upstream_parse", Message: "upstream choices must be an array"}
		}
	}
	choices, _ := obj["choices"].([]any)
	for _, item := range choices {
		choice, ok := item.(map[string]any)
		if !ok {
			return &StreamError{Code: "upstream_parse", Message: "upstream stream contained an invalid choice"}
		}
		idx := 0
		if value, ok := choice["index"].(float64); ok {
			idx = int(value)
		}
		if s.choices == nil {
			s.choices = map[int]*streamChoice{}
		}
		state := s.choices[idx]
		if state == nil {
			state = &streamChoice{}
			s.choices[idx] = state
		}
		normalized := map[string]any{}
		for _, key := range []string{"delta", "message"} {
			if value := choice[key]; value != nil {
				if _, ok := value.(map[string]any); !ok {
					return &StreamError{Code: "upstream_parse", Message: "upstream " + key + " must be an object or null"}
				}
			}
			if output, ok := choice[key].(map[string]any); ok {
				delta, err := state.observeOutput(output, key == "message")
				if err != nil {
					return err
				}
				mergeOutputDelta(normalized, delta)
			}
		}
		// 下游只处理一次统一增量，完整 message 不再走另一个丢字段/重复输出的旁路。
		choice["delta"] = normalized
		delete(choice, "message")
		if reason, ok := choice["finish_reason"].(string); ok && reason != "" {
			if !validFinishReason(reason) {
				return &StreamError{Code: "upstream_parse", Message: "upstream stream contained an unknown finish_reason"}
			}
			state.finishReason = reason
			// WorkBuddy may send a finish marker before the final argument delta or
			// message snapshot. Validate once all frames through DONE/EOF arrive.
			// Stream withholds the finish marker until that validation succeeds.
		}
	}
	return nil
}

func (s *streamState) end() error {
	hasResponse := false
	for _, choice := range s.choices {
		if choice.output || choice.finishReason != "" {
			hasResponse = true
		}
		if choice.output && !s.done && choice.finishReason == "" {
			return &StreamError{Code: "upstream_truncated", Message: "upstream stream ended before a terminal marker"}
		}
		if err := choice.validateArguments(); err != nil {
			return err
		}
		// A finish marker can precede late metadata. Enforce presence only at the
		// response boundary, after all frames before DONE have been consumed.
		if (choice.finishReason == "tool_calls" || choice.finishReason == "function_call") && len(choice.tools) == 0 && choice.function == nil {
			return &StreamError{Code: "missing_tool_call", Message: "upstream ended with a tool finish reason but no tool call"}
		}
	}
	if !hasResponse {
		return &StreamError{Code: "empty_upstream", Message: "upstream stream contained no valid data events"}
	}
	return nil
}

func (c *streamChoice) aggregate(index int) map[string]any {
	role := c.role
	if role == "" {
		role = "assistant"
	}
	message := map[string]any{"role": role, "content": ""}
	for _, key := range []string{"content", "reasoning_content", "refusal"} {
		if text := c.text[key]; text != nil && text.value.Len() > 0 {
			message[key] = text.value.String()
		}
	}
	incomplete := c.finishReason == "length" || c.finishReason == "content_filter"
	function := func(value *streamFunction) map[string]any {
		if value == nil || (incomplete && (!value.arguments.seen || isTruncatedArguments(value.arguments.value.String()))) {
			return nil
		}
		out := map[string]any{}
		if value.name != "" {
			out["name"] = value.name
		}
		if value.arguments.seen {
			out["arguments"] = value.arguments.value.String()
		}
		return out
	}
	if fn := function(c.function); fn != nil {
		message["function_call"] = fn
	}
	indexes := make([]int, 0, len(c.tools))
	for index := range c.tools {
		indexes = append(indexes, index)
	}
	sort.Ints(indexes)
	calls := make([]map[string]any, 0, len(indexes))
	for _, index := range indexes {
		call := c.tools[index]
		fn := function(&call.function)
		if fn == nil {
			continue
		}
		out := map[string]any{"index": index, "function": fn}
		if call.id != "" {
			out["id"] = call.id
		}
		if call.typ != "" {
			out["type"] = call.typ
		}
		calls = append(calls, out)
	}
	if len(calls) > 0 {
		message["tool_calls"] = calls
	}
	finish := c.finishReason
	if finish == "" {
		finish = "stop"
	}
	return map[string]any{"index": index, "message": message, "finish_reason": finish}
}

// Aggregate 读取完整 SSE 流，保留每个 choice 的正文、工具与终态。
// 分片/多行 SSE 由 readSSE 处理；只有合法结束才返回成功，异常流返回 *StreamError。
// tool_calls 以流式 delta 到达（按 index 合并：首片带 id/type/name，后续只带 arguments 片段）。
func Aggregate(r io.Reader, options ...StreamOptions) (map[string]any, error) {
	var (
		id, model string
		created   float64
		usage     map[string]any
	)
	state := newStreamState(options)
	err := readSSE(r, func(ev sseEvent) (bool, error) {
		chunk, done, err := decodeSSEEvent(ev)
		if err != nil || done {
			state.done = done
			if done && err == nil {
				err = state.finishReasoningLoops()
			}
			return true, err
		}
		if err := state.observe(chunk); err != nil {
			return true, err
		}
		if value, ok := chunk["id"].(string); ok && id == "" {
			id = value
		}
		if value, ok := chunk["model"].(string); ok && model == "" {
			model = value
		}
		if value, ok := chunk["created"].(float64); ok && created == 0 {
			created = value
		}
		if value, ok := chunk["usage"].(map[string]any); ok {
			usage = MergeUsage(usage, value)
		}
		if err := state.observeReasoningLoops(chunk); err != nil {
			return true, err
		}
		return false, nil
	}, nil)
	if err == nil {
		err = state.finishReasoningLoops()
	}
	if err != nil {
		return nil, err
	}
	if err := state.end(); err != nil {
		return nil, err
	}
	if id == "" {
		id = fmt.Sprintf("chatcmpl-%d", time.Now().UnixNano())
	}
	if created == 0 {
		created = float64(time.Now().Unix())
	}
	indexes := make([]int, 0, len(state.choices))
	for index := range state.choices {
		indexes = append(indexes, index)
	}
	sort.Ints(indexes)
	choices := make([]any, 0, len(indexes))
	for _, index := range indexes {
		choices = append(choices, state.choices[index].aggregate(index))
	}
	resp := map[string]any{
		"id":      id,
		"object":  "chat.completion",
		"created": int64(created),
		"model":   model,
		"choices": choices,
	}
	if usage != nil {
		resp["usage"] = usage
	}
	return resp, nil
}

func mergeFunctionDelta(merged, delta map[string]any) {
	if value, ok := delta["name"].(string); ok && value != "" {
		merged["name"] = value
	}
	if value, ok := delta["arguments"].(string); ok {
		previous, _ := merged["arguments"].(string)
		merged["arguments"] = previous + value
	}
}

func mergeOutputDelta(merged, delta map[string]any) {
	for key, value := range delta {
		switch key {
		case "content", "reasoning_content", "refusal":
			previous, _ := merged[key].(string)
			addition, _ := value.(string)
			merged[key] = previous + addition
		case "tool_calls":
			previous, _ := merged[key].([]any)
			addition, _ := value.([]any)
			merged[key] = append(previous, addition...)
		case "function_call":
			fn, _ := merged[key].(map[string]any)
			if fn == nil {
				fn = map[string]any{}
				merged[key] = fn
			}
			addition, _ := value.(map[string]any)
			mergeFunctionDelta(fn, addition)
		default:
			merged[key] = value
		}
	}
}

// stripToolCallNames 收敛流式 tool_calls 的 name 语义为「每个 index 只出现一次」：
// 首片保留 function.name，同一 index 后续分片里的 name 键一律删除（无论上游是
// 空串还是重复非空串）。这是 OpenAI 官方流的真实形态——首帧带 name，后续帧只带
// arguments 片段、不再出现 name 键——因此是累加型与覆盖型客户端的共同祖先行为。
//
// 两类消费模型在该形态下同时正确：
//   - 累加型（官方 WorkBuddy/CodeBuddy `name += tc_function?.name || ""`）：
//     后续分片 name 键缺失 → 追加空串，累积 name 保持唯一，不再拼成 Bash×帧数（issue #82）。
//   - 覆盖型（hawklithm#2 / Grok Build `name ?? state.name` 或 `if (name) state.name = name`）：
//     后续分片 name 键缺失 → 保留已建好的首帧 name，不被空串意外清空。
//     键缺失是比空串更安全的形态：`??` 与 truthy 守卫对缺失键必然保留旧值，
//     而对空串，`??` 会误判为重设并清空工具名。
//
// seen 记录每个 index 是否已实际发过非空 name；仅收到 id/arguments 不能挡住后补的 name。
// 只动 function.name 键，id/type/arguments 原样透传。
func stripToolCallNames(obj map[string]any, seen map[[2]int]bool) {
	choices, _ := obj["choices"].([]any)
	for _, ci := range choices {
		c, _ := ci.(map[string]any)
		if c == nil {
			continue
		}
		choiceIndex := 0
		if value, ok := c["index"].(float64); ok {
			choiceIndex = int(value)
		}
		delta, _ := c["delta"].(map[string]any)
		if delta == nil {
			continue
		}
		tcs, _ := delta["tool_calls"].([]any)
		for _, tci := range tcs {
			tc, _ := tci.(map[string]any)
			if tc == nil {
				continue
			}
			idx := 0
			if v, ok := tc["index"].(float64); ok {
				idx = int(v)
			}
			key := [2]int{choiceIndex, idx}
			if seen[key] {
				// 已发过首片：删除本分片的 name 键（存在即删，幂等）。
				if fn, _ := tc["function"].(map[string]any); fn != nil {
					delete(fn, "name")
				}
				continue
			}
			if fn, _ := tc["function"].(map[string]any); fn != nil {
				if name, _ := fn["name"].(string); name != "" {
					seen[key] = true
				} else {
					delete(fn, "name")
				}
			}
		}
	}
}

// normalizeFrame 以 OpenAI 流式规范白名单重建帧：仅保留标准字段，
// 剔除上游噪声（finish_reason:"" → null、空 content/refusal、空 tool_calls 列表、
// 空占位 function_call、顶层未知字段），空 delta 键一律省略，
// usage 缺失 → null，保证任意标准客户端按规范解析。
func normalizeFrame(obj map[string]any) map[string]any {
	if value, exists := obj["error"]; exists && value != nil {
		return map[string]any{"error": value}
	}
	out := map[string]any{}
	for _, k := range []string{"id", "object", "created", "model", "system_fingerprint", "service_tier"} {
		if v, ok := obj[k]; ok && v != nil {
			out[k] = v
		}
	}
	if _, ok := out["object"]; !ok {
		out["object"] = "chat.completion.chunk"
	}
	if _, ok := out["id"]; !ok {
		out["id"] = "chatcmpl-wb2api"
	}
	if chs, ok := obj["choices"].([]any); ok {
		nchs := make([]any, 0, len(chs))
		for _, ci := range chs {
			c, ok := ci.(map[string]any)
			if !ok {
				continue
			}
			nc := map[string]any{}
			if idx, ok := c["index"]; ok {
				nc["index"] = idx
			}
			delta := map[string]any{}
			d, _ := c["delta"].(map[string]any)
			if d == nil {
				d, _ = c["message"].(map[string]any)
			}
			if d != nil {
				if v, ok := d["role"].(string); ok && v != "" {
					delta["role"] = v
				}
				if v, ok := d["content"].(string); ok && v != "" {
					delta["content"] = v
				}
				if v, ok := d["reasoning_content"].(string); ok && v != "" {
					delta["reasoning_content"] = v
				}
				if v, ok := d["refusal"].(string); ok && v != "" {
					delta["refusal"] = v
				}
				if tcs, ok := d["tool_calls"].([]any); ok && len(tcs) > 0 {
					delta["tool_calls"] = tcs
				}
				if fc, ok := d["function_call"]; ok && fc != nil {
					// 空占位 function_call（name/arguments 全空）视为噪声剔除
					keep := false
					if fcm, ok2 := fc.(map[string]any); ok2 {
						n, _ := fcm["name"].(string)
						a, _ := fcm["arguments"].(string)
						keep = n != "" || a != ""
					} else {
						keep = true
					}
					if keep {
						delta["function_call"] = fc
					}
				}
			}
			nc["delta"] = delta
			// logprobs 也是 choice 级字段：客户端请求后拿不到字段（而不是 null）
			// 时，SDK 读 chunk.choices[0].logprobs.content 会直接抛异常。
			if v, ok := c["logprobs"]; ok {
				nc["logprobs"] = v
			} else {
				nc["logprobs"] = nil
			}

			if fr, ok := c["finish_reason"].(string); ok && fr != "" {
				nc["finish_reason"] = fr
			} else {
				nc["finish_reason"] = nil
			}
			nchs = append(nchs, nc)
		}
		out["choices"] = nchs
	}
	if u, ok := obj["usage"]; ok {
		out["usage"] = u
	} else {
		out["usage"] = nil
	}
	return out
}

// Stream 逐事件规范化并 flush；合法终态写唯一 [DONE]。
// 上游错误/读错误/截断先发 error 再发 [DONE] 并返回 *StreamError。
// [DONE] 只关闭传输，不覆盖已有 error；客户端写失败直接返回原始错误。
//
// 重复推理保护命中时：若 options.LoopRetryAvailable 为 true，说明调用方还会在同一
// 账号上重发，本次不向客户端写任何字节（连 error 帧与 [DONE] 都不写），只把错误标成
// Retryable 返回；调用方重发失败而无法继续时，必须用 WriteStreamError 补上错误帧，
// 否则客户端会既收不到内容也看不到失败原因。
func Stream(w http.ResponseWriter, r io.Reader, options ...StreamOptions) error {
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
	fl, _ := w.(http.Flusher)

	// toolCallSeen 跨帧记录 delta.tool_calls 里已发过首片的 index，
	// 供逐 chunk 透传时收敛 name 为「每 index 一次」（对齐 OpenAI 官方流）。
	toolCallSeen := map[[2]int]bool{}

	// firstID 透传流的消息级 id 基准：缓存首个非空上游 id，后续帧缺失/空串时复用
	// （issue #35：同一条 SSE 消息所有帧共用一个真实 id，后台按 id 归并；此前中间帧
	// 一律补 chatcmpl-wb2api 哨兵，造成同流 id 分裂）。全流无真实 id → 才出现哨兵。
	firstID := ""
	var usage map[string]any
	terminalMeta := map[string]any{}

	state := newStreamState(options)

	// ── 写出闸门（仅在重复推理保护启用时生效）────────────────────────────
	//
	// 保护命中时我们想「整段重发」而让用户完全无感，但流式一旦写出去就收不回。
	// 因此先把帧压在内存里，直到能确认不是这条规则能抓的循环：
	//   - 纯推理阶段一直压住，直到推理字符上限、缓冲上限或时间上限（长推理不能无限等）；
	//   - 正文阶段压到出现长行、空行、第三种短行或正文上限为止（正常回答通常一行就放行）；
	//   - refusal 与工具进展立即放行，它们不是被监控的载荷；
	//   - 在压制期间命中循环 → 客户端零字节，错误标记 Retryable，由 handler 整段重发。
	// 保护未启用时不进这个分支，行为与引入前完全一致。
	var pending []byte
	released := !state.loopGuardEnabled
	holdBackDeadline := time.Now().Add(ReasoningLoopHoldBackTimeout)
	// 正文压制期的独立时限：从第一帧正文开始计时，比推理侧短得多，避免「没有换行的
	// 短回答」被闸门压到推理侧的 60 秒上限。holdingContent 记录当前是否正处于正文
	// 压制期：一次工具调用会把正文窗口清零，之后若又出现可疑正文，时限要重新起算，
	// 不能用上一段的旧期限把它立刻放行。
	var contentDeadline time.Time
	holdingContent := false
	releasePending := func() error {
		if released {
			return nil
		}
		released = true
		if len(pending) == 0 {
			return nil
		}
		if _, werr := w.Write(pending); werr != nil {
			return werr
		}
		pending = nil
		if fl != nil {
			fl.Flush()
		}
		return nil
	}
	// 正常帧和错误帧共享一个写出口，任何客户端断开均向上传递。
	writeRaw := func(payload string) error {
		frame := "data: " + payload + "\n\n"
		if !released {
			pending = append(pending, frame...)
			if len(pending) >= ReasoningLoopHoldBackBytes {
				return releasePending()
			}
			return nil
		}
		if _, werr := io.WriteString(w, frame); werr != nil {
			return werr
		}
		if fl != nil {
			fl.Flush()
		}
		return nil
	}

	err := readSSE(r, func(ev sseEvent) (bool, error) {
		obj, done, err := decodeSSEEvent(ev)
		if err != nil || done {
			state.done = done
			if done && err == nil {
				err = state.finishReasoningLoops()
			}
			return true, err
		}
		if err := state.observe(obj); err != nil {
			return true, err
		}
		// 先判定这一帧是否命中循环，再决定要不要放行：命中帧本身必须留在压制区里，
		// 否则客户端会先看到一段重复推理，再收到错误。
		guardErr := state.observeReasoningLoops(obj)
		// 命中帧本身必须留在压制区里：先放行再报错，客户端就会看到一段重复文本。
		// 未命中时按下面几条出口决定要不要继续压住：
		//   - refusal 与工具进展一出现就放行，它们不是被监控的载荷；
		//   - 纯推理阶段（还没有正文）继续压住，让推理循环有机会被整段丢弃后重发；
		//   - 正文阶段只要还不能排除短行循环就继续压住（holdOutput），一旦出现长行、
		//     短行种类变多或字符超限就放行；
		//   - 推理累计到压制字符上限、或压制超时 → 放行（长推理不能无限等）。
		if guardErr == nil {
			// 纯推理阶段（hasVisibleProgress 为假）继续压住；一旦出现正文或其它可见
			// 进展，就只按正文闸门与「非正文进展」决定。
			contentHeld := !state.hasNonContentProgress() && state.holdOutput()
			if contentHeld && !holdingContent {
				holdingContent = true
				contentDeadline = time.Now().Add(ContentLoopHoldBackTimeout)
			} else if !contentHeld {
				holdingContent = false
			}
			keepHolding := !state.hasVisibleProgress() || contentHeld
			if !keepHolding || state.reasoningCharacters() >= reasoningLoopHoldBackChars ||
				!time.Now().Before(holdBackDeadline) ||
				(contentHeld && !time.Now().Before(contentDeadline)) {
				if releaseErr := releasePending(); releaseErr != nil {
					return true, releaseErr
				}
			}
		}
		if value, ok := obj["usage"].(map[string]any); ok {
			usage = MergeUsage(usage, value)
			obj["usage"] = usage
		}
		// Text, argument deltas and observed usage continue streaming. A finish
		// is not safe to expose until late arguments and upstream errors have
		// been consumed; the authoritative reasons remain in streamState.
		choices, _ := obj["choices"].([]any)
		for _, raw := range choices {
			raw.(map[string]any)["finish_reason"] = nil
		}
		stripToolCallNames(obj, toolCallSeen)
		if firstID == "" {
			if value, ok := obj["id"].(string); ok && value != "" {
				firstID = value
			}
		} else if value, ok := obj["id"].(string); !ok || value == "" {
			obj["id"] = firstID
		}
		for _, key := range []string{"id", "model", "created", "system_fingerprint", "service_tier"} {
			if value, exists := obj[key]; exists && value != nil {
				terminalMeta[key] = value
			}
		}
		raw, err := json.Marshal(normalizeFrame(obj))
		if err != nil {
			return true, &StreamError{Code: "upstream_parse", Message: "upstream frame could not be encoded", Cause: err}
		}
		if writeErr := writeRaw(string(raw)); writeErr != nil {
			return true, errors.Join(guardErr, writeErr)
		}
		return guardErr != nil, guardErr
	}, func(line string) error {
		// SSE 注释行同样要过闸门：压制期间若直接写出，客户端就不是零字节了，
		// 命中循环时便无法整段重发。
		if !released {
			pending = append(pending, line+"\n\n"...)
			// 注释行也要受同样的约束：上游只发 keepalive 时既不能让缓冲无限增长，
			// 也不能让客户端一直看不到任何响应。
			if len(pending) >= ReasoningLoopHoldBackBytes || !time.Now().Before(holdBackDeadline) {
				return releasePending()
			}
			return nil
		}
		if _, err := io.WriteString(w, line+"\n\n"); err != nil {
			return err
		}
		if fl != nil {
			fl.Flush()
		}
		return nil
	})
	if err == nil {
		err = state.finishReasoningLoops()
	}
	if err == nil {
		err = state.end()
	}
	// 命中循环且客户端零字节时，整段压住的帧直接丢弃：调用方会在同一账号上重发，
	// 客户端不该先看到半截推理再收到错误。
	var heldErr *StreamError
	heldLoop := err != nil && IsLoopGuardError(err) && !released && errors.As(err, &heldErr)
	// 只有在调用方明确还会重发时（LoopRetryAvailable）才压制错误帧；否则按原有方式
	// 把错误如实写给客户端，避免"既不重发也不报错"的黑洞。
	retryPending := heldLoop && state.loopRetryAvailable
	if heldLoop {
		// 压住的这一段本身就是那串重复文本：重发时整段丢弃；重发用尽后同样丢弃，
		// 只把错误写给客户端。否则用户会先看到一屏重复输出、再看到失败，正是这项
		// 保护要消除的现象。直接清空缓冲并把闸门标记为已放行，让下面的错误帧照常写出。
		pending = nil
		released = true
	} else if releaseErr := releasePending(); releaseErr != nil {
		// 其余情况（正常收尾、上游截断等）把压住的帧原样放行，让客户端看到真实行为。
		return errors.Join(err, releaseErr)
	}
	if err == nil {
		indexes := make([]int, 0, len(state.choices))
		for index, choice := range state.choices {
			if choice.finishReason != "" {
				indexes = append(indexes, index)
			}
		}
		sort.Ints(indexes)
		if len(indexes) > 0 {
			choices := make([]any, 0, len(indexes))
			for _, index := range indexes {
				choices = append(choices, map[string]any{"index": index, "delta": map[string]any{}, "finish_reason": state.choices[index].finishReason})
			}
			terminalMeta["choices"] = choices
			// Usage was already streamed in its observed frames; do not count it
			// again when publishing this deferred finish-only event.
			raw, marshalErr := json.Marshal(normalizeFrame(terminalMeta))
			if marshalErr != nil {
				return marshalErr
			}
			if writeErr := writeRaw(string(raw)); writeErr != nil {
				return writeErr
			}
		}
	}
	if err != nil {
		var streamErr *StreamError
		if !errors.As(err, &streamErr) {
			return err
		}
		// 循环命中、尚未放行任何字节、且调用方还会重发 → 客户端什么都没看到，
		// 这里不写 error 帧与 [DONE]，只把错误标成 Retryable 返回。
		if retryPending {
			streamErr.Retryable = true
			return err
		}
		var raw []byte
		var marshalErr error
		if len(streamErr.rawFrame) > 0 {
			// 多行 SSE 的 JSON 收成一行，仅去格式空白，不重新解析数字或删诊断字段。
			var compact bytes.Buffer
			marshalErr = json.Compact(&compact, streamErr.rawFrame)
			raw = compact.Bytes()
		} else {
			raw, marshalErr = json.Marshal(map[string]any{"error": streamErr.ErrorObject()})
		}
		if marshalErr != nil {
			return errors.Join(err, marshalErr)
		}
		if writeErr := writeRaw(string(raw)); writeErr != nil {
			return errors.Join(err, writeErr)
		}
	}
	if writeErr := writeRaw("[DONE]"); writeErr != nil {
		return errors.Join(err, writeErr)
	}
	return err
}

// WriteStreamError 把一次未交付给客户端的流式失败补写成 SSE error 帧 + [DONE]。
//
// 用于 Stream 因「客户端零字节、调用方准备重发」而压制了错误帧、但重发最终没能建立
// 的场景：那时必须把失败如实交给客户端，不能静默结束。
// 返回是否真的写出了错误（未写出时调用方应改用普通错误响应）。
func WriteStreamError(w http.ResponseWriter, err error) bool {
	var streamErr *StreamError
	if !errors.As(err, &streamErr) {
		return false
	}
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Accel-Buffering", "no")
	var raw []byte
	var marshalErr error
	if len(streamErr.rawFrame) > 0 {
		var compact bytes.Buffer
		marshalErr = json.Compact(&compact, streamErr.rawFrame)
		raw = compact.Bytes()
	} else {
		raw, marshalErr = json.Marshal(map[string]any{"error": streamErr.ErrorObject()})
	}
	if marshalErr != nil {
		return false
	}
	fl, _ := w.(http.Flusher)
	if _, werr := io.WriteString(w, "data: "+string(raw)+"\n\n"); werr != nil {
		return false
	}
	if _, werr := io.WriteString(w, "data: [DONE]\n\n"); werr != nil {
		return false
	}
	if fl != nil {
		fl.Flush()
	}
	return true
}
