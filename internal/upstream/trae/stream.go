// stream.go — Trae llm_utils_chat 的 SSE 事件流 → OpenAI chat.completions SSE。
//
// 这是嵌入 Trae2api-cn 的核心：上游发的是 Trae 私有事件族（output / token_usage /
// done），下游整条协议栈（Chat / Anthropic / Responses / 面板 / 池 / 粘性）只认
// OpenAI SSE。转换器把私有事件归一成标准帧，于是 Trae 账号可以和既有 realm
// 一样复用全部下游能力——这正是"模型使用相关用我们写好的模式"的落点。
//
// 移植自 Trae2api-cn 的 sse.py（translate_ide_stream 及其累加器）。保留的语义：
//   - 文本可能是**累计快照**也可能是**增量**：两者都按前缀合并（add）；
//   - 工具调用按 index 跨帧合并（首片带 id/name，后续只带参数片段）；
//   - `<think>…</think>` 从正文里拆出来当 reasoning，不混进回答；
//   - 没有终态事件就是截断（require_terminal），不能让半截回答伪装成功；
//   - 上游自造的 finish_reason 归一（对齐网关既有的 validFinishReason 口径）。
//
// ═══ 更新日志 ═══
// 2026-10-11：初版——事件→OpenAI SSE 转译 + 工具累加器 + 终态校验。
package trae

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"workbuddy2api/internal/auth"
)

// 上游事件名（Trae llm_utils_chat）。
const (
	evOutput     = "output"
	evTokenUsage = "token_usage"
	evDone       = "done"
	evError      = "error"
	// 远程/CLI 通道会用这些名字发心跳，保持连接不静默。
	evHeartbeat = "heartbeat"
)

// thinkOpen/thinkClose 是网关侧把 reasoning 包回正文的标记（对齐 Trae2api-cn）。
const (
	thinkOpen  = " thinking\n\n"
	thinkClose = "\n response\n\n"
)

var chunkSeq uint64

// ChatStream 调用 llm_utils_chat 并返回**已转译成 OpenAI SSE** 的流。
//
// 返回的 io.ReadCloser 与既有 upstream.ChatStreamContext 契约一致：调用方直接
// 交给 upstream.Stream / Aggregate，无需知道上游是 Trae。上游非 2xx 时返回
// (nil, status, body, nil) 让上层按既有错误分类处理（对齐既有出站实现）。
func ChatStream(ctx context.Context, a *auth.Auth, prepared []byte, clientIP string) (io.ReadCloser, int, []byte, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	requestID := newRequestID()
	body, err := BuildChatBody(prepared, requestID)
	if err != nil {
		return nil, 0, nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, GatewayBase+ChatPath, bytes.NewReader(body))
	if err != nil {
		return nil, 0, nil, err
	}
	headers := buildHeaders(a)
	for k, vs := range headers {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	if clientIP != "" {
		req.Header.Set("X-Forwarded-For", clientIP)
	}
	client := &http.Client{Transport: chatTransport()}
	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		defer resp.Body.Close()
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		return nil, resp.StatusCode, raw, nil
	}
	model := modelFromPrepared(prepared)
	tr := newTranslator(model)
	return newTranslateReader(resp.Body, tr), resp.StatusCode, nil, nil
}

// chatTransport 是 SSE 专用 Transport：首字节有超时、流中空闲交给上游事件心跳，
// 不设总时长（长回答不能因为整体耗时被砍断）。
func chatTransport() http.RoundTripper {
	return &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		MaxIdleConns:          64,
		MaxIdleConnsPerHost:   32,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   15 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		ResponseHeaderTimeout: 120 * time.Second,
	}
}

func newRequestID() string {
	n := atomic.AddUint64(&chunkSeq, 1)
	return fmt.Sprintf("trae-%d-%d", time.Now().UnixNano(), n)
}

func modelFromPrepared(prepared []byte) string {
	var src map[string]any
	if json.Unmarshal(prepared, &src) != nil {
		return ""
	}
	model, _ := src["model"].(string)
	return model
}

// ── 文本累加器 ────────────────────────────────────────────────────────────

// textAccumulator 合并「累计快照」与「增量」两种上游形态。
//
// Trae 的 output 事件有时发完整快照（response 是到目前为止的全部文本），有时只发
// 新增片段，两种会在同一条流里混用。判定规则来自 Trae2api-cn 的实测：
//   - 新值以旧值开头 → 是更新的快照，替换；
//   - 旧值以新值开头 → 是迟到的旧快照，丢弃；
//   - 公共前缀够长（≥4 或超过较短者一半）→ 认为是对同一段的重写，替换；
//   - 否则 → 视为纯增量，追加。
type textAccumulator struct {
	raw     string
	visible string
}

// add 吸收一个新值，返回本次应当交付给客户端的**新增**可见文本。
func (t *textAccumulator) add(value string) string {
	if value == "" {
		return ""
	}
	switch {
	case strings.HasPrefix(value, t.raw):
		t.raw = value
	case strings.HasPrefix(t.raw, value):
		// 旧快照，不改动。
	default:
		common := commonPrefixLen(t.raw, value)
		shorter := min(len(t.raw), len(value))
		if common >= 4 || (shorter > 0 && common*2 >= shorter) {
			t.raw = value
		} else {
			t.raw += value
		}
	}
	return t.delta()
}

// finalize 在流结束时放行最后一个可见增量（含尾部不完整转义前缀）。
func (t *textAccumulator) finalize() string { return t.delta() }

func (t *textAccumulator) delta() string {
	visible := visibleText(t.raw)
	if visible == t.visible {
		return ""
	}
	addition := textDelta(t.visible, visible)
	t.visible = visible
	return addition
}

// visibleText 剥掉序列化在正文里的工具调用块（XML 形态），并压住不完整的开头。
func visibleText(raw string) string {
	return holdIncompleteToolBlock(stripToolCallBlocks(raw))
}

// textDelta 返回 current 相对 previous 的新增部分。
func textDelta(previous, current string) string {
	if current == "" {
		return ""
	}
	if strings.HasPrefix(current, previous) {
		return current[len(previous):]
	}
	if strings.HasPrefix(previous, current) {
		return ""
	}
	return current[commonPrefixLen(previous, current):]
}

func commonPrefixLen(left, right string) int {
	limit := min(len(left), len(right))
	index := 0
	for index < limit && left[index] == right[index] {
		index++
	}
	return index
}

// toolBlockPairs 是正文里可能出现的序列化工具块（上游把工具调用写进文本的形态）。
var toolBlockPairs = [][2]string{
	{"<opencode_tool_call>", "</opencode_tool_call>"},
	{"<tool_use>", "</tool_use>"},
	{"<tool_call", "</tool_call>"},
	{"<tool_cell", "</tool_cell>"},
	{"<invoke", "</invoke>"},
	{"<bash>", "</bash>"},
	{"<read>", "</read>"},
	{"<write>", "</write>"},
	{"<edit>", "</edit>"},
	{"<glob>", "</glob>"},
	{"<grep>", "</grep>"},
	{"<task>", "</task>"},
}

// stripToolCallBlocks 移除成对的工具块；未闭合的块由 holdIncompleteToolBlock 压住。
func stripToolCallBlocks(text string) string {
	out := text
	for _, pair := range toolBlockPairs {
		for {
			start := strings.Index(out, pair[0])
			if start < 0 {
				break
			}
			end := strings.Index(out[start+len(pair[0]):], pair[1])
			if end < 0 {
				break
			}
			out = out[:start] + out[start+len(pair[0])+end+len(pair[1]):]
		}
	}
	return out
}

// holdIncompleteToolBlock 截断到第一个「已开始但还没闭合」的工具块之前，
// 避免把半截工具标记当正文发给客户端。
func holdIncompleteToolBlock(text string) string {
	lowered := strings.ToLower(text)
	earliest := len(text)
	for _, pair := range toolBlockPairs {
		start := strings.Index(lowered, pair[0])
		if start < 0 {
			continue
		}
		if strings.Index(lowered[start+len(pair[0]):], pair[1]) < 0 {
			earliest = min(earliest, start)
		}
	}
	// 可能正好停在 "<tool_c" 这种前缀上：压住最后一个 '<' 起的尾巴。
	if last := strings.LastIndex(lowered, "<"); last >= 0 {
		suffix := lowered[last:]
		for _, pair := range toolBlockPairs {
			if strings.HasPrefix(pair[0], suffix) {
				earliest = min(earliest, last)
				break
			}
		}
	}
	return text[:earliest]
}

// ── 工具调用累加器 ────────────────────────────────────────────────────────

type toolCallState struct {
	id        string
	typ       string
	name      string
	arguments strings.Builder
	seenArgs  bool
}

// toolAccumulator 按 index 合并工具调用片段。
//
// Trae 原生形态：首片带 id/name，后续片段只带 arguments 增量、不带 id/name。
// 因此 index 是唯一稳定的合并键（Trae2api-cn 的 ToolCallAccumulator 结论）。
type toolAccumulator struct {
	order []int
	calls map[int]*toolCallState
}

func newToolAccumulator() *toolAccumulator {
	return &toolAccumulator{calls: map[int]*toolCallState{}}
}

func (t *toolAccumulator) hasCalls() bool { return len(t.calls) > 0 }

// absorb 吸收一批工具片段，返回本帧要交付的 delta 列表（OpenAI 形状）。
func (t *toolAccumulator) absorb(items []any) []map[string]any {
	var deltas []map[string]any
	for fallback, raw := range items {
		call, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		index := fallback
		if value, ok := call["index"].(float64); ok {
			index = int(value)
		}
		state := t.calls[index]
		if state == nil {
			state = &toolCallState{}
			t.calls[index] = state
			t.order = append(t.order, index)
		}
		delta := map[string]any{"index": index}
		if id, _ := call["id"].(string); id != "" {
			if state.id == "" {
				state.id = id
				delta["id"] = id
			}
		}
		if typ, _ := call["type"].(string); typ != "" {
			if state.typ == "" {
				state.typ = typ
				delta["type"] = typ
			}
		}
		fn, _ := call["function"].(map[string]any)
		if fn == nil {
			// 紧凑形态：name/arguments 直接放在调用对象上。
			fn = call
		}
		fnDelta := map[string]any{}
		if name, _ := fn["name"].(string); name != "" && state.name == "" {
			state.name = name
			fnDelta["name"] = name
		}
		if args, ok := fn["arguments"].(string); ok && args != "" {
			// 参数可能是累计快照也可能是增量，与正文同一套合并规则。
			addition := args
			if existing := state.arguments.String(); existing != "" {
				addition = textDelta(existing, args)
				if addition == "" && strings.HasPrefix(existing, args) {
					continue
				}
			}
			state.arguments.WriteString(addition)
			state.seenArgs = true
			fnDelta["arguments"] = addition
		}
		if len(fnDelta) > 0 {
			delta["function"] = fnDelta
		}
		if len(delta) > 1 {
			deltas = append(deltas, delta)
		}
	}
	return deltas
}

// ── 转译器 ────────────────────────────────────────────────────────────────

type translator struct {
	model     string
	id        string
	text      *textAccumulator
	reasoning *textAccumulator
	tools     *toolAccumulator

	usage        map[string]any
	finishReason string
	sawTerminal  bool
	started      bool
	contentCount int
	// thinkingOpen 记录是否已经输出过 think 开头，用于把 reasoning 包成 think 块。
	thinkingOpen bool
	thinkingDone bool
}

func newTranslator(model string) *translator {
	return &translator{
		model:     model,
		id:        "chatcmpl-trae-" + newRequestID(),
		text:      &textAccumulator{},
		reasoning: &textAccumulator{},
		tools:     newToolAccumulator(),
	}
}

// emit 把一条 OpenAI SSE 帧写进输出缓冲。
func (t *translator) emit(out *bytes.Buffer, delta map[string]any, finish any) {
	choice := map[string]any{"index": 0, "delta": delta, "finish_reason": finish}
	frame := map[string]any{
		"id":      t.id,
		"object":  "chat.completion.chunk",
		"created": time.Now().Unix(),
		"model":   t.model,
		"choices": []any{choice},
	}
	encoded, err := json.Marshal(frame)
	if err != nil {
		return
	}
	out.WriteString("data: ")
	out.Write(encoded)
	out.WriteString("\n\n")
}

func (t *translator) start(out *bytes.Buffer) {
	if t.started {
		return
	}
	t.started = true
	t.emit(out, map[string]any{"role": "assistant"}, nil)
}

// handleEvent 处理一个上游事件，把产生的 OpenAI 帧写进 out。
// 返回 done=true 表示上游已给出终态，停止消费。
func (t *translator) handleEvent(name string, data []byte, out *bytes.Buffer) (bool, error) {
	name = normalizeEventName(name)
	var obj map[string]any
	if len(data) > 0 {
		if err := json.Unmarshal(data, &obj); err != nil {
			// 事件名已经说明是终态时，载荷不合法也算收尾（对齐 Trae2api-cn）。
			if name == evDone {
				t.sawTerminal = true
				return true, nil
			}
			return false, nil
		}
	}
	if obj == nil {
		obj = map[string]any{}
	}
	if name == "" {
		name = payloadEventName(obj)
	}

	switch name {
	case evError:
		message, _ := obj["message"].(string)
		if message == "" {
			message = "Trae upstream returned an error event"
		}
		return false, fmt.Errorf("%s", message)
	case evHeartbeat:
		// 心跳：保持连接活着，不产生内容。
		out.WriteString(": relay-keepalive\n\n")
		return false, nil
	case evTokenUsage:
		if usage, ok := obj["usage"].(map[string]any); ok {
			t.usage = mapUsage(usage)
		} else {
			t.usage = mapUsage(obj)
		}
		return false, nil
	case evDone:
		t.sawTerminal = true
		if reason := payloadFinishReason(obj); reason != "" {
			t.finishReason = reason
		}
		return true, nil
	}

	// output 事件（或没有事件名的 data 帧）：正文 + reasoning + 工具。
	reasoning := payloadReasoningText(obj)
	rawResponse, _ := obj["response"].(string)
	embeddedReasoning, visibleResponse := splitEmbeddedReasoning(rawResponse)

	// reasoning 先发：客户端要能区分"思考"与"回答"。
	for _, chunk := range []string{reasoning, embeddedReasoning} {
		if chunk == "" {
			continue
		}
		if addition := t.reasoning.add(chunk); addition != "" {
			t.start(out)
			t.emit(out, map[string]any{"reasoning_content": addition}, nil)
		}
	}
	if addition := t.text.add(visibleResponse); addition != "" {
		t.start(out)
		t.contentCount++
		t.emit(out, map[string]any{"content": addition}, nil)
	}
	if calls, ok := obj["tool_calls"].([]any); ok && len(calls) > 0 {
		for _, delta := range t.tools.absorb(calls) {
			t.start(out)
			t.emit(out, map[string]any{"tool_calls": []any{delta}}, nil)
		}
	}
	if usage, ok := obj["usage"].(map[string]any); ok {
		t.usage = mapUsage(usage)
	}
	if reason := payloadFinishReason(obj); reason != "" {
		// 中间快照也会带 finish_reason，那只是元数据；只有 done 才是终态。
		t.finishReason = reason
	}
	return false, nil
}

// finish 在流结束时补齐尾部增量、终态帧与 [DONE]。
func (t *translator) finish(out *bytes.Buffer) error {
	if !t.sawTerminal {
		return fmt.Errorf("Trae upstream ended before its terminal event")
	}
	// 尾部增量（可能含最后一段正文或 reasoning）。
	if addition := t.text.finalize(); addition != "" {
		t.start(out)
		t.contentCount++
		t.emit(out, map[string]any{"content": addition}, nil)
	}
	if addition := t.reasoning.finalize(); addition != "" {
		t.start(out)
		t.emit(out, map[string]any{"reasoning_content": addition}, nil)
	}
	if t.contentCount == 0 && !t.tools.hasCalls() {
		t.start(out)
		t.emit(out, map[string]any{"content": "(trae upstream returned an empty response)"}, nil)
	}
	reason := finalFinishReason(t.finishReason, t.tools.hasCalls())
	t.emit(out, map[string]any{}, reason)
	if t.usage != nil {
		usageFrame := map[string]any{
			"id":      t.id,
			"object":  "chat.completion.chunk",
			"created": time.Now().Unix(),
			"model":   t.model,
			"choices": []any{},
			"usage":   t.usage,
		}
		if encoded, err := json.Marshal(usageFrame); err == nil {
			out.WriteString("data: ")
			out.Write(encoded)
			out.WriteString("\n\n")
		}
	}
	out.WriteString("data: [DONE]\n\n")
	return nil
}

// ── 事件字段抽取 ──────────────────────────────────────────────────────────

func normalizeEventName(value string) string {
	value = strings.TrimSpace(value)
	value = strings.TrimPrefix(value, "event:")
	value = strings.TrimSpace(value)
	value = strings.ToLower(value)
	return strings.ReplaceAll(value, "-", "_")
}

func payloadEventName(obj map[string]any) string {
	if value, ok := obj["event"].(string); ok {
		return normalizeEventName(value)
	}
	return evOutput
}

// payloadFinishReason 从多种拼写里取终态（Trae 各通道字段名不一致）。
func payloadFinishReason(obj map[string]any) string {
	for _, key := range []string{"finish_reason", "stop_reason", "finishReason", "stopReason"} {
		if value, ok := obj[key].(string); ok && value != "" {
			return value
		}
	}
	if message, ok := obj["message"].(map[string]any); ok {
		return payloadFinishReason(message)
	}
	return ""
}

// payloadReasoningText 取 reasoning 字段，不把它当正文。
func payloadReasoningText(obj map[string]any) string {
	for _, key := range []string{"reasoning_content", "reasoningContent", "thought", "thinking"} {
		switch value := obj[key].(type) {
		case string:
			if value != "" {
				return value
			}
		case map[string]any:
			for _, nested := range []string{"content", "text", "summary"} {
				if text, ok := value[nested].(string); ok && text != "" {
					return text
				}
			}
		}
	}
	if message, ok := obj["message"].(map[string]any); ok {
		return payloadReasoningText(message)
	}
	return ""
}

// finalFinishReason 把上游终态归一成下游契约认识的值。
//
// 与网关既有的 validFinishReason 口径一致；上游自造的终止值（error/abort/…）
// 归一到 stop——正文已经交付，不该因为一个词不认识就制造失败。
func finalFinishReason(value string, hasToolCalls bool) string {
	if hasToolCalls {
		return "tool_calls"
	}
	reason := strings.ToLower(strings.TrimSpace(value))
	switch reason {
	case "length", "max_tokens", "max_output_tokens", "max_token", "token_limit",
		"model_context_window_exceeded", "context_window_exceeded", "context_length_exceeded":
		return "length"
	case "content_filter", "content-filter", "safety":
		return "content_filter"
	}
	// stop / end_turn / eos / done / finished / complete，以及上游自造的
	// error/abort/canceled/failed/interrupted 与空值，一律按正常结束。
	return "stop"
}

// mapUsage 把 Trae 的 usage 归一成 OpenAI 形状。
func mapUsage(usage map[string]any) map[string]any {
	number := func(keys ...string) (int64, bool) {
		for _, key := range keys {
			if value, ok := usageCount(usage[key]); ok {
				return value, true
			}
		}
		return 0, false
	}
	out := map[string]any{}
	if input, ok := number("prompt_tokens", "input_token", "input_tokens", "promptTokenCount"); ok {
		out["prompt_tokens"] = input
	}
	if output, ok := number("completion_tokens", "output_token", "output_tokens", "candidatesTokenCount"); ok {
		out["completion_tokens"] = output
	}
	if total, ok := number("total_tokens", "totalTokenCount"); ok {
		out["total_tokens"] = total
	} else {
		in, _ := out["prompt_tokens"].(int64)
		oo, _ := out["completion_tokens"].(int64)
		out["total_tokens"] = in + oo
	}
	if cached, ok := number("cached_tokens", "cache_read_input_tokens", "prompt_cache_hit_tokens"); ok {
		out["prompt_tokens_details"] = map[string]any{"cached_tokens": cached}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func usageCount(value any) (int64, bool) {
	switch v := value.(type) {
	case float64:
		return int64(v), true
	case json.Number:
		n, err := v.Int64()
		if err != nil {
			return 0, false
		}
		return n, true
	case int64:
		return v, true
	case int:
		return int64(v), true
	}
	return 0, false
}

// ── <think> 拆分 ──────────────────────────────────────────────────────────

// splitEmbeddedReasoning 把正文里的 <think>…</think> 拆成 reasoning 与可见文本。
func splitEmbeddedReasoning(text string) (reasoning, visible string) {
	if text == "" {
		return "", ""
	}
	lowered := strings.ToLower(text)
	var parts []string
	var out strings.Builder
	index := 0
	for {
		open := indexOfAny(lowered[index:], "<think", "<thinking")
		if open < 0 {
			out.WriteString(text[index:])
			break
		}
		open += index
		// 找到开标签的 '>'
		tagEnd := strings.IndexByte(text[open:], '>')
		if tagEnd < 0 {
			out.WriteString(text[index:])
			break
		}
		tagEnd += open
		// 匹配的闭合标签与开标签同名
		tagName := "think"
		if strings.HasPrefix(lowered[open:], "<thinking") {
			tagName = "thinking"
		}
		closeTag := "</" + tagName + ">"
		close := strings.Index(lowered[tagEnd:], closeTag)
		if close < 0 {
			// 未闭合：整段留在可见文本里（由 holdIncompleteToolBlock 之外的原样透传）。
			out.WriteString(text[index:])
			break
		}
		close += tagEnd
		body := text[tagEnd+1 : close]
		if body != "" {
			parts = append(parts, body)
		}
		out.WriteString(text[index:open])
		index = close + len(closeTag)
	}
	return strings.Join(parts, "\n"), out.String()
}

func indexOfAny(haystack string, needles ...string) int {
	best := -1
	for _, needle := range needles {
		if at := strings.Index(haystack, needle); at >= 0 && (best < 0 || at < best) {
			best = at
		}
	}
	return best
}

// ── 流式读取器 ────────────────────────────────────────────────────────────

// translateReader 把上游 Trae SSE 惰性转成 OpenAI SSE。
//
// 用 goroutine + io.Pipe 而不是全量缓冲：下游的 chatStatsReader 会边读边统计
// TTFB 与 usage，必须让字节尽快流出去，否则长回答的 TTFB 会被整段生成时间吃掉。
type translateReader struct {
	pr     *io.PipeReader
	pw     *io.PipeWriter
	closer io.Closer
	done   chan struct{}
}

func newTranslateReader(upstream io.ReadCloser, tr *translator) *translateReader {
	pr, pw := io.Pipe()
	r := &translateReader{pr: pr, pw: pw, closer: upstream, done: make(chan struct{})}
	go r.run(upstream, tr)
	return r
}

func (r *translateReader) run(upstream io.ReadCloser, tr *translator) {
	defer close(r.done)
	var err error
	defer func() {
		_ = upstream.Close()
		_ = r.pw.CloseWithError(err)
	}()
	reader := bufio.NewReaderSize(upstream, 64*1024)
	var (
		out         bytes.Buffer
		eventName   string
		dataLines   []string
		sawUpstream bool
		terminal    bool
	)
	// flush 交付一个完整事件。terminal=true 表示上游已给出终态（done / [DONE]），
	// 停止消费但**不能**就此返回——尾部增量与终态帧还要在 finish 里补齐。
	flush := func() bool {
		if len(dataLines) == 0 {
			eventName = ""
			return false
		}
		payload := strings.Join(dataLines, "\n")
		dataLines = nil
		name := eventName
		eventName = ""
		sawUpstream = true
		if strings.TrimSpace(payload) == "[DONE]" {
			tr.sawTerminal = true
			return true
		}
		done, herr := tr.handleEvent(name, []byte(payload), &out)
		if herr != nil {
			err = herr
			return true
		}
		if out.Len() > 0 {
			if _, werr := r.pw.Write(out.Bytes()); werr != nil {
				err = werr
				return true
			}
			out.Reset()
		}
		return done
	}
	for {
		line, rerr := reader.ReadString('\n')
		if line != "" {
			trimmed := strings.TrimRight(line, "\r\n")
			switch {
			case trimmed == "":
				if flush() {
					terminal = true
				}
			case strings.HasPrefix(trimmed, ":"):
				// 上游注释行：转成下游心跳，客户端不会把它当内容。
				if _, werr := r.pw.Write([]byte(": relay-keepalive\n\n")); werr != nil {
					err = werr
					terminal = true
				}
			default:
				field, value, found := strings.Cut(trimmed, ":")
				if !found {
					continue
				}
				value = strings.TrimPrefix(value, " ")
				switch field {
				case "event":
					eventName = value
				case "data":
					dataLines = append(dataLines, value)
				}
			}
		}
		if terminal {
			break
		}
		if rerr != nil {
			if rerr == io.EOF {
				flush()
			} else {
				err = rerr
			}
			break
		}
	}
	if err != nil {
		return
	}
	if !sawUpstream {
		err = fmt.Errorf("Trae upstream returned no data events")
		return
	}
	// 尾部增量 + 终态帧 + [DONE]；缺终态时 finish 会返回截断错误。
	if ferr := tr.finish(&out); ferr != nil {
		err = ferr
		return
	}
	if out.Len() > 0 {
		if _, werr := r.pw.Write(out.Bytes()); werr != nil {
			err = werr
			return
		}
	}
}

func (r *translateReader) Read(p []byte) (int, error) { return r.pr.Read(p) }

func (r *translateReader) Close() error {
	_ = r.pr.Close()
	<-r.done
	return nil
}
