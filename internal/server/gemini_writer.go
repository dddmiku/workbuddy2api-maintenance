// ═══ 更新日志 ═══
// 2026-09-25：保留逐密钥频率与并发响应头，客户端可读取共享额度与重试提示。
// 2026-09-25：统一错误文本的小写开头，通过最终静态检查而不改变错误代码或协议行为。
// 2026-09-25：Gemini 输出保留真实聚合用量和错误终态，SSE 只输出客户端可解析的数据帧。
// 2026-09-25：整组验证函数参数再交付，固定客户端请求的流式运输，并传播最终写出失败。
// 2026-09-25：将工具缓冲期注释保活转换为空 JSON data 帧，避免 Google 客户端零字节等待且不提前暴露工具。
// 2026-09-25：流错误同时提供原生 OTHER 失败终态，防止官方 SDK 丢弃 error 对象后只返回部分正文。
package server

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"sort"
	"strings"

	"workbuddy2api/internal/jsonutil"
	"workbuddy2api/internal/upstream"
)

const geminiBufferLimit = 16 << 20
const geminiEventLimit = 4 << 20

type geminiTool struct {
	id, name  string
	arguments strings.Builder
}

type geminiWriter struct {
	inner                  http.ResponseWriter
	hdr                    http.Header
	status                 int
	model, id              string
	stream, started, ended bool
	buffer                 []byte
	usage                  map[string]any
	stop                   string
	hasContent             bool
	includeThoughts        bool
	tools                  map[int]*geminiTool
	toolBytes              int
	err                    error
}

func newGeminiWriter(w http.ResponseWriter) *geminiWriter {
	return &geminiWriter{inner: w, hdr: make(http.Header), status: 200, id: "gw_" + rand.Text(), tools: map[int]*geminiTool{}}
}

func (g *geminiWriter) Header() http.Header         { return g.hdr }
func (g *geminiWriter) Unwrap() http.ResponseWriter { return g.inner }
func (g *geminiWriter) WriteHeader(code int)        { g.status = code }
func (g *geminiWriter) CompletionError() error      { return g.err }
func (g *geminiWriter) FinishResponse() error       { g.finish(); return g.err }
func (g *geminiWriter) Flush()                      { _ = g.FlushError() }
func (g *geminiWriter) FlushError() error {
	if g.started && g.err == nil {
		if err := http.NewResponseController(g.inner).Flush(); err != nil && !errors.Is(err, http.ErrNotSupported) {
			g.err = err
		}
	}
	if check, ok := g.inner.(interface{ CompletionError() error }); ok && g.err == nil {
		g.err = check.CompletionError()
	}
	return g.err
}

func (g *geminiWriter) Write(data []byte) (int, error) {
	if g.err != nil {
		return 0, g.err
	}
	if g.ended {
		return len(data), nil
	}
	if len(data) > geminiBufferLimit-len(g.buffer) {
		g.failure(502, "upstream_response_too_large", "upstream response exceeds the adapter limit")
		return 0, g.err
	}
	g.buffer = append(g.buffer, data...)
	if !strings.HasPrefix(g.hdr.Get("Content-Type"), "text/event-stream") {
		if !json.Valid(g.buffer) {
			return len(data), nil
		}
		var object map[string]any
		if jsonutil.Decode(g.buffer, &object) != nil || object == nil {
			g.failure(502, "upstream_parse", "invalid upstream response")
			return 0, g.err
		}
		g.buffer = nil
		if problem, ok := object["error"].(map[string]any); ok {
			g.upstreamFailure(g.status, problem)
			return len(data), g.err
		}
		if g.status >= 400 {
			g.failure(g.status, "upstream_error", "upstream returned an unsuccessful response")
			return len(data), g.err
		}
		converted, err := g.convert(object)
		if err != nil {
			g.failure(502, "response_contract_violation", err.Error())
			return 0, g.err
		}
		if g.stream {
			// A JSON upstream response must not change the requested transport:
			// NF's agent chat parser requires SSE even for a single complete reply.
			g.event(converted)
			g.ended = true
		} else {
			g.nativeJSON(g.status, converted)
		}
		return len(data), g.err
	}
	for !g.ended && g.err == nil {
		index, delimiter := messagesFrameEnd(g.buffer)
		if index < 0 {
			break
		}
		frame := string(g.buffer[:index])
		g.buffer = g.buffer[index+delimiter:]
		var lines []string
		comment := false
		for _, line := range strings.Split(strings.ReplaceAll(strings.ReplaceAll(frame, "\r\n", "\n"), "\r", "\n"), "\n") {
			if strings.HasPrefix(line, "data:") {
				lines = append(lines, strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
			} else if strings.HasPrefix(line, ":") {
				comment = true
			}
		}
		payload := strings.TrimSpace(strings.Join(lines, "\n"))
		if payload == "" {
			if comment {
				// Chat may be holding every tool delta until the whole group
				// validates. Keep the downstream alive without tool identifiers,
				// content, usage or a terminal signal. Google SDKs that reject
				// comment frames can parse this ordinary empty JSON data frame.
				g.event(map[string]any{})
			}
			continue
		}
		if payload == "[DONE]" {
			g.complete()
			break
		}
		var object map[string]any
		if jsonutil.Decode([]byte(payload), &object) != nil || object == nil {
			g.failure(502, "upstream_parse", "invalid stream event")
			break
		}
		if usage, ok := object["usage"].(map[string]any); ok {
			g.usage = upstream.MergeUsage(g.usage, usage)
		}
		if problem, ok := object["error"].(map[string]any); ok {
			g.upstreamFailure(502, problem)
			break
		}
		choices, choicesOK := object["choices"].([]any)
		if object["choices"] != nil && !choicesOK {
			g.failure(502, "response_contract_violation", "upstream choices must be an array")
			break
		}
		if len(choices) > 1 {
			g.failure(502, "response_contract_violation", "gemini expects one output choice")
			break
		}
		for _, raw := range choices {
			choice, ok := raw.(map[string]any)
			if !ok {
				g.failure(502, "response_contract_violation", "upstream choice must be an object")
				break
			}
			if value := choice["index"]; value != nil {
				if index, ok := upstream.UsageCount(value); !ok || index != 0 {
					g.failure(502, "response_contract_violation", "gemini expects output choice index zero")
					break
				}
			}
			if err := requestValidationOptionalStrings(choice, "upstream.choice", "finish_reason"); err != nil {
				g.failure(502, "response_contract_violation", err.Error())
				break
			}
			if stop := stringField(choice, "finish_reason"); stop != "" {
				if g.stop != "" && g.stop != stop {
					g.failure(502, "response_contract_violation", "upstream changed its finish reason")
					break
				}
				g.stop = stop
			}
			delta, ok := choice["delta"].(map[string]any)
			if choice["delta"] != nil && !ok {
				g.failure(502, "response_contract_violation", "upstream delta must be an object")
				break
			}
			if err := requestValidationOptionalStrings(delta, "upstream.delta", "reasoning_content", "content", "refusal"); err != nil {
				g.failure(502, "response_contract_violation", err.Error())
				break
			}
			rawTools, toolsOK := delta["tool_calls"].([]any)
			if delta["tool_calls"] != nil && !toolsOK {
				g.failure(502, "response_contract_violation", "upstream tool_calls must be an array")
				break
			}
			for _, field := range []string{"reasoning_content", "content", "refusal"} {
				if field == "reasoning_content" && !g.includeThoughts {
					continue
				}
				if text := stringField(delta, field); text != "" {
					part := map[string]any{"text": text}
					if field == "reasoning_content" {
						part["thought"] = true
					}
					g.hasContent = true
					g.event(g.response([]any{part}, "", nil))
				}
			}
			for _, raw := range rawTools {
				call, _ := raw.(map[string]any)
				if err := g.toolDelta(call); err != nil {
					g.failure(502, "response_contract_violation", err.Error())
					break
				}
			}
			if delta["function_call"] != nil {
				g.failure(502, "response_contract_violation", "legacy function_call output cannot be replayed safely without a call ID")
			}
		}
	}
	if g.err != nil {
		return 0, g.err
	}
	return len(data), nil
}

func (g *geminiWriter) response(parts []any, stop string, usage map[string]any) map[string]any {
	candidate := map[string]any{"index": 0}
	if len(parts) > 0 {
		candidate["content"] = map[string]any{"role": "model", "parts": parts}
	}
	if stop != "" {
		candidate["finishReason"] = stop
	}
	result := map[string]any{"candidates": []any{candidate}, "modelVersion": g.model, "responseId": g.id}
	if metadata := geminiUsage(usage); metadata != nil {
		result["usageMetadata"] = metadata
	}
	return result
}

func (g *geminiWriter) convert(chat map[string]any) (map[string]any, error) {
	choices := responseArray(chat["choices"])
	if len(choices) != 1 {
		return nil, fmt.Errorf("gemini expects one output choice")
	}
	choice, _ := choices[0].(map[string]any)
	if value := choice["index"]; value != nil {
		if index, ok := upstream.UsageCount(value); !ok || index != 0 {
			return nil, fmt.Errorf("gemini expects output choice index zero")
		}
	}
	message, ok := choice["message"].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("upstream message must be an object")
	}
	if err := requestValidationOptionalStrings(message, "upstream.message", "reasoning_content", "content", "refusal"); err != nil {
		return nil, err
	}
	stop := geminiFinishReason(stringField(choice, "finish_reason"))
	if stop == "" {
		return nil, fmt.Errorf("upstream did not provide a valid finish reason")
	}
	parts := make([]any, 0)
	for _, key := range []string{"reasoning_content", "content", "refusal"} {
		if key == "reasoning_content" && !g.includeThoughts {
			continue
		}
		if text := stringField(message, key); text != "" {
			part := map[string]any{"text": text}
			if key == "reasoning_content" {
				part["thought"] = true
			}
			parts = append(parts, part)
		}
	}
	if message["function_call"] != nil {
		return nil, fmt.Errorf("legacy function_call output cannot be replayed safely without a call ID")
	}
	rawTools, toolsOK := message["tool_calls"].([]any)
	if message["tool_calls"] != nil && !toolsOK {
		return nil, fmt.Errorf("upstream tool_calls must be an array")
	}
	for i, raw := range rawTools {
		call, ok := raw.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("upstream tool call must be an object")
		}
		adapted := make(map[string]any, len(call)+1)
		for key, value := range call {
			adapted[key] = value
		}
		adapted["index"] = i
		if err := g.toolDelta(adapted); err != nil {
			return nil, err
		}
	}
	tools, err := g.validatedTools()
	if err != nil {
		return nil, err
	}
	if stringField(choice, "finish_reason") == "tool_calls" && len(tools) == 0 {
		return nil, fmt.Errorf("upstream tool finish reason contains no tool")
	}
	if stop != "STOP" && len(tools) > 0 {
		return nil, fmt.Errorf("upstream truncated or blocked a tool response")
	}
	parts = append(parts, tools...)
	if len(parts) == 0 && stop == "STOP" {
		return nil, fmt.Errorf("upstream returned an empty response")
	}
	usage, _ := chat["usage"].(map[string]any)
	return g.response(parts, stop, usage), nil
}

func (g *geminiWriter) complete() {
	if g.ended || g.err != nil {
		return
	}
	stop := geminiFinishReason(g.stop)
	if stop == "" || (!g.hasContent && len(g.tools) == 0 && stop == "STOP") {
		g.failure(502, "upstream_incomplete", "upstream returned no content or no valid finish reason")
		return
	}
	parts, err := g.validatedTools()
	if err != nil {
		g.failure(502, "invalid_tool_arguments", err.Error())
		return
	}
	if (g.stop == "tool_calls" && len(parts) == 0) || (stop != "STOP" && len(parts) > 0) {
		g.failure(502, "response_contract_violation", "upstream tool completion is missing, truncated or blocked")
		return
	}
	// Validate the entire group, including event sizes, before exposing any
	// executable functionCall. NF runs every received part as a distinct call.
	for _, part := range parts {
		g.event(g.response([]any{part}, "", nil))
	}
	g.event(g.response(nil, stop, g.usage))
	g.ended = true
}

func (g *geminiWriter) toolDelta(call map[string]any) error {
	index, ok := upstream.UsageCount(call["index"])
	if !ok {
		return fmt.Errorf("upstream tool index must be a nonnegative integer")
	}
	if err := requestValidationOptionalStrings(call, "upstream.tool", "id", "type"); err != nil {
		return err
	}
	if kind := stringField(call, "type"); kind != "" && kind != "function" {
		return fmt.Errorf("upstream tool must use function type")
	}
	tool := g.tools[index]
	if tool == nil {
		if len(g.tools) >= 1024 {
			return fmt.Errorf("upstream exceeded the adapter tool count limit")
		}
		tool = &geminiTool{}
		g.tools[index] = tool
	}
	if id := stringField(call, "id"); id != "" {
		if tool.id != "" && tool.id != id {
			return fmt.Errorf("upstream changed a tool call ID")
		}
		for otherIndex, other := range g.tools {
			if otherIndex != index && other.id == id {
				return fmt.Errorf("upstream reused a tool call ID")
			}
		}
		if tool.id == "" {
			tool.id = id
			g.toolBytes += len(id)
		}
	}
	if call["function"] == nil {
		if g.toolBytes > geminiEventLimit {
			return fmt.Errorf("upstream tool metadata exceed the adapter limit")
		}
		return nil
	}
	function, err := requestValidationObject(call["function"], "upstream.tool.function")
	if err != nil {
		return err
	}
	if err := requestValidationOptionalStrings(function, "upstream.tool.function", "name"); err != nil {
		return err
	}
	if name := stringField(function, "name"); name != "" {
		if tool.name != "" && tool.name != name {
			return fmt.Errorf("upstream changed a function name")
		}
		if tool.name == "" {
			tool.name = name
			g.toolBytes += len(name)
		}
	}
	if raw, exists := function["arguments"]; exists {
		args, ok := raw.(string)
		if !ok {
			return fmt.Errorf("upstream tool arguments must be a JSON string")
		}
		g.toolBytes += len(args)
		if g.toolBytes > geminiEventLimit {
			return fmt.Errorf("upstream tool arguments exceed the adapter limit")
		}
		tool.arguments.WriteString(args)
	}
	if g.toolBytes > geminiEventLimit {
		return fmt.Errorf("upstream tool metadata exceed the adapter limit")
	}
	return nil
}

func (g *geminiWriter) validatedTools() ([]any, error) {
	indices := make([]int, 0, len(g.tools))
	for index := range g.tools {
		indices = append(indices, index)
	}
	sort.Ints(indices)
	parts := make([]any, 0, len(indices))
	for _, index := range indices {
		tool := g.tools[index]
		var args map[string]any
		if strings.TrimSpace(tool.id) == "" || strings.TrimSpace(tool.name) == "" || jsonutil.Decode([]byte(tool.arguments.String()), &args) != nil || args == nil {
			return nil, fmt.Errorf("upstream ended with incomplete or invalid tool arguments")
		}
		part := map[string]any{"functionCall": map[string]any{"id": tool.id, "name": tool.name, "args": args}}
		encoded, err := json.Marshal(g.response([]any{part}, "", nil))
		if err != nil || len(encoded) > geminiEventLimit {
			return nil, fmt.Errorf("upstream tool cannot fit in a complete Gemini event")
		}
		parts = append(parts, part)
	}
	return parts, nil
}

func (g *geminiWriter) finish() {
	if g.ended || g.err != nil {
		return
	}
	if len(g.buffer) > 0 {
		g.failure(502, "upstream_incomplete", "upstream ended with an incomplete response")
		return
	}
	g.complete()
}

func (g *geminiWriter) copyHeaders() {
	for _, key := range []string{"Retry-After", "X-Request-ID", "X-Gateway-Capabilities", "Cache-Control", "X-RateLimit-Limit", "X-RateLimit-Remaining", "X-Concurrency-Limit"} {
		if value := g.hdr.Get(key); value != "" {
			g.inner.Header().Set(key, value)
		}
	}
}

func (g *geminiWriter) nativeJSON(status int, payload any) {
	if g.ended || g.err != nil {
		return
	}
	g.copyHeaders()
	g.err = writeJSON(g.inner, status, payload)
	g.ended = true
}

func (g *geminiWriter) event(payload map[string]any) {
	if g.err != nil || g.ended {
		return
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		g.err = err
		return
	}
	if len(encoded) > geminiEventLimit {
		g.failure(502, "upstream_response_too_large", "upstream event exceeds the Gemini event limit")
		return
	}
	if !g.started {
		g.copyHeaders()
		g.inner.Header().Set("Content-Type", "text/event-stream")
		g.inner.Header().Set("Cache-Control", "no-cache")
		g.inner.Header().Set("X-Accel-Buffering", "no")
		g.inner.WriteHeader(200)
		g.started = true
	}
	raw := append(append([]byte("data: "), encoded...), '\n', '\n')
	if n, err := g.inner.Write(raw); err != nil {
		g.err = err
	} else if n != len(raw) {
		g.err = fmt.Errorf("short downstream write")
	}
	_ = g.FlushError()
}

func (g *geminiWriter) upstreamFailure(status int, problem map[string]any) {
	if detail, ok := upstream.ContextTooLongErrorDetail(problem); ok {
		g.failure(400, "context_length_exceeded", detail)
		return
	}
	if status < 400 || status == 502 {
		if code, ok := upstream.UsageCount(problem["code"]); ok && code >= 400 && code <= 599 {
			status = code
		} else {
			kind := stringField(problem, "type")
			if kind == "" {
				kind = stringField(problem, "status")
			}
			switch kind {
			case "invalid_request_error", "INVALID_ARGUMENT", "FAILED_PRECONDITION":
				status = 400
			case "authentication_error", "UNAUTHENTICATED":
				status = 401
			case "permission_error", "PERMISSION_DENIED":
				status = 403
			case "not_found_error", "NOT_FOUND":
				status = 404
			case "rate_limit_error", "RESOURCE_EXHAUSTED":
				status = 429
			case "overloaded_error", "UNAVAILABLE":
				status = 503
			case "request_too_large":
				status = 413
			case "CANCELLED":
				status = 499
			case "UNIMPLEMENTED":
				status = 501
			}
		}
	}
	message := stringField(problem, "message")
	if message == "" {
		message = stringField(problem, "msg")
	}
	g.failure(status, stringField(problem, "code"), message)
}

func (g *geminiWriter) failure(status int, code, message string) {
	if g.ended || g.err != nil {
		return
	}
	if status < 400 || status > 599 {
		status = 502
	}
	if message == "" {
		message = "request failed"
	}
	problem := map[string]any{"code": status, "status": geminiErrorStatus(status), "message": message}
	if code != "" {
		problem["details"] = []any{map[string]any{"@type": "type.googleapis.com/google.rpc.ErrorInfo", "reason": code, "domain": "workbuddy2api"}}
	}
	payload := map[string]any{"error": problem}
	if g.started {
		// The official JS SDK drops error objects in SSE response conversion.
		// Preserve the error for NF and expose a native failure terminal for
		// SDK consumers; this is never a STOP or an executable tool response.
		payload["candidates"] = []any{map[string]any{"index": 0, "finishReason": "OTHER", "finishMessage": message}}
		g.event(payload)
	} else {
		g.copyHeaders()
		g.err = writeJSON(g.inner, status, payload)
	}
	g.ended = true
	if g.err == nil {
		g.err = errors.New(message)
	}
}

func geminiFinishReason(reason string) string {
	switch reason {
	case "stop", "tool_calls":
		return "STOP"
	case "length":
		return "MAX_TOKENS"
	case "content_filter":
		return "SAFETY"
	}
	return ""
}

func geminiErrorStatus(status int) string {
	switch status {
	case 400, 413, 415, 422:
		return "INVALID_ARGUMENT"
	case 401:
		return "UNAUTHENTICATED"
	case 403:
		return "PERMISSION_DENIED"
	case 404:
		return "NOT_FOUND"
	case 429:
		return "RESOURCE_EXHAUSTED"
	case 499:
		return "CANCELLED"
	case 501:
		return "UNIMPLEMENTED"
	case 502, 503, 504, 529:
		return "UNAVAILABLE"
	default:
		return "INTERNAL"
	}
}

func geminiUsage(usage map[string]any) map[string]any {
	if len(usage) == 0 {
		return nil
	}
	p, hasP := upstream.UsageCount(usage["prompt_tokens"])
	c, hasC := upstream.UsageCount(usage["completion_tokens"])
	details, _ := usage["completion_tokens_details"].(map[string]any)
	r, hasR := 0, false
	for _, candidate := range []any{details["reasoning_tokens"], usage["completion_thinking_tokens"], usage["reasoning_tokens"]} {
		if count, ok := upstream.UsageCount(candidate); ok {
			r, hasR = count, true
			break
		}
	}
	result := map[string]any{}
	inconsistent := false
	if hasP {
		result["promptTokenCount"] = p
	}
	if k, ok := upstream.CachedInputTokens(usage); ok {
		if hasP && k > p {
			inconsistent = true
		} else {
			result["cachedContentTokenCount"] = k
		}
	}
	validR := hasR && (!hasC || r <= c)
	if hasR && !validR {
		inconsistent = true
	}
	if validR {
		result["thoughtsTokenCount"] = r
	}
	if hasC {
		result["candidatesTokenCount"] = c
		if validR {
			result["candidatesTokenCount"] = c - r
		}
	}
	complete := hasP && hasC && c <= math.MaxInt-p
	if complete {
		result["totalTokenCount"] = p + c
	} else if hasP && hasC {
		inconsistent = true
	}
	// NF currently reads candidatesTokenCount but ignores totalTokenCount.
	// With no measured R, retain C for that client and explicitly identify this
	// field as aggregate output, never fabricate a candidates/thoughts split.
	extension := map[string]any{"source": "upstream", "complete": complete, "thoughtsReported": validR, "candidatesIncludeThoughts": hasC && !validR}
	if hasC {
		extension["outputTokenCount"] = c
	}
	if inconsistent {
		extension["inconsistent"] = true
	}
	result["gatewayUsage"] = extension
	return result
}
