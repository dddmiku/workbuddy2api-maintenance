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
