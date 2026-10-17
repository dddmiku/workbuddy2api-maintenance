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
	// holdSince 记录压制期首次回调的时刻，用于判断「零字节盲等」已持续多久。
	// 跨重发轮次累计、不在 ResetForRetry 里清零：盲等时长要按整个请求算，
	// 否则每轮重置会让总盲等时间无上限。
	holdSince time.Time
}

// DeliveredContent 报告是否已把客户端可见内容推下去（正文/思考/工具参数）。
// 只发过空帧或用量帧不算，那种情况换号重试不会重复输出。
func (w *chatContractWriter) DeliveredContent() bool { return len(w.marked) > 0 }

// ResetForRetry 在「上游掐流且客户端零内容」后换号重发前清空上一轮的缓冲与校验状态。
//
// 流已经开启（响应头已发，无法收回），要清的是**内容状态**：
//   - raw：上一轮的帧会被 CompletionError 聚合，混进重试的终态判定；
//   - checked：一次性标志，不重置的话重试后不再做契约校验；
//   - errorFrame：上一轮的错误帧会被写进重试的流里（客户端看到凭空的 error）；
//   - marked：影响 DeliveredContent，重试前本就该是空。
//
// 调用前提由 handler 保证：deliveredContent 为 false，客户端还没收到任何内容。
func (w *chatContractWriter) ResetForRetry() {
	w.raw.Reset()
	w.marked = nil
	w.buffer = nil
	w.errorFrame = nil
	w.checked = false
	w.err = nil
	w.writeErr = nil
	// usage 与 usageEnvelope 也必须清空：被丢弃的那一轮已经 absorb 过用量，
	// 不清的话重试成功后末帧会把上一轮的计数再报一次（客户端与账本都翻倍）。
	// upstream.MergeUsage(previous, nil) 返回 previous，所以只要不清就一定会留下。
	w.usage = nil
	w.usageEnvelope = nil
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
	// 内容帧写出后必须立即 flush。net/http 在响应体上有一层缓冲，只有收尾、
	// 心跳或缓冲填满才真正发到 socket：实测 71 token/16KB 的响应，网关内部
	// 首帧在 1448ms，客户端却到 1747ms 才收到字节；更慢的流要等到 5s 心跳。
	// 重试资格由 DeliveredContent()（正文/思考/拒答）判定，不取决于是否 flush，
	// 所以这里只在已经交付过内容时 flush——纯角色帧仍留在缓冲里，保住
	// 「零内容时可干净重试」的契约。
	if w.DeliveredContent() {
		w.Flush()
	}
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
	// 有 output 契约时保留正文：json_object / json_schema 要在交付前按 schema 校验，
	// 必须留全文。
	//
	// 已知代价（2026-09-30 审计提出，评估后保留）：response_format={"type":"text"} 虽然
	// 不需要校验，也会走这条分支把正文留在 16MB 缓冲里。改成按 chatFormat != nil 判定
	// 试过，但 w.raw 同时是 Aggregate 的输入与「是否有内容」的判据（见下方 informative），
	// 去掉正文会让聚合与判据同时失真，破坏既有行为。收益（仅 text 格式下省一次缓冲）
	// 不足以承担这个风险，故维持原样。
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
				// 拒答是**立即下发**的客户端可见内容（下面只删除 tool_calls/function_call，
				// 不删 refusal），不标记的话 DeliveredContent() 对拒答帧返回 false，
				// 掐流重试会在客户端已经读到拒答之后重发，用户看到两段互相矛盾的回答
				// （2026-09-30 深度体检发现）。
				//
				// tool_calls / function_call 刻意**不标记**：它们在 frame() 里被从下发的
				// 帧中删除，要等参数整组校验通过才在收尾时交付，此刻客户端什么都还没收到，
				// 重试是安全的。标记它们会让掐流重试白白失效。
				if key == "refusal" {
					if w.marked == nil {
						w.marked = map[any]bool{}
					}
					w.marked[choice["index"]] = true
				}
			}
		}
		for _, key := range []string{"content", "reasoning_content"} {
			text, _ := delta[key].(string)
			if text == "" {
				continue
			}
			// 只要真的下发过正文/思考，就要记「已交付」——这与 keepText 无关。
			// 此前只在 keepText=false 的分支里记，于是结构化输出（keepText=true）
			// 时 DeliveredContent() 恒为 false，掐流重试会在客户端已收到内容后重发，
			// 造成重复输出（2026-09-30 深度体检发现）。
			if w.marked == nil {
				w.marked = map[any]bool{}
			}
			first := !w.marked[choice["index"]]
			w.marked[choice["index"]] = true
			if keepText {
				slimDelta[key] = text
				informative = true
			} else if first {
				// 每个 choice 首次出现输出时留一个占位字符，让聚合仍能区分
				// 「有输出但被截断」与「空响应」；正文本身不入缓冲。
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
//
// 幂等：失败终态只能交付一次。若已被别的路径写过（terminal 或 checked），
// 这里返回 false 让调用方去走自己的兜底，而不是再写一个 [DONE]。
func (w *chatContractWriter) StreamFailure(code, message string) bool {
	if !w.streaming || w.writeErr != nil || w.checked {
		return false
	}
	encoded, err := json.Marshal(map[string]any{"error": map[string]any{"code": code, "message": message, "type": "api_error"}})
	if err != nil {
		return false
	}
	w.writeRaw(append(append([]byte("data: "), encoded...), '\n', '\n'))
	w.writeRaw([]byte("data: [DONE]\n\n"))
	w.Flush()
	// 标记已完成：否则 defer 的 finishResponseWriters 会再进 CompletionError，
	// 把 stale 的 raw 聚合一遍、再写一遍错误帧与 [DONE]（客户端看到重复终态）。
	w.checked = true
	w.err = nil
	return true
}

// HoldProgress 由 handler 在循环保护压制期调用：压制期上游的帧被压在内存里不发，
// 这里负责让客户端与反代知道连接还活着。
//
// 两难与取舍（2026-10-14 实测确认）：
//
//	重发资格（StreamError.Retryable）的前提是「客户端零字节」——只有零字节，响应头
//	才还没发出去，命中循环时才能整段丢弃、同账号静默重发，重发再失败时也才能返回
//	干净的 400 + JSON 错误体。这两条被 TestContextLoopRetryHTTPFailurePreservesRecovery
//	与 TestContextHTTPClassificationContract 锁定为契约，**不能**在压制一开始就写心跳。
//
//	但压制期上限是 ReasoningLoopHoldBackTimeout（120 秒），而这条链路是
//	Cloudflare(100 秒无响应即 524) → nginx(read_timeout 600s) → 网关。整个压制期
//	零字节，会在 Cloudflare 那层被判 524，客户端拿到的就不是循环错误而是网关超时。
//
// 折中：压制前期保持零字节（保住重发资格与 400 JSON 契约），只有盲等逼近反代上限时
// 才开流发心跳、把连接保下来。绝大多数循环在毫秒级就被判定（实测判定点集中在
// 9439–76312 字符），根本走不到这条路径，契约对它们完全不变；只有真正拖到接近上限的
// 极端形态才会放弃「干净 400」，换取「不被反代掐断」。
//
// 常量按最紧的一层（Cloudflare 100 秒）取值，留 15 秒余量。
func (w *chatContractWriter) HoldProgress() {
	// checked 表示终态（成功收尾或错误帧 + [DONE]）已经交付，此后任何写入都会落在
	// [DONE] 之后，客户端要么报解析错、要么丢弃。StreamFailure 与 CompletionError
	// 都守了这个标志，这里一并守住。
	if w.err != nil || w.writeErr != nil || w.checked {
		return
	}
	if !w.streaming {
		// 零字节盲等：先记起点，未逼近反代上限时什么都不写，保住重发资格。
		if w.holdSince.IsZero() {
			w.holdSince = time.Now()
		}
		if time.Since(w.holdSince) < chatHoldBackBlindWindow {
			return
		}
		// 已逼近反代上限：开流发心跳。此后响应头已定，失败只能按 SSE 帧交付。
		w.streaming = true
		w.writeRaw([]byte(": keepalive\n\n"))
		w.Flush()
		return
	}
	if time.Since(w.lastWrite) < protocolProgressInterval {
		return
	}
	w.writeRaw([]byte(": keepalive\n\n"))
	w.Flush()
}

// chatHoldBackBlindWindow 是 chat 协议在压制期允许「零字节盲等」的时长。
//
// 取值依据：这条链路最紧的一层是 Cloudflare 的 100 秒无响应判 524（nginx 侧是 600s，
// 不是瓶颈）。留 15 秒余量，让心跳在 85 秒时就发出去，确保 Cloudflare 先看到字节而不是
// 先判超时。超过这个窗口的压制属于极端形态，此时让出「干净 400」契约换取连接不被掐断。
const chatHoldBackBlindWindow = 85 * time.Second

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
	// 交付工具调用时把 finish_reason 归一为 tool_calls、空参数写成 {}，与流式路径
	// （CompletionError 的收尾）保持一致。此前只有流式做了这两件事，非流式客户端
	// 会同时拿到 "finish_reason":"stop" 和填好的 tool_calls：按 finish_reason
	// 分支的客户端直接丢掉这一轮工具调用；严格客户端 JSON.parse("") 还会抛错
	// （2026-09-30 深度体检发现）。
	delivers := w.deliversToolCalls(chat)
	for _, raw := range responseArray(chat["choices"]) {
		choice, _ := raw.(map[string]any)
		message, _ := choice["message"].(map[string]any)
		if choice["finish_reason"] == "length" || choice["finish_reason"] == "content_filter" {
			delete(message, "tool_calls")
			delete(message, "function_call")
		}
		if delivers {
			if finish, _ := choice["finish_reason"].(string); finish == "" || finish == "stop" {
				choice["finish_reason"] = "tool_calls"
			}
		}
		for _, value := range responseArray(message["tool_calls"]) {
			call, _ := value.(map[string]any)
			function, _ := call["function"].(map[string]any)
			if function == nil {
				continue
			}
			if args, _ := function["arguments"].(string); strings.TrimSpace(args) == "" {
				function["arguments"] = "{}"
			}
		}
		if function := legacyResponseFunction(message); function != nil {
			if args, _ := function["arguments"].(string); strings.TrimSpace(args) == "" {
				function["arguments"] = "{}"
			}
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
