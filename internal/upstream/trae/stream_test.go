package trae

import (
	"encoding/json"
	"io"
	"strings"
	"testing"
)

// collect 驱动 translateReader，返回下游会看到的完整 OpenAI SSE 文本。
func collect(t *testing.T, upstream string) (string, error) {
	t.Helper()
	tr := newTranslator("glm-5.3")
	r := newTranslateReader(io.NopCloser(strings.NewReader(upstream)), tr)
	defer r.Close()
	data, err := io.ReadAll(r)
	return string(data), err
}

// frames 把 OpenAI SSE 文本拆成帧 JSON（跳过注释行与 [DONE]）。
func frames(t *testing.T, sse string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range strings.Split(sse, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		payload := strings.TrimPrefix(line, "data: ")
		if payload == "[DONE]" {
			continue
		}
		var frame map[string]any
		if err := json.Unmarshal([]byte(payload), &frame); err != nil {
			t.Fatalf("frame is not valid JSON: %q: %v", payload, err)
		}
		out = append(out, frame)
	}
	return out
}

// contentOf 拼出所有 content 增量。
func contentOf(t *testing.T, sse string) string {
	t.Helper()
	var b strings.Builder
	for _, frame := range frames(t, sse) {
		for _, raw := range frame["choices"].([]any) {
			choice := raw.(map[string]any)
			delta, _ := choice["delta"].(map[string]any)
			if text, ok := delta["content"].(string); ok {
				b.WriteString(text)
			}
		}
	}
	return b.String()
}

// TestTranslateIncrementalOutput 覆盖最常见的形态：output 事件发增量文本。
func TestTranslateIncrementalOutput(t *testing.T) {
	upstream := "event: output\ndata: {\"response\":\"Hello\"}\n\n" +
		"event: output\ndata: {\"response\":\", world\"}\n\n" +
		"event: token_usage\ndata: {\"input_token\":10,\"output_token\":3}\n\n" +
		"event: done\ndata: {\"finish_reason\":\"stop\"}\n\n"

	sse, err := collect(t, upstream)
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	if got := contentOf(t, sse); got != "Hello, world" {
		t.Errorf("content=%q want %q", got, "Hello, world")
	}
	if !strings.Contains(sse, "data: [DONE]") {
		t.Errorf("missing [DONE]: %q", sse)
	}
	// 终态帧必须带 stop。
	if !strings.Contains(sse, `"finish_reason":"stop"`) {
		t.Errorf("missing normalized finish_reason: %q", sse)
	}
	// usage 帧带上了映射后的 token 数。
	if !strings.Contains(sse, `"prompt_tokens":10`) || !strings.Contains(sse, `"completion_tokens":3`) {
		t.Errorf("usage not mapped: %q", sse)
	}
}

// TestTranslateCumulativeSnapshots 覆盖累计快照形态：每次发到目前为止的全部文本。
func TestTranslateCumulativeSnapshots(t *testing.T) {
	upstream := "event: output\ndata: {\"response\":\"Hel\"}\n\n" +
		"event: output\ndata: {\"response\":\"Hello\"}\n\n" +
		"event: output\ndata: {\"response\":\"Hello there\"}\n\n" +
		"event: done\ndata: {}\n\n"

	sse, err := collect(t, upstream)
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	// 累计快照不能被重复交付成 "HelHelloHello there"。
	if got := contentOf(t, sse); got != "Hello there" {
		t.Errorf("content=%q want %q (snapshot must not duplicate)", got, "Hello there")
	}
}

// TestTranslateLateOldSnapshot 覆盖迟到的旧快照：不能把已交付的文本回退。
func TestTranslateLateOldSnapshot(t *testing.T) {
	upstream := "event: output\ndata: {\"response\":\"Hello there\"}\n\n" +
		"event: output\ndata: {\"response\":\"Hello\"}\n\n" +
		"event: done\ndata: {}\n\n"

	sse, err := collect(t, upstream)
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	if got := contentOf(t, sse); got != "Hello there" {
		t.Errorf("content=%q want %q (stale snapshot must be dropped)", got, "Hello there")
	}
}

// TestTranslateEmbeddedThink 覆盖 <think> 拆分：思考内容不进正文。
func TestTranslateEmbeddedThink(t *testing.T) {
	upstream := "event: output\ndata: {\"response\":\"<think>let me reason</think>the answer\"}\n\n" +
		"event: done\ndata: {}\n\n"

	sse, err := collect(t, upstream)
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	content := contentOf(t, sse)
	if strings.Contains(content, "let me reason") {
		t.Errorf("reasoning leaked into content: %q", content)
	}
	if !strings.Contains(content, "the answer") {
		t.Errorf("visible answer missing: %q", content)
	}
	if !strings.Contains(sse, `"reasoning_content"`) || !strings.Contains(sse, "let me reason") {
		t.Errorf("reasoning not delivered as reasoning_content: %q", sse)
	}
}

// TestTranslateToolCalls 覆盖工具调用的跨帧合并：首片带 id/name，后续只带参数片段。
func TestTranslateToolCalls(t *testing.T) {
	upstream := "event: output\ndata: {\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"type\":\"function\",\"function\":{\"name\":\"read\",\"arguments\":\"{\\\"pa\"}}]}\n\n" +
		"event: output\ndata: {\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"th\\\":\\\"a.txt\\\"}\"}}]}\n\n" +
		"event: done\ndata: {\"finish_reason\":\"tool_calls\"}\n\n"

	sse, err := collect(t, upstream)
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	// 终态归一为 tool_calls（有工具调用时优先级最高）。
	if !strings.Contains(sse, `"finish_reason":"tool_calls"`) {
		t.Errorf("finish_reason should be tool_calls: %q", sse)
	}
	// id 与 name 只在首片出现一次，参数片段按序拼接。
	if n := strings.Count(sse, `"id":"call_1"`); n != 1 {
		t.Errorf("tool call id delivered %d times want 1: %q", n, sse)
	}
	if n := strings.Count(sse, `"name":"read"`); n != 1 {
		t.Errorf("tool name delivered %d times want 1: %q", n, sse)
	}
	if !strings.Contains(sse, `"arguments":"{\"pa"`) || !strings.Contains(sse, `"arguments":"th\":\"a.txt\"}"`) {
		t.Errorf("argument fragments missing: %q", sse)
	}
}

// TestTranslateReasoningField 覆盖独立 reasoning 字段。
func TestTranslateReasoningField(t *testing.T) {
	upstream := "event: output\ndata: {\"reasoning_content\":\"thinking hard\",\"response\":\"done\"}\n\n" +
		"event: done\ndata: {}\n\n"

	sse, err := collect(t, upstream)
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	if !strings.Contains(sse, "thinking hard") || !strings.Contains(sse, `"reasoning_content"`) {
		t.Errorf("reasoning_content missing: %q", sse)
	}
	if strings.Contains(contentOf(t, sse), "thinking hard") {
		t.Errorf("reasoning leaked into content: %q", sse)
	}
}

// TestTranslateHeartbeatKeepsStreamAlive 覆盖心跳事件转成 SSE 注释。
func TestTranslateHeartbeatKeepsStreamAlive(t *testing.T) {
	upstream := "event: heartbeat\ndata: {}\n\n" +
		"event: output\ndata: {\"response\":\"hi\"}\n\n" +
		"event: done\ndata: {}\n\n"

	sse, err := collect(t, upstream)
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	if !strings.Contains(sse, ": relay-keepalive") {
		t.Errorf("heartbeat was not forwarded as a comment: %q", sse)
	}
	if got := contentOf(t, sse); got != "hi" {
		t.Errorf("content=%q want hi", got)
	}
}

// TestTranslateErrorEvent 覆盖上游 error 事件：必须作为失败上报，不能伪装成功。
func TestTranslateErrorEvent(t *testing.T) {
	upstream := "event: output\ndata: {\"response\":\"partial\"}\n\n" +
		"event: error\ndata: {\"message\":\"upstream exploded\"}\n\n"

	_, err := collect(t, upstream)
	if err == nil || !strings.Contains(err.Error(), "upstream exploded") {
		t.Fatalf("error event must surface as a failure, got %v", err)
	}
}

// TestTranslateRequiresTerminal 覆盖截断：没有终态事件就必须报错。
//
// 这是最关键的一条——上游掐流时若静默收尾，客户端会把半截回答当成完整答案。
func TestTranslateRequiresTerminal(t *testing.T) {
	upstream := "event: output\ndata: {\"response\":\"half an answer\"}\n\n"

	_, err := collect(t, upstream)
	if err == nil {
		t.Fatal("stream without a terminal event must fail")
	}
	if !strings.Contains(err.Error(), "terminal") {
		t.Errorf("error should name the missing terminal event: %v", err)
	}
}

// TestTranslateDONEWithoutEventName 覆盖只有 data: [DONE] 的收尾。
func TestTranslateDONEWithoutEventName(t *testing.T) {
	upstream := "data: {\"response\":\"ok\"}\n\n" +
		"data: [DONE]\n\n"

	sse, err := collect(t, upstream)
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	if got := contentOf(t, sse); got != "ok" {
		t.Errorf("content=%q want ok", got)
	}
}

// TestTranslateEmptyUpstreamFails 覆盖零事件流。
func TestTranslateEmptyUpstreamFails(t *testing.T) {
	if _, err := collect(t, ""); err == nil {
		t.Fatal("an upstream with no data events must fail")
	}
}

// TestFinalFinishReason 锁定终态归一，含上游自造的终止值。
func TestFinalFinishReason(t *testing.T) {
	cases := []struct {
		in       string
		tools    bool
		expected string
	}{
		{"stop", false, "stop"},
		{"", false, "stop"},
		{"end_turn", false, "stop"},
		{"length", false, "length"},
		{"max_tokens", false, "length"},
		{"model_context_window_exceeded", false, "length"},
		{"content_filter", false, "content_filter"},
		{"safety", false, "content_filter"},
		// 上游自造的终止值：正文已交付，按正常结束处理（对齐网关 sse.go 的口径）。
		{"error", false, "stop"},
		{"abort", false, "stop"},
		{"failed", false, "stop"},
		// 有工具调用时终态优先。
		{"stop", true, "tool_calls"},
		{"length", true, "tool_calls"},
	}
	for _, tc := range cases {
		if got := finalFinishReason(tc.in, tc.tools); got != tc.expected {
			t.Errorf("finalFinishReason(%q, %v)=%q want %q", tc.in, tc.tools, got, tc.expected)
		}
	}
}

// TestSplitEmbeddedReasoning 覆盖 think 拆分的边界。
func TestSplitEmbeddedReasoning(t *testing.T) {
	cases := []struct {
		in            string
		wantReasoning string
		wantVisible   string
	}{
		{"plain text", "", "plain text"},
		{"<think>a</think>b", "a", "b"},
		{"<thinking>a</thinking>b", "a", "b"},
		{"pre<think>a</think>post", "a", "prepost"},
		{"<think>a</think>mid<think>b</think>end", "a\nb", "midend"},
		// 未闭合的 think：原样保留在可见文本里，不能把后面的正文吃掉。
		{"<think>unclosed text", "", "<think>unclosed text"},
	}
	for _, tc := range cases {
		reasoning, visible := splitEmbeddedReasoning(tc.in)
		if reasoning != tc.wantReasoning || visible != tc.wantVisible {
			t.Errorf("splitEmbeddedReasoning(%q)=(%q,%q) want (%q,%q)",
				tc.in, reasoning, visible, tc.wantReasoning, tc.wantVisible)
		}
	}
}

// TestVisibleTextHoldsIncompleteToolBlock 覆盖正文里半截工具块的压制。
func TestVisibleTextHoldsIncompleteToolBlock(t *testing.T) {
	if got := visibleText("answer<tool_call>half"); got != "answer" {
		t.Errorf("incomplete tool block leaked: %q", got)
	}
	if got := visibleText("answer<tool_call>x</tool_call>tail"); got != "answertail" {
		t.Errorf("complete tool block not stripped: %q", got)
	}
	// 正好停在标记前缀上：必须压住，否则客户端会看到 "<tool_c"。
	if got := visibleText("answer<tool_c"); got != "answer" {
		t.Errorf("partial tag prefix leaked: %q", got)
	}
}

// TestResolveModelName 覆盖模型名映射。
//
// Trae 上游按 config_name **精确匹配**（大小写敏感），所以宽松的客户端模型名必须
// 先映射；映射错了上游会直接拒掉请求，而客户端看到的是一个莫名其妙的参数错误。
func TestResolveModelName(t *testing.T) {
	cases := []struct{ in, want string }{
		{"glm-5.3", "glm-5.3"},
		{"GLM-5.3", "glm-5.3"},        // 查表按小写归一
		{"gpt-4o", "DeepSeek-V4-Pro"}, // 外部名映射到 Trae 真实名
		{"claude-sonnet-4-5", "glm-5.2"},
		{"deepseek-v4-pro", "DeepSeek-V4-Pro"}, // 真实名的大小写由表决定
		{"  glm-5.3  ", "glm-5.3"},             // 去空白
		// 未知名原样透传：上游的报错比网关自己猜一个替代模型更可用。
		{"brand-new-model", "brand-new-model"},
		{"", ""},
	}
	for _, tc := range cases {
		if got := ResolveModelName(tc.in); got != tc.want {
			t.Errorf("ResolveModelName(%q)=%q want %q", tc.in, got, tc.want)
		}
	}
}

// TestBuildChatBodyAppliesModelAlias 确认映射真的作用到了出站请求体。
func TestBuildChatBodyAppliesModelAlias(t *testing.T) {
	prepared := []byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`)
	body, err := BuildChatBody(prepared, "req-1")
	if err != nil {
		t.Fatalf("BuildChatBody: %v", err)
	}
	var parsed map[string]any
	if err := json.Unmarshal(body, &parsed); err != nil {
		t.Fatalf("body is not JSON: %v", err)
	}
	if parsed["config_name"] != "DeepSeek-V4-Pro" {
		t.Errorf("config_name=%v want DeepSeek-V4-Pro", parsed["config_name"])
	}
	if parsed["model"] != "DeepSeek-V4-Pro" {
		t.Errorf("model=%v want DeepSeek-V4-Pro", parsed["model"])
	}
	if parsed["function"] != "solo_work_lite" {
		t.Errorf("function=%v want solo_work_lite", parsed["function"])
	}
	if parsed["stream"] != true {
		t.Errorf("stream=%v want true", parsed["stream"])
	}
}

// TestModelIDsSortedAndComplete 确认目录公布的名字集合自洽（别名与真实名都在）。
func TestModelIDsSortedAndComplete(t *testing.T) {
	ids := ModelIDs()
	if len(ids) == 0 {
		t.Fatal("model catalog is empty")
	}
	for i := 1; i < len(ids); i++ {
		if ids[i-1] > ids[i] {
			t.Fatalf("ModelIDs is not sorted: %q > %q", ids[i-1], ids[i])
		}
	}
	seen := map[string]bool{}
	for _, id := range ids {
		if seen[id] {
			t.Errorf("duplicate id %q", id)
		}
		seen[id] = true
	}
	for _, want := range []string{"glm-5.3", "gpt-4o", "DeepSeek-V4-Pro"} {
		if !seen[want] {
			t.Errorf("catalog missing %q", want)
		}
	}
	// 每个别名映射的目标都必须也在清单里，否则客户端按真实名请求会 404。
	for key, value := range ModelAliases() {
		if !seen[value] {
			t.Errorf("alias %q -> %q but %q is not in the published catalog", key, value, value)
		}
	}
}

// TestNativeMessageContentIsBlockArray 锁定 Trae 要求的内容块数组形态。
//
// 2026-10-11 真实上游实测：直接透传 OpenAI 消息被拒——
//
//	{"code":4001,"message":"bad request: json: cannot unmarshal string into Go struct
//	 field LLMRawMessage.messages.content of type []*idecopilot.LLMRawMessageContent"}
//
// 也就是说 content 必须是数组。这条测试钉住它，避免以后有人"顺手简化"回字符串。
func TestNativeMessageContentIsBlockArray(t *testing.T) {
	msg := nativeMessage(map[string]any{"role": "user", "content": "hello"})
	blocks, ok := msg["content"].([]any)
	if !ok {
		t.Fatalf("content must be an array, got %T: %v", msg["content"], msg["content"])
	}
	if len(blocks) != 1 {
		t.Fatalf("blocks=%v want one", blocks)
	}
	block, _ := blocks[0].(map[string]any)
	if block["type"] != "text" || block["text"] != "hello" {
		t.Errorf("block=%v want {type:text text:hello}", block)
	}
}

func TestNativeMessageRoleNormalization(t *testing.T) {
	cases := map[string]string{
		"user":      "user",
		"USER":      "user",
		"assistant": "assistant",
		"system":    "system",
		"developer": "system", // developer 是 system 的新名字
		"tool":      "tool",
		"weird":     "user", // 未知角色回落 user
	}
	for in, want := range cases {
		got, _ := nativeMessage(map[string]any{"role": in, "content": "x"})["role"].(string)
		if got != want {
			t.Errorf("role %q -> %q want %q", in, got, want)
		}
	}
}

// TestNativeAssistantToolCallsShape 锁定助手工具调用转成上游的 function_call 形状。
func TestNativeAssistantToolCallsShape(t *testing.T) {
	msg := nativeMessage(map[string]any{
		"role":    "assistant",
		"content": "",
		"tool_calls": []any{map[string]any{
			"id":   "call_1",
			"type": "function",
			"function": map[string]any{
				"name":      "read",
				"arguments": `{"path":"a.txt"}`,
			},
		}},
	})
	calls, _ := msg["tool_calls"].([]map[string]any)
	if len(calls) != 1 {
		t.Fatalf("tool_calls=%v want one", msg["tool_calls"])
	}
	call := calls[0]
	if call["id"] != "call_1" || call["type"] != "function" {
		t.Errorf("call identity wrong: %v", call)
	}
	// 关键：上游要 function_call，不是 function。
	fn, ok := call["function_call"].(map[string]any)
	if !ok {
		t.Fatalf("must carry function_call, got keys %v", call)
	}
	if fn["name"] != "read" || fn["arguments"] != `{"path":"a.txt"}` {
		t.Errorf("function_call=%v", fn)
	}
}

// TestNativeToolResultKeepsCorrelation 锁定 tool 消息保留关联字段。
func TestNativeToolResultKeepsCorrelation(t *testing.T) {
	msg := nativeMessage(map[string]any{
		"role": "tool", "tool_call_id": "call_1", "name": "read", "content": "file body",
	})
	if msg["tool_call_id"] != "call_1" || msg["name"] != "read" {
		t.Errorf("tool correlation lost: %v", msg)
	}
	if _, ok := msg["content"].([]any); !ok {
		t.Errorf("tool content must also be a block array: %v", msg["content"])
	}
}

// TestNativeContentAcceptsBlockArray 覆盖已是块数组的输入（Anthropic 风格）。
func TestNativeContentAcceptsBlockArray(t *testing.T) {
	msg := nativeMessage(map[string]any{
		"role": "user",
		"content": []any{
			map[string]any{"type": "text", "text": "hi"},
			map[string]any{"type": "input_text", "content": "there"},
		},
	})
	blocks, _ := msg["content"].([]any)
	if len(blocks) != 2 {
		t.Fatalf("blocks=%v", blocks)
	}
	first, _ := blocks[0].(map[string]any)
	second, _ := blocks[1].(map[string]any)
	if first["type"] != "text" || first["text"] != "hi" {
		t.Errorf("block0=%v", first)
	}
	// input_text 归一为 text，文本从 content 提到 text。
	if second["type"] != "text" || second["text"] != "there" {
		t.Errorf("block1=%v", second)
	}
}

// TestBuildChatBodyMessagesAreNative 端到端确认出站体里的 content 是数组。
func TestBuildChatBodyMessagesAreNative(t *testing.T) {
	prepared := []byte(`{"model":"glm-5.3","messages":[
		{"role":"system","content":"be brief"},
		{"role":"user","content":"hi"}]}`)
	body, err := BuildChatBody(prepared, "req-1")
	if err != nil {
		t.Fatalf("BuildChatBody: %v", err)
	}
	var parsed map[string]any
	if err := json.Unmarshal(body, &parsed); err != nil {
		t.Fatalf("not JSON: %v", err)
	}
	msgs, _ := parsed["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("messages=%v", parsed["messages"])
	}
	for i, raw := range msgs {
		m, _ := raw.(map[string]any)
		blocks, ok := m["content"].([]any)
		if !ok {
			t.Fatalf("message %d content is %T, must be an array (upstream rejects strings)",
				i, m["content"])
		}
		if len(blocks) == 0 {
			t.Fatalf("message %d has empty content array", i)
		}
	}
}

// TestTranslateNativeFunctionCallShape 锁定 2026-10-11 真实上游的工具调用形态。
//
// 原始事件实测（read_file 调用）：
//
//	{"index":0,"id":"call_97917b0d...","type":"function",
//	 "function_call":{"name":"read_file","arguments":"{\""}}
//	{"index":0,"id":"","type":"function",
//	 "function_call":{"name":"","arguments":"path\":\"/etc/hostname\"}"}}
//
// 三个必须同时成立的点，缺一个工具调用就废：
//  1. name/arguments 在 **function_call** 里（不是 OpenAI 的 function）——
//     只认 function 会让名字与参数全丢，下游报 invalid tool arguments；
//  2. 身份与参数**拆在两个事件**里，第二帧的 id/name 是空串，不能覆盖第一帧；
//  3. arguments 是**增量拼接**（"{\"" + "path\":...}"），不是累计快照。
func TestTranslateNativeFunctionCallShape(t *testing.T) {
	upstream := "event: output\ndata: {\"response\":\"\",\"reasoning_content\":null," +
		"\"tool_calls\":[{\"index\":0,\"id\":\"call_abc\",\"type\":\"function\"," +
		"\"function_call\":{\"name\":\"read_file\",\"arguments\":\"{\\\"\"}}]}\n\n" +
		"event: output\ndata: {\"response\":\"\",\"reasoning_content\":null," +
		"\"tool_calls\":[{\"index\":0,\"id\":\"\",\"type\":\"function\"," +
		"\"function_call\":{\"name\":\"\",\"arguments\":\"path\\\":\\\"/etc/hostname\\\"}\"}}]}\n\n" +
		"event: done\ndata: {\"finish_reason\":\"tool_calls\"}\n\n"

	sse, err := collect(t, upstream)
	if err != nil {
		t.Fatalf("translate: %v", err)
	}

	// 首片必须带 id/name/type 各一次。
	if n := strings.Count(sse, `"id":"call_abc"`); n != 1 {
		t.Errorf("tool id delivered %d times want 1: %q", n, sse)
	}
	if n := strings.Count(sse, `"name":"read_file"`); n != 1 {
		t.Errorf("tool name delivered %d times want 1: %q", n, sse)
	}
	if n := strings.Count(sse, `"type":"function"`); n != 1 {
		t.Errorf("tool type delivered %d times want 1: %q", n, sse)
	}
	// 参数按增量交付（首片 "{\""，次片 "path\":...}"）。
	if !strings.Contains(sse, `"arguments":"{\""`) {
		t.Errorf("first argument fragment missing: %q", sse)
	}
	if !strings.Contains(sse, `path\":\"/etc/hostname\"}`) {
		t.Errorf("second argument fragment missing: %q", sse)
	}
	// 终态归一为 tool_calls。
	if !strings.Contains(sse, `"finish_reason":"tool_calls"`) {
		t.Errorf("finish_reason should be tool_calls: %q", sse)
	}
	// 空 id/name 的后续帧不得再交付身份字段。
	if strings.Contains(sse, `"name":""`) || strings.Contains(sse, `"id":""`) {
		t.Errorf("empty identity fields leaked into the stream: %q", sse)
	}
}

// TestTranslateNativeToolUsageMapping 锁定上游 usage 字段名（token_usage 事件）。
//
// 真实事件：{"prompt_tokens":189,"completion_tokens":43,"total_tokens":232,
//
//	"cache_read_input_tokens":128,"reasoning_tokens":29}
//
// 网关下游按 prompt_tokens_details.cached_tokens / completion_tokens_details.
// reasoning_tokens 读缓存与思考量，不归一就会静默丢字段。
func TestTranslateNativeToolUsageMapping(t *testing.T) {
	upstream := "event: output\ndata: {\"response\":\"hi\"}\n\n" +
		"event: token_usage\ndata: {\"prompt_tokens\":189,\"completion_tokens\":43," +
		"\"total_tokens\":232,\"cache_read_input_tokens\":128,\"reasoning_tokens\":29}\n\n" +
		"event: done\ndata: {}\n\n"

	sse, err := collect(t, upstream)
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	for _, want := range []string{
		`"prompt_tokens":189`,
		`"completion_tokens":43`,
		`"total_tokens":232`,
		`"cached_tokens":128`,
		`"reasoning_tokens":29`,
	} {
		if !strings.Contains(sse, want) {
			t.Errorf("usage field %s missing from %q", want, sse)
		}
	}
}
