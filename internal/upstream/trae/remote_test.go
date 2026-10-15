// ═══ 更新日志 ═══
// 2026-10-15：初版——Remote 会话端点回落（工具轮次超限 3004）的回归。
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
	query := buildRemoteQuery(messages)

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

	for _, want := range []string{"[System]", "[Assistant]", "[Client Tool Result]",
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

// TestRemoteTrimHistoryKeepsSystemAndNewest 裁剪保留全部 system 与最新消息。
func TestRemoteTrimHistoryKeepsSystemAndNewest(t *testing.T) {
	filler := strings.Repeat("x", 2000)
	messages := []map[string]any{
		{"role": "system", "content": filler},
	}
	for i := 0; i < 20; i++ {
		role := "user"
		if i%2 == 1 {
			role = "assistant"
		}
		messages = append(messages, map[string]any{"role": role, "content": filler})
	}
	kept, dropped := trimRemoteHistory(messages, 5000)
	if dropped == 0 {
		t.Fatal("超限时应丢弃历史")
	}
	if len(kept) == 0 {
		t.Fatal("不该裁到空")
	}
	if role, _ := kept[0]["role"].(string); role != "system" {
		t.Errorf("system 必须保留在首位，实际 %v", kept[0]["role"])
	}
	// 保留的应是**最新**的：最后一条必须与原文最后一条相同。
	last, _ := kept[len(kept)-1]["content"].(string)
	orig, _ := messages[len(messages)-1]["content"].(string)
	if last != orig {
		t.Error("应保留最新消息（从最早的开始丢）")
	}
}

// TestRemoteTrimDropsToolPairsTogether 裁剪时成对丢（assistant(tool_calls) + 紧随的 tool）。
//
// 只丢一半会留下孤立的工具结果，上游对「调用与结果不配对」是直接报错的。
func TestRemoteTrimDropsToolPairsTogether(t *testing.T) {
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
	kept, _ := trimRemoteHistory(messages, 4000)
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

// TestRemoteBodyShape 建会话请求体的形态（字段名与枚举值）。
func TestRemoteBodyShape(t *testing.T) {
	prepared, _ := json.Marshal(map[string]any{
		"model":    "glm-5.3",
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	})
	body, kept, dropped, err := buildRemoteBody(prepared, "req-1")
	if err != nil {
		t.Fatalf("构造失败：%v", err)
	}
	if kept != 1 || dropped != 0 {
		t.Errorf("小请求不该裁剪：kept=%d dropped=%d", kept, dropped)
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
	if init["model_name"] != "glm-5.3" {
		t.Errorf("model_name 应为 glm-5.3：%v", init["model_name"])
	}
	if init["chat_session_id"] != "" {
		t.Errorf("chat_session_id 应为空串（服务端回填）：%v", init["chat_session_id"])
	}
	common, _ := init["common_params"].(string)
	for _, want := range []string{"marscode-cn", "biz_session_id", "req-1"} {
		if !strings.Contains(common, want) {
			t.Errorf("common_params 应含 %q：%s", want, common)
		}
	}
}

// TestRemoteHeadersAreWebShaped Remote 头族必须是网页端形态。
//
// 与 llm_utils_chat 的 SOLO/IDE 指纹头**不能混用**：混用会得到 401/404。
func TestRemoteHeadersAreWebShaped(t *testing.T) {
	a := testAuth()
	h := remoteHeaders(a)
	checks := map[string]string{
		"Authorization":        "Cloud-IDE-JWT " + a.AccessToken,
		"X-Trae-Client-Type":   "web",
		"Origin":               "https://solo.trae.cn",
		"Referer":              "https://solo.trae.cn/",
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
	// 不得混入 llm_utils_chat 的指纹头。
	for _, forbidden := range []string{"x-ide-version", "x-app-id", "x-uid", "x-ide-token"} {
		if h.Get(forbidden) != "" {
			t.Errorf("Remote 头不该含 %s（那是 llm_utils_chat 的指纹头）", forbidden)
		}
	}
}

// TestTranslateRemoteEventsExtractsThought Remote 的 plan_item.thought 要转成 output.response。
//
// 这是正文所在：Remote 不给 output 事件，正文在 plan_item 的 thought 里（累计快照）。
func TestTranslateRemoteEventsExtractsThought(t *testing.T) {
	upstream := strings.Join([]string{
		"event: status_changed\ndata: {\"new_status\":3}\n\n",
		"event: metadata\ndata: {\"message_id\":\"m1\"}\n\n",
		"event: plan_item\ndata: {\"thought\":\"PONG\",\"reasoning_content\":\"\"}\n\n",
		"event: token_usage\ndata: {\"prompt_tokens\":10,\"completion_tokens\":4,\"total_tokens\":14}\n\n",
		"event: done\ndata: {\"status\":\"completed\"}\n\n",
	}, "")
	out, err := io.ReadAll(translateRemoteEvents(io.NopCloser(strings.NewReader(upstream)), "glm-5.3"))
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

// TestTranslateRemoteEventsUsageNesting Remote 的 usage 是平铺的，要包成 usage 对象。
//
// llm_utils_chat 的 translator 期望嵌套在 usage 字段里（_map_usage 的读取口径）。
func TestTranslateRemoteEventsUsageNesting(t *testing.T) {
	upstream := "event: token_usage\ndata: {\"prompt_tokens\":100,\"completion_tokens\":20," +
		"\"total_tokens\":120,\"cache_read_input_tokens\":50}\n\n"
	out, _ := io.ReadAll(translateRemoteEvents(io.NopCloser(strings.NewReader(upstream)), "glm-5.3"))
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

// TestRemoteToolsAreNotPassedThrough Remote 的工具调用**不得**透传给客户端。
//
// 这是 2026-10-15 实测后定的口径：Remote 是自带沙箱的 agent，它用自己的工具
// （实测 LS / EnvironmentSetup / finish / Glob / Read…）自行执行并继续下一轮，
// 不是「向客户端请求执行工具」的协议。透传会同时踩两个坑：
//   1. 参数永远不闭合（Remote 不需要客户端回结果），网关校验器报
//      "upstream ended with incomplete or invalid tool arguments"，整个回答变失败；
//   2. 客户端会去执行一个已经被 Remote 执行过的工具（双重执行），
//      而且这些沙箱工具客户端根本没有。
func TestRemoteToolsAreNotPassedThrough(t *testing.T) {
	upstream := strings.Join([]string{
		"event: plan_item\ndata: {\"thought\":\"let me look\"}\n\n",
		"event: plan_item\ndata: {\"tool_call_info\":{\"id\":\"pi_1\",\"name\":\"LS\",\"params\":{\"path\":\"/workspace\"}}}\n\n",
		"event: plan_item\ndata: {\"tool_call_info\":{\"id\":\"h1\",\"name\":\"EnvironmentSetup\",\"params\":{\"command\":\"x\"}}}\n\n",
		"event: plan_item\ndata: {\"thought\":\"found it\"}\n\n",
		"event: done\ndata: {\"status\":\"completed\"}\n\n",
	}, "")
	out, err := io.ReadAll(translateRemoteEvents(io.NopCloser(strings.NewReader(upstream)), "glm-5.3"))
	if err != nil {
		t.Fatalf("转译失败：%v", err)
	}
	text := string(out)
	for _, want := range []string{"let me look", "found it"} {
		if !strings.Contains(text, want) {
			t.Errorf("正文应含 %q：%s", want, text)
		}
	}
	for _, forbidden := range []string{"tool_calls", "LS", "EnvironmentSetup"} {
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

// TestRemoteFallbackSwitchesOn3004 上游在首个事件发 3004 时，透明切到 Remote 并交付正文。
//
// 这是本次修复的核心行为：长会话（工具轮次超限）不再 502，而是换端点拿到回答。
func TestRemoteFallbackSwitchesOn3004(t *testing.T) {
	stub := &remoteStub{
		createBody: `{"code":0,"data":{"chat_session_id":"sid-1","message_id":"mid-1"},"message":"success"}`,
		eventsBody: "event: plan_item\ndata: {\"thought\":\"REMOTE-ANSWER\"}\n\n" +
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
	r := newRemoteFallbackReader(context.Background(), testAuth(), prepared, "", primary)

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
		eventsBody: "event: plan_item\ndata: {\"thought\":\"SHOULD-NOT-APPEAR\"}\n\n",
	}
	srv := stub.start(t)
	old := RemoteBaseVar
	RemoteBaseVar = srv.URL
	t.Cleanup(func() { RemoteBaseVar = old })

	primary := newStubReadCloser(
		`data: {"choices":[{"index":0,"delta":{"content":"partial"}}]}`+"\n\n",
		&upstreamerr.Error{Code: 3004, Message: "rate limit"})

	prepared, _ := json.Marshal(map[string]any{"model": "glm-5.3"})
	r := newRemoteFallbackReader(context.Background(), testAuth(), prepared, "", primary)

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
	r := newRemoteFallbackReader(context.Background(), testAuth(), prepared, "", primary)

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

var _ = time.Second
