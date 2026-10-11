package upstream

import (
	"io"
	"net/http/httptest"
	"strings"
	"testing"

	"workbuddy2api/internal/upstream/trae"
)

// 本文件是 Trae 域（internal/upstream/trae）与网关上游解析器的**契约测试**。
//
// 放在 upstream 包而不是 trae 包里，是因为 upstream 已经 import trae（出站分流），
// 反向 import 会成环。这里验证的是移植最容易翻车的地方：Trae 转换器写的帧，
// 网关自己的 Stream / Aggregate 必须真能消费——单测只证明"帧是我们想发的形状"。

// traeSSE 用真实上游事件族驱动转换器，返回下游会看到的 OpenAI SSE。
func traeSSE(t *testing.T, upstreamEvents string) string {
	t.Helper()
	r := trae.TranslateStream(io.NopCloser(strings.NewReader(upstreamEvents)), "glm-5.3")
	defer r.Close()
	var b strings.Builder
	buf := make([]byte, 4096)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			b.Write(buf[:n])
		}
		if err != nil {
			break
		}
	}
	return b.String()
}

// TestTraeTranslatedStreamIsConsumableByGateway 覆盖「正文 + 工具调用 + usage」回合。
func TestTraeTranslatedStreamIsConsumableByGateway(t *testing.T) {
	sse := traeSSE(t, "event: output\ndata: {\"response\":\"The answer\"}\n\n"+
		"event: output\ndata: {\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"type\":\"function\","+
		"\"function\":{\"name\":\"read\",\"arguments\":\"{\\\"path\\\":\\\"a.txt\\\"}\"}}]}\n\n"+
		"event: token_usage\ndata: {\"input_token\":120,\"output_token\":8}\n\n"+
		"event: done\ndata: {\"finish_reason\":\"tool_calls\"}\n\n")

	// 1) 流式路径：网关必须零错误地透传。
	rec := httptest.NewRecorder()
	if err := Stream(rec, strings.NewReader(sse)); err != nil {
		t.Fatalf("gateway Stream rejected the translated frames: %v", err)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "The answer") {
		t.Errorf("content lost through the gateway parser: %q", body)
	}
	if !strings.Contains(body, `"finish_reason":"tool_calls"`) {
		t.Errorf("terminal reason lost: %q", body)
	}

	// 2) 聚合路径：必须合成一个完整的非流式响应。
	chat, err := Aggregate(strings.NewReader(sse))
	if err != nil {
		t.Fatalf("gateway Aggregate rejected the translated frames: %v", err)
	}
	choices, _ := chat["choices"].([]any)
	if len(choices) != 1 {
		t.Fatalf("choices=%v want exactly one", chat["choices"])
	}
	choice, _ := choices[0].(map[string]any)
	message, _ := choice["message"].(map[string]any)
	if message["content"] != "The answer" {
		t.Errorf("aggregated content=%v want %q", message["content"], "The answer")
	}
	// Aggregate 内部按具体类型构造（[]map[string]any），不是 JSON 解码来的 []any。
	calls, _ := message["tool_calls"].([]map[string]any)
	if len(calls) != 1 {
		t.Fatalf("aggregated tool_calls=%v want one call", message["tool_calls"])
	}
	call := calls[0]
	fn, _ := call["function"].(map[string]any)
	if fn["name"] != "read" {
		t.Errorf("tool name=%v want read", fn["name"])
	}
	if fn["arguments"] != `{"path":"a.txt"}` {
		t.Errorf("tool arguments=%v want the full JSON object", fn["arguments"])
	}
	if choice["finish_reason"] != "tool_calls" {
		t.Errorf("finish_reason=%v want tool_calls", choice["finish_reason"])
	}
	usage, _ := chat["usage"].(map[string]any)
	if usage == nil {
		t.Fatalf("usage lost: %v", chat["usage"])
	}
	if got := usage["prompt_tokens"]; got != float64(120) {
		t.Errorf("usage.prompt_tokens=%v want 120", got)
	}
}

// TestTraeTranslatedTextStreamIsConsumableByGateway 覆盖纯文本回合（含累计快照合并）。
func TestTraeTranslatedTextStreamIsConsumableByGateway(t *testing.T) {
	sse := traeSSE(t, "event: output\ndata: {\"response\":\"Hello\"}\n\n"+
		"event: output\ndata: {\"response\":\"Hello, world\"}\n\n"+
		"event: done\ndata: {\"finish_reason\":\"stop\"}\n\n")

	chat, err := Aggregate(strings.NewReader(sse))
	if err != nil {
		t.Fatalf("gateway Aggregate rejected the translated frames: %v", err)
	}
	choices, _ := chat["choices"].([]any)
	choice, _ := choices[0].(map[string]any)
	message, _ := choice["message"].(map[string]any)
	if message["content"] != "Hello, world" {
		t.Errorf("content=%v want %q (snapshot must not duplicate)", message["content"], "Hello, world")
	}
	if choice["finish_reason"] != "stop" {
		t.Errorf("finish_reason=%v want stop", choice["finish_reason"])
	}
}

// TestTraeTranslatedTruncatedStreamIsRejected 确认截断不被下游当成成功。
//
// 这是转换器最关键的一条约束：上游没给终态时必须报错，否则半截回答会以
// finish_reason=stop 的形态伪装成完整答案流到客户端。
func TestTraeTranslatedTruncatedStreamIsRejected(t *testing.T) {
	r := trae.TranslateStream(
		io.NopCloser(strings.NewReader("event: output\ndata: {\"response\":\"half\"}\n\n")), "glm-5.3")
	defer r.Close()
	buf := make([]byte, 4096)
	var err error
	for {
		if _, err = r.Read(buf); err != nil {
			break
		}
	}
	if err == nil {
		t.Fatal("a truncated Trae stream must surface an error, not EOF")
	}
	if !strings.Contains(err.Error(), "terminal") {
		t.Errorf("error should name the missing terminal event: %v", err)
	}
}
