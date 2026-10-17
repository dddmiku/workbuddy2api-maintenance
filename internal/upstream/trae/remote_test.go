// ═══ 更新日志 ═══
// 2026-10-15：初版——Remote 会话端点回落（工具轮次超限 3004）的回归。
// 2026-10-16：Remote 升为主路径后的重写——扁平化（prompt.go）、压缩链、
//             custom_model 绑定、max 六件套、事件转译（content 优先 +
//             finish summary 兜底）、首事件超时。
//
// 本文件锁定的核心实测结论（2026-10-15，1047 条真实会话，同一账号交替对照）：
//
//	最近 100 条 / 293KB → llm_utils_chat 3004   | Remote 成功（正文 794 字符）
//	最近 120 条 / 311KB → llm_utils_chat 3004   | Remote 成功（正文 627 字符）
//	最近 150 条 / 332KB → llm_utils_chat 3004   | Remote 成功（正文 588 字符）
//	最近 200 条 / 396KB → llm_utils_chat 3004   | Remote 成功（正文 310 字符）
//
// 触发维度是**工具交互轮次**，不是体积：
//
//	2 条 / 1600KB 纯文本          → 成功（体积不是条件）
//	300 条 / 336KB 合成纯文本      → 成功（条数本身不是条件）
//	120 条真实，去掉 tools 定义     → 3004
//	120 条真实，只丢 tool_calls     → 3004
//	120 条真实，只丢 tool_call_id   → 3004
//	120 条真实，两者都转纯文本       → 成功  ← 唯一通过的变体
package trae

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"workbuddy2api/internal/upstreamerr"
)

// remoteStub 假 Remote 上游：两步协议（建会话 + 收流）都用 httptest 接管。
type remoteStub struct {
	createBody  string
	eventsBody  string
	createCalls int
	eventsCalls int
	lastCreate  map[string]any
}

func (s *remoteStub) start(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/chat_sessions") && r.Method == http.MethodPost:
			s.createCalls++
			raw, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(raw, &s.lastCreate)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(s.createBody))
		case strings.Contains(r.URL.Path, "/events"):
			s.eventsCalls++
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte(s.eventsBody))
		default:
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("{}"))
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestRemoteQueryFlattensToolCallsToText 扁平化后**不得**残留 tool_calls / tool_call_id 字段。
//
// 这是绕开 3004 的关键：实测「只丢 tool_calls」或「只丢 tool_call_id」都仍然被拒，
// 只有两者都变成纯文本才通过。
func TestRemoteQueryFlattensToolCallsToText(t *testing.T) {
	messages := []map[string]any{
		{"role": "system", "content": []any{map[string]any{"type": "text", "text": "You are helpful."}}},
		{"role": "user", "content": []any{map[string]any{"type": "text", "text": "read the file"}}},
		{"role": "assistant", "content": []any{map[string]any{"type": "text", "text": ""}},
			"tool_calls": []any{map[string]any{
				"id": "call_abc", "type": "function",
				"function": map[string]any{"name": "read_file", "arguments": `{"path":"/etc/hostname"}`},
			}}},
		{"role": "tool", "tool_call_id": "call_abc",
			"content": []any{map[string]any{"type": "text", "text": "myhost"}}},
	}
	query := flattenQuery(messages)

	// query 是一个 JSON 字符串数组，解出来后取内容。
	var parts []map[string]any
	if err := json.Unmarshal([]byte(query), &parts); err != nil {
		t.Fatalf("query 应是合法 JSON：%v", err)
	}
	if len(parts) != 1 {
		t.Fatalf("query 应是单元素数组，实际 %d", len(parts))
	}
	data, _ := parts[0]["data"].(map[string]any)
	text, _ := data["content"].(string)

	for _, want := range []string{"[System]", "[Assistant]", "[Client Tool Result: call_abc",
		"read_file", "/etc/hostname", "myhost", "call_abc"} {
		if !strings.Contains(text, want) {
			t.Errorf("扁平化文本应含 %q，实际：%s", want, text)
		}
	}
	// 关键：结构化字段名不得出现（否则上游按工具轮次计数，仍会 3004）。
	for _, forbidden := range []string{`"tool_calls"`, `"tool_call_id"`} {
		if strings.Contains(query, forbidden) {
			t.Errorf("query 里不得残留 %s（上游据此计数并拒绝）：%s", forbidden, query)
		}
	}
}

// TestFlattenQueryCamelCaseToolResultId 兼容 toolCallId 拼写的工具结果。
func TestFlattenQueryCamelCaseToolResultId(t *testing.T) {
	messages := []map[string]any{
		{"role": "tool", "toolCallId": "cc_1", "name": "ls", "content": "file"},
	}
	query := flattenQuery(messages)
	if !strings.Contains(query, "[Client Tool Result: cc_1 ls status=succeeded]") {
		t.Errorf("toolCallId 应被识别：%s", query)
	}
}

// TestFlattenQueryFailedToolResult 失败工具结果的 status 标记。
func TestFlattenQueryFailedToolResult(t *testing.T) {
	messages := []map[string]any{
		{"role": "tool", "tool_call_id": "f1", "name": "write", "content": "denied", "is_error": true},
	}
	if !strings.Contains(flattenQuery(messages), "status=failed") {
		t.Error("is_error 的工具结果应标记 status=failed")
	}
	messages[0]["is_error"] = false
	messages[0]["status"] = "timeout"
	if !strings.Contains(flattenQuery(messages), "status=failed") {
		t.Error("status=timeout 的工具结果应标记 status=failed")
	}
}

// TestCompactRemoteHistoryKeepsSystemAndBounds 压缩保留全部 system（去重）与最新消息。
func TestCompactRemoteHistoryKeepsSystemAndBounds(t *testing.T) {
	filler := strings.Repeat("x", 2000)
	messages := []map[string]any{
		{"role": "system", "content": "sys-a"},
		{"role": "system", "content": "sys-a"}, // 重复，应去重
		{"role": "system", "content": "sys-b"},
	}
	for i := 0; i < 20; i++ {
		role := "user"
		if i%2 == 1 {
			role = "assistant"
		}
		messages = append(messages, map[string]any{"role": role, "content": filler})
	}
	kept := compactRemoteHistory(messages, 5000, 20000)
	if len(kept) == 0 {
		t.Fatal("不该裁到空")
	}
	// system 去重后都保留且在前。
	var systems int
	for i, m := range kept {
		if role, _ := m["role"].(string); role == "system" {
			systems++
			if i > 2 {
				t.Errorf("system 应排在最前，实际第 %d 位是 system", i)
			}
		}
	}
	if systems != 2 {
		t.Errorf("重复 system 应去重（保留 2 条），实际 %d", systems)
	}
	// 保留的应是**最新**的：最后一条必须与原文最后一条相同。
	last, _ := kept[len(kept)-1]["content"].(string)
	orig, _ := messages[len(messages)-1]["content"].(string)
	if last != orig {
		t.Error("应保留最新消息（从最早的开始丢）")
	}
}

// TestBoundedRemoteQueryDropsToolPairsTogether 裁剪时成对丢（assistant(tool_calls) + 紧随的 tool）。
//
// 只丢一半会留下孤立的工具结果，上游对「调用与结果不配对」是直接报错的。
func TestBoundedRemoteQueryDropsToolPairsTogether(t *testing.T) {
	big := strings.Repeat("y", 3000)
	messages := []map[string]any{
		{"role": "system", "content": "sys"},
		{"role": "user", "content": big},
		{"role": "assistant", "content": "", "tool_calls": []any{map[string]any{
			"id": "c1", "function": map[string]any{"name": "f", "arguments": "{}"}}}},
		{"role": "tool", "tool_call_id": "c1", "content": big},
		{"role": "assistant", "content": big, "tool_calls": []any{map[string]any{
			"id": "c2", "function": map[string]any{"name": "f", "arguments": "{}"}}}},
		{"role": "tool", "tool_call_id": "c2", "content": big},
		{"role": "user", "content": "latest question"},
	}
	kept, _ := boundedRemoteQuery(messages, 4000)
	// 不得出现「tool 消息没有紧邻的 assistant(tool_calls)」这种孤立形态。
	for i, m := range kept {
		if role, _ := m["role"].(string); role != "tool" {
			continue
		}
		if i == 0 {
			t.Fatalf("首条不该是孤立的 tool 消息：%+v", kept)
		}
		prev := kept[i-1]
		if prevRole, _ := prev["role"].(string); prevRole != "assistant" {
			t.Errorf("tool 消息前应是 assistant，实际 %v", prevRole)
		}
		if calls, ok := prev["tool_calls"].([]any); !ok || len(calls) == 0 {
			t.Errorf("tool 消息前应是带 tool_calls 的 assistant：%+v", prev)
		}
	}
}

// TestBuildRemoteBodyShape 建会话请求体的形态（字段名与枚举值）。
func TestBuildRemoteBodyShape(t *testing.T) {
	prepared, _ := json.Marshal(map[string]any{
		"model":    "auto",
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	})
	body, agentType, maxApplied, err := buildRemoteBody(context.Background(), testAuth(), prepared)
	if err != nil {
		t.Fatalf("构造失败：%v", err)
	}
	if agentType != remoteDefaultAgentType {
		t.Errorf("纯文本请求应走 %s，实际 %s", remoteDefaultAgentType, agentType)
	}
	if maxApplied {
		t.Error("auto 策略不该套 max 六件套")
	}
	var parsed map[string]any
	if err := json.Unmarshal(body, &parsed); err != nil {
		t.Fatalf("body 应是合法 JSON：%v", err)
	}
	if parsed["mode"] != "code" || parsed["env"] != "remote" || parsed["origin"] != "web" {
		t.Errorf("顶层字段错误：%+v", parsed)
	}
	if parsed["environment_id"] != "default" {
		t.Errorf("environment_id 应为 default：%v", parsed["environment_id"])
	}
	init, _ := parsed["initial_message"].(map[string]any)
	if init == nil {
		t.Fatal("缺 initial_message")
	}
	if init["agent_type"] != "solo_agent_remote" || init["agent_id"] != "solo_agent_remote" {
		t.Errorf("agent_type/agent_id 应为 solo_agent_remote：%+v", init)
	}
	if init["model_selection_strategy"] != "auto" {
		t.Errorf("model_selection_strategy 应为 auto：%v", init["model_selection_strategy"])
	}
	if init["model_name"] != "" {
		t.Errorf("auto 策略的 model_name 应为空：%v", init["model_name"])
	}
	if init["chat_session_id"] != "" {
		t.Errorf("chat_session_id 应为空串（服务端回填）：%v", init["chat_session_id"])
	}
	common, _ := init["common_params"].(string)
	for _, want := range []string{"marscode-cn", "solo_chat_mode"} {
		if !strings.Contains(common, want) {
			t.Errorf("common_params 应含 %q：%s", want, common)
		}
	}
}

// TestBuildRemoteBodyRoutesWorkForTools 带客户端工具的请求路由到 solo_work_remote。
//
// 对齐参考 caller_tools_use_work 默认（=1）：Agent 档的沙箱会自己动文件/终端，
// Work 档把客户端声明的工具保持为调用（opencode_tool_call 文本协议）。
func TestBuildRemoteBodyRoutesWorkForTools(t *testing.T) {
	prepared, _ := json.Marshal(map[string]any{
		"model": "auto",
		"tools": []any{map[string]any{
			"type": "function",
			"function": map[string]any{
				"name": "read_file", "description": "read",
				"parameters": map[string]any{"type": "object"},
			},
		}},
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	})
	_, agentType, _, err := buildRemoteBody(context.Background(), testAuth(), prepared)
	if err != nil {
		t.Fatalf("构造失败：%v", err)
	}
	if agentType != remoteWorkAgentType {
		t.Errorf("带工具的请求应路由到 %s，实际 %s", remoteWorkAgentType, agentType)
	}
}

// TestBuildRemoteBodyWorkModelName work 模型名显式要求 Work 档。
func TestBuildRemoteBodyWorkModelName(t *testing.T) {
	prepared, _ := json.Marshal(map[string]any{
		"model":    "work",
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	})
	body, agentType, _, err := buildRemoteBody(context.Background(), testAuth(), prepared)
	if err != nil {
		t.Fatalf("构造失败：%v", err)
	}
	if agentType != remoteWorkAgentType {
		t.Errorf("work 应走 %s，实际 %s", remoteWorkAgentType, agentType)
	}
	var parsed map[string]any
	_ = json.Unmarshal(body, &parsed)
	init, _ := parsed["initial_message"].(map[string]any)
	if init["agent_type"] != remoteWorkAgentType {
		t.Errorf("body 里的 agent_type 应为 %s：%+v", remoteWorkAgentType, init)
	}
	// work 是路由 id，model_name 应为空（strategy 仍是 auto）。
	if init["model_name"] != "" {
		t.Errorf("work 模型名的 model_name 应为空：%v", init["model_name"])
	}
}

// TestBuildRemoteBodyInjectsRuntimePrompt 带工具时注入 opencode_tool_call 运行时提示。
func TestBuildRemoteBodyInjectsRuntimePrompt(t *testing.T) {
	prepared, _ := json.Marshal(map[string]any{
		"model": "auto",
		"tools": []any{map[string]any{
			"function": map[string]any{"name": "list_dir", "parameters": map[string]any{"type": "object"}},
		}},
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	})
	body, _, _, err := buildRemoteBody(context.Background(), testAuth(), prepared)
	if err != nil {
		t.Fatalf("构造失败：%v", err)
	}
	if !strings.Contains(string(body), "opencode_tool_call") {
		t.Error("query 应含注入的 opencode_tool_call 协议提示")
	}
	if !strings.Contains(string(body), "list_dir") {
		t.Error("query 应含工具定义")
	}
}

// TestBuildRemoteBodyInjectsStyleInstruction 无工具时注入简洁答案指令。
func TestBuildRemoteBodyInjectsStyleInstruction(t *testing.T) {
	prepared, _ := json.Marshal(map[string]any{
		"model":    "auto",
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	})
	body, _, _, err := buildRemoteBody(context.Background(), testAuth(), prepared)
	if err != nil {
		t.Fatalf("构造失败：%v", err)
	}
	if !strings.Contains(string(body), "Response format") {
		t.Error("无工具时也应注入 response style 指令（防推理轨迹当答案）")
	}
}

// TestRemoteHeadersAreWebShaped Remote 头族必须是网页端形态。
//
// 与 llm_utils_chat 的 SOLO/IDE 指纹头**不能混用**：混用会得到 401/404。
func TestRemoteHeadersAreWebShaped(t *testing.T) {
	a := testAuth()
	h := remoteHeaders(a, true)
	checks := map[string]string{
		"Authorization":         "Cloud-IDE-JWT " + a.AccessToken,
		"X-Trae-Client-Type":   "web",
		"Origin":                "https://solo.trae.cn",
		"Referer":               "https://solo.trae.cn/",
		"x-user-region":        "CN",
		"X-Preferenced-Language": "zh-CN",
	}
	for key, want := range checks {
		if got := h.Get(key); got != want {
			t.Errorf("头 %s=%q，应为 %q", key, got, want)
		}
	}
	if ua := h.Get("User-Agent"); !strings.Contains(ua, "Mozilla/5.0") {
		t.Errorf("User-Agent 应是浏览器形态，实际 %q", ua)
	}
	if accept := h.Get("Accept"); accept != "text/event-stream" {
		t.Errorf("流式 Accept 应是 text/event-stream，实际 %q", accept)
	}
	// 不得混入 llm_utils_chat 的指纹头。
	for _, forbidden := range []string{"x-ide-version", "x-app-id", "x-uid", "x-ide-token"} {
		if h.Get(forbidden) != "" {
			t.Errorf("Remote 头不该含 %s（那是 llm_utils_chat 的指纹头）", forbidden)
		}
	}
}

// TestTranslateRemoteEventsContentOverThought plan_item 的显式 content 优先于 thought。
//
// 参考实现的修正：content 才是公开答案快照，thought 是推理轨迹（旧版把 thought
// 当正文，会把「The user wants…」式叙述当答案交付）。
func TestTranslateRemoteEventsContentOverThought(t *testing.T) {
	upstream := strings.Join([]string{
		"event: status_changed\ndata: {\"new_status\":3}\n\n",
		"event: metadata\ndata: {\"message_id\":\"m1\"}\n\n",
		"event: plan_item\ndata: {\"thought\":\"The user wants a ping. Let me just answer.\",\"content\":\"PONG\",\"reasoning_content\":\"\"}\n\n",
		"event: token_usage\ndata: {\"prompt_tokens\":10,\"completion_tokens\":4,\"total_tokens\":14}\n\n",
		"event: done\ndata: {\"status\":\"completed\"}\n\n",
	}, "")
	out, err := io.ReadAll(translateRemoteEvents(io.NopCloser(strings.NewReader(upstream)), "glm-5.3", ""))
	if err != nil {
		t.Fatalf("转译失败：%v", err)
	}
	text := string(out)
	if !strings.Contains(text, "event: output") {
		t.Fatalf("应产出 output 事件：%s", text)
	}
	if !strings.Contains(text, "PONG") {
		t.Errorf("应含正文 PONG：%s", text)
	}
	if strings.Contains(text, "The user wants") {
		t.Errorf("thought 叙述不该当正文交付：%s", text)
	}
	if !strings.Contains(text, "event: token_usage") {
		t.Errorf("应透传 token_usage：%s", text)
	}
	if !strings.Contains(text, "event: done") {
		t.Errorf("应产出 done：%s", text)
	}
	// 不携带正文的事件不该出现（否则 translator 会做无谓处理）。
	for _, noise := range []string{"status_changed", "metadata"} {
		if strings.Contains(text, noise) {
			t.Errorf("不携带正文的 %s 应被丢弃：%s", noise, text)
		}
	}
}

// TestTranslateRemoteEventsThoughtFallback 无 content 的旧布局回落 thought。
func TestTranslateRemoteEventsThoughtFallback(t *testing.T) {
	upstream := "event: plan_item\ndata: {\"thought\":\"pong\"}\n\n" +
		"event: done\ndata: {\"status\":\"completed\"}\n\n"
	out, err := io.ReadAll(translateRemoteEvents(io.NopCloser(strings.NewReader(upstream)), "glm-5.3", ""))
	if err != nil {
		t.Fatalf("转译失败：%v", err)
	}
	if !strings.Contains(string(out), "pong") {
		t.Errorf("旧布局（无 content）应回落 thought 当正文：%s", out)
	}
}

// TestTranslateRemoteEventsFinishSummary finish 工具的 summary 兜底为正文。
//
// 纯工具轮次的会话里，公开答案只在 finish 的 summary 参数里。
func TestTranslateRemoteEventsFinishSummary(t *testing.T) {
	upstream := strings.Join([]string{
		"event: plan_item\ndata: {\"tool_call_info\":{\"id\":\"p1\",\"name\":\"LS\",\"params\":{\"path\":\"/w\"}}}\n\n",
		"event: plan_item\ndata: {\"tool_call_info\":{\"id\":\"f1\",\"name\":\"finish\",\"params\":{\"summary\":\"已列出 3 个文件\"}}}\n\n",
		"event: done\ndata: {\"status\":\"completed\"}\n\n",
	}, "")
	out, err := io.ReadAll(translateRemoteEvents(io.NopCloser(strings.NewReader(upstream)), "glm-5.3", ""))
	if err != nil {
		t.Fatalf("转译失败：%v", err)
	}
	if !strings.Contains(string(out), "已列出 3 个文件") {
		t.Errorf("finish summary 应兜底为正文：%s", out)
	}
}

// TestTranslateRemoteEventsMessageEvent 显式 message 事件转成 output 正文。
func TestTranslateRemoteEventsMessageEvent(t *testing.T) {
	upstream := strings.Join([]string{
		"event: message\ndata: {\"content\":\"hello from message event\"}\n\n",
		"event: done\ndata: {\"status\":\"completed\"}\n\n",
	}, "")
	out, err := io.ReadAll(translateRemoteEvents(io.NopCloser(strings.NewReader(upstream)), "glm-5.3", ""))
	if err != nil {
		t.Fatalf("转译失败：%v", err)
	}
	if !strings.Contains(string(out), "hello from message event") {
		t.Errorf("message 事件的正文应转成 output：%s", out)
	}
}

// TestTranslateRemoteEventsUsageNesting Remote 的 usage 是平铺的，要包成 usage 对象。
//
// llm_utils_chat 的 translator 期望嵌套在 usage 字段里（_map_usage 的读取口径）。
func TestTranslateRemoteEventsUsageNesting(t *testing.T) {
	upstream := "event: token_usage\ndata: {\"prompt_tokens\":100,\"completion_tokens\":20," +
		"\"total_tokens\":120,\"cache_read_input_tokens\":50}\n\n"
	out, _ := io.ReadAll(translateRemoteEvents(io.NopCloser(strings.NewReader(upstream)), "glm-5.3", ""))
	var payload map[string]any
	for _, line := range strings.Split(string(out), "\n") {
		if strings.HasPrefix(line, "data: ") {
			if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &payload); err != nil {
				t.Fatalf("payload 非法：%v", err)
			}
		}
	}
	usage, _ := payload["usage"].(map[string]any)
	if usage == nil {
		t.Fatalf("usage 应被包成嵌套对象：%s", string(out))
	}
	if v, _ := usage["prompt_tokens"].(float64); v != 100 {
		t.Errorf("prompt_tokens 应为 100，实际 %v", usage["prompt_tokens"])
	}
	if v, _ := usage["cache_read_input_tokens"].(float64); v != 50 {
		t.Errorf("cache_read_input_tokens 应为 50，实际 %v", usage["cache_read_input_tokens"])
	}
}

// TestRemoteToolsAreNotPassedThrough Remote 的沙箱工具调用**不得**透传给客户端。
//
// 这是 2026-10-15 实测后定的口径：Remote 是自带沙箱的 agent，它用自己的工具
// （实测 LS / EnvironmentSetup / finish / Glob / Read…）自行执行并继续下一轮，
// 不是「向客户端请求执行工具」的协议。透传会同时踩两个坑：
//  1. 参数永远不闭合（Remote 不需要客户端回结果），网关校验器报
//     "upstream ended with incomplete or invalid tool arguments"，整个回答变失败；
//  2. 客户端会去执行一个已经被 Remote 执行过的工具（双重执行），
//     而且这些沙箱工具客户端根本没有。
func TestRemoteToolsAreNotPassedThrough(t *testing.T) {
	upstream := strings.Join([]string{
		"event: plan_item\ndata: {\"thought\":\"let me look\"}\n\n",
		"event: plan_item\ndata: {\"tool_call_info\":{\"id\":\"pi_1\",\"name\":\"LS\",\"params\":{\"path\":\"/workspace\"}}}\n\n",
		"event: plan_item\ndata: {\"tool_call_info\":{\"id\":\"h1\",\"name\":\"EnvironmentSetup\",\"params\":{\"command\":\"x\"}}}\n\n",
		"event: plan_item\ndata: {\"thought\":\"found it\"}\n\n",
		"event: done\ndata: {\"status\":\"completed\"}\n\n",
	}, "")
	out, err := io.ReadAll(translateRemoteEvents(io.NopCloser(strings.NewReader(upstream)), "glm-5.3", ""))
	if err != nil {
		t.Fatalf("转译失败：%v", err)
	}
	text := string(out)
	for _, want := range []string{"let me look", "found it"} {
		if !strings.Contains(text, want) {
			t.Errorf("正文应含 %q：%s", want, text)
		}
	}
	for _, forbidden := range []string{"EnvironmentSetup", "/workspace"} {
		if strings.Contains(text, forbidden) {
			t.Errorf("Remote 工具活动不该透传（出现了 %q）：%s", forbidden, text)
		}
	}
}

// TestRemoteConverterCountsToolRounds 工具轮次只计数（供日志），不进输出。
func TestRemoteConverterCountsToolRounds(t *testing.T) {
	conv := &remoteEventConverter{}
	// 同一个工具重复出现只算一轮。
	for i := 0; i < 3; i++ {
		conv.convert("plan_item", `{"tool_call_info":{"id":"p1","name":"LS","params":{"path":"/w"}}}`)
	}
	conv.convert("plan_item", `{"tool_call_info":{"id":"p2","name":"Glob","params":{"pattern":"**"}}}`)
	if conv.toolRounds != 2 {
		t.Errorf("应计 2 轮工具（LS 去重 + Glob），实际 %d", conv.toolRounds)
	}
	// 计数不影响输出：无 thought 的工具帧应被丢弃。
	if out, ok := conv.convert("plan_item", `{"tool_call_info":{"id":"p3","name":"Read"}}`); ok {
		t.Errorf("纯工具帧不该产出输出：%s", out)
	}
	// finish 工具不算工具轮次，它的 summary 是答案。
	if out, ok := conv.convert("plan_item", `{"tool_call_info":{"id":"f","name":"finish","params":{"summary":"done"}}}`); ok {
		t.Errorf("finish 帧本身不该产出 output（summary 在收尾兜底）：%s", out)
	}
	if conv.finishSummary != "done" {
		t.Errorf("finish summary 应被记录，实际 %q", conv.finishSummary)
	}
}

// TestTranslateRemoteEventsHeartbeat 心跳转发（不静默丢弃）。
func TestTranslateRemoteEventsHeartbeat(t *testing.T) {
	upstream := strings.Join([]string{
		"event: heartbeat\ndata: {}\n\n",
		"event: plan_item\ndata: {\"content\":\"ok\"}\n\n",
		"event: done\ndata: {\"status\":\"completed\"}\n\n",
	}, "")
	out, err := io.ReadAll(translateRemoteEvents(io.NopCloser(strings.NewReader(upstream)), "glm-5.3", ""))
	if err != nil {
		t.Fatalf("转译失败：%v", err)
	}
	if !strings.Contains(string(out), ": relay-keepalive") {
		t.Errorf("心跳应转成保活注释行：%s", out)
	}
}

// TestContainsSubstantiveDelta role 帧不算实质内容。
//
// 这决定了「上游在首个事件就发 3004」时能不能回落：translator 会先发一帧
// `{"delta":{"role":"assistant"}}`，若把它当成「已交付内容」，回落就永远不会触发。
func TestContainsSubstantiveDelta(t *testing.T) {
	cases := []struct {
		name  string
		chunk string
		want  bool
	}{
		{"role 帧不算", `data: {"choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}` + "\n\n", false},
		{"空 delta 不算", `data: {"choices":[{"index":0,"delta":{},"finish_reason":null}]}` + "\n\n", false},
		{"正文算", `data: {"choices":[{"index":0,"delta":{"content":"PONG"}}]}` + "\n\n", true},
		{"思考算", `data: {"choices":[{"index":0,"delta":{"reasoning_content":"hmm"}}]}` + "\n\n", true},
		{"工具帧算", `data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0}]}}]}` + "\n\n", true},
		{"无 data 行不算", ": relay-keepalive\n\n", false},
		{"非法 JSON 保守当作有内容", "data: {not json\n\n", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := containsSubstantiveDelta([]byte(tc.chunk)); got != tc.want {
				t.Errorf("containsSubstantiveDelta=%v，应为 %v", got, tc.want)
			}
		})
	}
}

// TestIsToolTurnLimitErrorOnlyMatches3004 回落判据必须只认 3004 业务码。
//
// 用文案判会误伤真正的限流（那种换号能解决，换端点没意义），且文案会随上游改版漂移。
func TestIsToolTurnLimitErrorOnlyMatches3004(t *testing.T) {
	if !isToolTurnLimitError(&upstreamerr.Error{Code: 3004, Message: "rate limit"}) {
		t.Error("3004 应被识别为工具轮次超限")
	}
	if isToolTurnLimitError(&upstreamerr.Error{Code: 4001, Message: "bad request"}) {
		t.Error("4001 不该触发 Remote 回落")
	}
	if isToolTurnLimitError(io.EOF) {
		t.Error("普通 EOF 不该触发回落")
	}
	if isToolTurnLimitError(nil) {
		t.Error("nil 不该触发回落")
	}
	// 包装一层仍要能识别。
	wrapped := error(&upstreamerr.Error{Code: 3004, Message: "rate limit"})
	if !isToolTurnLimitError(wrapped) {
		t.Error("包装后仍应识别 3004")
	}
}

// TestRemoteFallbackSwitchesOn3004 llm_utils_chat 被拒（3004）时，透明切到 Remote 并交付正文。
func TestRemoteFallbackSwitchesOn3004(t *testing.T) {
	stub := &remoteStub{
		createBody: `{"code":0,"data":{"chat_session_id":"sid-1","message_id":"mid-1"},"message":"success"}`,
		eventsBody: "event: plan_item\ndata: {\"content\":\"REMOTE-ANSWER\"}\n\n" +
			"event: done\ndata: {\"status\":\"completed\"}\n\n",
	}
	srv := stub.start(t)
	old := RemoteBaseVar
	RemoteBaseVar = srv.URL
	t.Cleanup(func() { RemoteBaseVar = old })

	// primary：模拟 llm_utils_chat 先给一帧 role，然后报 3004。
	primary := newStubReadCloser(
		`data: {"choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}`+"\n\n",
		&upstreamerr.Error{Code: 3004, Message: "exceeded the rate limit"})

	prepared, _ := json.Marshal(map[string]any{
		"model":    "glm-5.3",
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	})
	r := newRemoteFallbackReader(context.Background(), testAuth(), prepared, "", primary, false)

	// 先读一轮：应触发回落并拿到 Remote 的正文。
	var got strings.Builder
	buf := make([]byte, 4096)
	for i := 0; i < 40; i++ {
		n, err := r.Read(buf)
		got.Write(buf[:n])
		if err != nil {
			break
		}
	}
	if stub.createCalls != 1 {
		t.Errorf("应建一次 Remote 会话，实际 %d", stub.createCalls)
	}
	if stub.eventsCalls != 1 {
		t.Errorf("应收一次流，实际 %d", stub.eventsCalls)
	}
	if !strings.Contains(got.String(), "REMOTE-ANSWER") {
		t.Fatalf("应交付 Remote 的正文，实际：%s", got.String())
	}
	// 开场 role 帧必须被扣住丢掉，不能让下游看到两次开场。
	if strings.Count(got.String(), `"role":"assistant"`) > 1 {
		t.Errorf("开场 role 帧不该重复下发：%s", got.String())
	}
}

// TestRemoteFallbackDoesNotTriggerAfterContent 已交付正文后绝不回落（避免重复输出）。
func TestRemoteFallbackDoesNotTriggerAfterContent(t *testing.T) {
	stub := &remoteStub{
		createBody: `{"code":0,"data":{"chat_session_id":"sid","message_id":"mid"}}`,
		eventsBody: "event: plan_item\ndata: {\"content\":\"SHOULD-NOT-APPEAR\"}\n\n",
	}
	srv := stub.start(t)
	old := RemoteBaseVar
	RemoteBaseVar = srv.URL
	t.Cleanup(func() { RemoteBaseVar = old })

	primary := newStubReadCloser(
		`data: {"choices":[{"index":0,"delta":{"content":"partial"}}]}`+"\n\n",
		&upstreamerr.Error{Code: 3004, Message: "rate limit"})

	prepared, _ := json.Marshal(map[string]any{"model": "glm-5.3"})
	r := newRemoteFallbackReader(context.Background(), testAuth(), prepared, "", primary, false)

	var got strings.Builder
	buf := make([]byte, 4096)
	for i := 0; i < 40; i++ {
		n, err := r.Read(buf)
		got.Write(buf[:n])
		if err != nil {
			break
		}
	}
	if stub.createCalls != 0 {
		t.Error("已交付正文后不该回落（会导致用户看到重复/断裂输出）")
	}
	if strings.Contains(got.String(), "SHOULD-NOT-APPEAR") {
		t.Error("不该出现 Remote 的内容")
	}
	if !strings.Contains(got.String(), "partial") {
		t.Error("已交付的正文应保留")
	}
}

// TestRemoteFallbackReportsBothFailures 回落也失败时，两个原因都要如实报告。
func TestRemoteFallbackReportsBothFailures(t *testing.T) {
	stub := &remoteStub{createBody: `{"code":9074,"message":"upstream busy"}`}
	srv := stub.start(t)
	old := RemoteBaseVar
	RemoteBaseVar = srv.URL
	t.Cleanup(func() { RemoteBaseVar = old })

	primary := newStubReadCloser("", &upstreamerr.Error{Code: 3004, Message: "rate limit"})
	prepared, _ := json.Marshal(map[string]any{"model": "glm-5.3"})
	r := newRemoteFallbackReader(context.Background(), testAuth(), prepared, "", primary, false)

	var final error
	buf := make([]byte, 4096)
	for i := 0; i < 40; i++ {
		_, err := r.Read(buf)
		if err != nil {
			final = err
			break
		}
	}
	if final == nil {
		t.Fatal("两边都失败应报错")
	}
	if !strings.Contains(final.Error(), "Remote") {
		t.Errorf("错误应说明 Remote 回落也失败了：%v", final)
	}
}

// stubReadCloser 先吐一段固定字节，然后返回指定错误。
type stubReadCloser struct {
	chunk []byte
	err   error
	sent  bool
}

func newStubReadCloser(chunk string, err error) *stubReadCloser {
	return &stubReadCloser{chunk: []byte(chunk), err: err}
}

func (s *stubReadCloser) Read(p []byte) (int, error) {
	if !s.sent && len(s.chunk) > 0 {
		s.sent = true
		n := copy(p, s.chunk)
		if n < len(s.chunk) {
			s.chunk = s.chunk[n:]
			s.sent = false
		}
		return n, nil
	}
	return 0, s.err
}

func (s *stubReadCloser) Close() error { return nil }

// TestRemoteBaseIsStable 端到端契约：RemoteBase 常量指向正确的路径。
func TestRemoteBaseIsStable(t *testing.T) {
	if !strings.HasSuffix(RemoteBase, "/api/remote/v1") {
		t.Errorf("RemoteBase 应以 /api/remote/v1 结尾，实际 %s", RemoteBase)
	}
	if !strings.HasPrefix(RemoteBase, "https://") {
		t.Errorf("RemoteBase 应是 https，实际 %s", RemoteBase)
	}
}

// TestMaxModeCustomModel max 档 enrichment 只在目录标了 max_mode 时生效。
func TestMaxModeCustomModel(t *testing.T) {
	if _, ok := maxModeCustomModel(map[string]any{"max_mode": false}); ok {
		t.Error("未标 max_mode 的模型不该被套 1M 档")
	}
	enriched, ok := maxModeCustomModel(map[string]any{
		"max_mode": true, "config_name": "glm-5.3",
	})
	if !ok {
		t.Fatal("标了 max_mode 的模型应可 enrichment")
	}
	cws, _ := enriched["context_window_size"].(map[string]any)
	if cws == nil {
		t.Fatal("context_window_size 应是 {default, max} 对象")
	}
	if maxes, _ := cws["max"].([]any); len(maxes) != 1 || maxes[0] != int64(defaultMaxContextTokens) {
		t.Errorf("context_window_size.max 应为 [1M]，实际 %v", cws["max"])
	}
	if v, _ := enriched["prompt_max_tokens"].(int64); v != defaultMaxPromptTokens {
		t.Errorf("prompt_max_tokens 应为 %d，实际 %v", defaultMaxPromptTokens, enriched["prompt_max_tokens"])
	}
	if v, _ := enriched["max_tokens"].(int64); v != defaultMaxOutputTokens {
		t.Errorf("max_tokens 应为 %d，实际 %v", defaultMaxOutputTokens, enriched["max_tokens"])
	}
}

// TestMaxModeFields 六件套的形态。
func TestMaxModeFields(t *testing.T) {
	fields := maxModeFields(map[string]any{})
	auto, _ := fields["model_auto_selection"].(map[string]any)
	if auto == nil || auto["strategy"] != "max" {
		t.Fatalf("model_auto_selection.strategy 应为 max：%v", fields)
	}
	// fallback_to_advance_model / entitlement_id 是显式 null。
	encoded, _ := json.Marshal(auto)
	if !strings.Contains(string(encoded), `"fallback_to_advance_model":null`) {
		t.Errorf("fallback_to_advance_model 应显式为 null：%s", encoded)
	}
	if fields["model_selection_strategy"] != "max" {
		t.Errorf("model_selection_strategy 应为 max：%v", fields)
	}
	if v, _ := fields["mode_type"].(int64); v != defaultMaxModeType {
		t.Errorf("mode_type 应为 %d：%v", defaultMaxModeType, fields["mode_type"])
	}
	if v, _ := fields["context_window_size"].(int64); v != defaultMaxContextTokens {
		t.Errorf("context_window_size 应为 %d：%v", defaultMaxContextTokens, fields["context_window_size"])
	}
}

// TestCustomModelForFromCatalog 目录可用时按 config_name 匹配并补全字段。
func TestCustomModelForFromCatalog(t *testing.T) {
	models := []CatalogModel{
		{ConfigName: "glm-5.3", DisplayName: "GLM 5.3", MaxMode: true, Multimodal: true},
	}
	custom, ok := customModelFor(models, "glm-5.3", remoteDefaultAgentType, true)
	if !ok {
		t.Fatal("目录里的模型应可绑定")
	}
	if custom["config_name"] != "glm-5.3" || custom["model_name"] != "glm-5.3" {
		t.Errorf("custom_model 应含 config_name/model_name：%v", custom)
	}
	if custom["max_mode"] != true || custom["multimodal"] != true {
		t.Errorf("max_mode/multimodal 应透传：%v", custom)
	}
	if custom["use_remote_service"] != true {
		t.Errorf("use_remote_service 应为 true：%v", custom)
	}
	// 目录没有但静态表认识的（别名解析成已知模型）。
	custom2, ok2 := customModelFor(models, "gpt-4o", remoteDefaultAgentType, true)
	if !ok2 {
		t.Fatal("静态表认识的别名应可绑定")
	}
	if custom2["config_name"] != "DeepSeek-V4-Pro" {
		t.Errorf("gpt-4o 应解析到 DeepSeek-V4-Pro：%v", custom2)
	}
	// 完全未知的模型不绑定（让上游走 auto，不硬绑不存在的名字）。
	if _, ok3 := customModelFor(models, "totally-unknown-model", remoteDefaultAgentType, true); ok3 {
		t.Error("未知模型不该硬绑 custom_model")
	}
}

// TestRemoteFirstEventReader 首模型事件超时。
func TestRemoteFirstEventReader(t *testing.T) {
	// 心跳不算模型事件：只发心跳的流在超时后报错。
	heartbeats := &slowReadCloser{lines: []string{"event: heartbeat\ndata: {}\n\n"}}
	r := newRemoteFirstEventReader(heartbeats)
	r.deadline = time.Now().Add(-time.Second) // 已过期
	buf := make([]byte, 4096)
	heartbeats.onRead = func() {
		r.deadline = time.Now().Add(-time.Second)
	}
	if _, err := r.Read(buf); err == nil || !strings.Contains(err.Error(), "首") {
		// 首次读到心跳（未见过模型事件），第二次读应判超时。
		_, err2 := r.Read(buf)
		if err2 == nil || !strings.Contains(err2.Error(), "首") {
			t.Errorf("只发心跳的流应判首事件超时，err=%v err2=%v", err, err2)
		}
	}
	// 见过模型事件后不再计时。
	r2 := newRemoteFirstEventReader(io.NopCloser(strings.NewReader(
		"event: plan_item\ndata: {\"content\":\"x\"}\n\nevent: done\ndata: {}\n\n")))
	r2.deadline = time.Now().Add(-time.Second)
	out, err := io.ReadAll(r2)
	if err != nil {
		t.Fatalf("有模型事件的流不该超时：%v", err)
	}
	if !strings.Contains(string(out), "plan_item") {
		t.Errorf("模型事件应透传：%s", out)
	}
}

// slowReadCloser 逐行吐数据的慢速流（模拟上游心跳间隔）。
type slowReadCloser struct {
	lines  []string
	onRead func()
}

func (s *slowReadCloser) Read(p []byte) (int, error) {
	if s.onRead != nil {
		s.onRead()
	}
	if len(s.lines) == 0 {
		return 0, io.EOF
	}
	line := s.lines[0]
	s.lines = s.lines[1:]
	n := copy(p, line)
	return n, nil
}

func (s *slowReadCloser) Close() error { return nil }

// ═══ 路径反转的契约（2026-10-16，ChatStream 主路径 = Remote）═══

// TestChatStreamPrefersRemote ChatStream 优先走 Remote：Remote 可用时不碰
// llm_utils_chat（同一 stub 上不应出现对 ChatPath 的请求）。
func TestChatStreamPrefersRemote(t *testing.T) {
	stub := &remoteStub{
		createBody: `{"code":0,"data":{"chat_session_id":"sid-p","message_id":"mid-p"}}`,
		eventsBody: "event: plan_item\ndata: {\"content\":\"FROM-REMOTE\"}\n\n" +
			"event: done\ndata: {\"status\":\"completed\"}\n\n",
	}
	srv := stub.start(t)
	oldRemote := RemoteBaseVar
	RemoteBaseVar = srv.URL
	t.Cleanup(func() { RemoteBaseVar = oldRemote })

	legacyCalls := 0
	legacy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == ChatPath { // 目录请求（CatalogPath）不算 llm_utils_chat 调用
			legacyCalls++
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(legacy.Close)
	oldGateway := GatewayBaseVar
	GatewayBaseVar = legacy.URL
	t.Cleanup(func() { GatewayBaseVar = oldGateway })
	InvalidateCatalog("") // 目录缓存也指向旧基址，清掉

	prepared, _ := json.Marshal(map[string]any{
		"model":    "glm-5.3",
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	})
	rc, _, _, err := ChatStream(context.Background(), testAuth(), prepared, "")
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	defer rc.Close()
	out, rerr := io.ReadAll(rc)
	if rerr != nil && rerr != io.EOF {
		t.Fatalf("读流: %v", rerr)
	}
	if !strings.Contains(string(out), "FROM-REMOTE") {
		t.Errorf("Remote 可用时正文应来自 Remote：%s", out)
	}
	if stub.createCalls != 1 {
		t.Errorf("应建一次 Remote 会话，实际 %d", stub.createCalls)
	}
	if legacyCalls != 0 {
		t.Errorf("Remote 可用时不应碰 llm_utils_chat（调用 %d 次）", legacyCalls)
	}
}

// TestChatStreamFallsBackToLegacyOnRemoteFailure Remote 建会话失败时回落
// llm_utils_chat，正文来自回落端点（两个端点故障域不同，互为冗余）。
func TestChatStreamFallsBackToLegacyOnRemoteFailure(t *testing.T) {
	// Remote：建会话一律业务码失败。
	remoteDown := &remoteStub{createBody: `{"code":9074,"message":"upstream busy"}`}
	rsrv := remoteDown.start(t)
	oldRemote := RemoteBaseVar
	RemoteBaseVar = rsrv.URL
	t.Cleanup(func() { RemoteBaseVar = oldRemote })

	legacy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("event: output\ndata: {\"response\":\"FROM-LEGACY\"}\n\n" +
			"event: done\ndata: {}\n\n"))
	}))
	t.Cleanup(legacy.Close)
	oldGateway := GatewayBaseVar
	GatewayBaseVar = legacy.URL
	t.Cleanup(func() { GatewayBaseVar = oldGateway })
	InvalidateCatalog("")

	prepared, _ := json.Marshal(map[string]any{
		"model":    "glm-5.3",
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	})
	rc, _, _, err := ChatStream(context.Background(), testAuth(), prepared, "")
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	defer rc.Close()
	out, rerr := io.ReadAll(rc)
	if rerr != nil && rerr != io.EOF {
		t.Fatalf("读流: %v", rerr)
	}
	if !strings.Contains(string(out), "FROM-LEGACY") {
		t.Errorf("Remote 失败时正文应来自 llm_utils_chat：%s", out)
	}
}

// TestChatStreamRemoteDisabledGoesLegacy WB2API_TRAE_REMOTE=0 时直接走
// llm_utils_chat（排障开关，跳过 Remote 主路径）。
func TestChatStreamRemoteDisabledGoesLegacy(t *testing.T) {
	remoteCalled := 0
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		remoteCalled++
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(remote.Close)
	oldRemote := RemoteBaseVar
	RemoteBaseVar = remote.URL
	t.Cleanup(func() { RemoteBaseVar = oldRemote })

	legacy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("event: output\ndata: {\"response\":\"LEGACY-ONLY\"}\n\n" +
			"event: done\ndata: {}\n\n"))
	}))
	t.Cleanup(legacy.Close)
	oldGateway := GatewayBaseVar
	GatewayBaseVar = legacy.URL
	t.Cleanup(func() { GatewayBaseVar = oldGateway })
	InvalidateCatalog("")

	t.Setenv("WB2API_TRAE_REMOTE", "0")

	prepared, _ := json.Marshal(map[string]any{
		"model":    "glm-5.3",
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	})
	rc, _, _, err := ChatStream(context.Background(), testAuth(), prepared, "")
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	defer rc.Close()
	out, rerr := io.ReadAll(rc)
	if rerr != nil && rerr != io.EOF {
		t.Fatalf("读流: %v", rerr)
	}
	if !strings.Contains(string(out), "LEGACY-ONLY") {
		t.Errorf("关闭 Remote 后正文应来自 llm_utils_chat：%s", out)
	}
	if remoteCalled != 0 {
		t.Errorf("WB2API_TRAE_REMOTE=0 时不应碰 Remote（调用 %d 次）", remoteCalled)
	}
}
