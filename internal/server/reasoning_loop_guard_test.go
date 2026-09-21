// ═══ 更新日志 ═══
// 2026-09-21：保护退回「命中即停止」：去掉同账号重发断言，改为断言每次命中只调用上游
// 一次、错误码如实回报、账号与粘性不受影响；正文循环仍用正文侧错误码。
// 2026-09-19：以合成上游覆盖重复推理保护的四种出口、失败用量及账号/粘性边界。
// 2026-09-19：按32字符短行规则补齐完整早期用量、关闭开关与正常进展边界，避免误截和用量推算。
package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/session"
	"workbuddy2api/internal/usage"
)

type reasoningGuardBody struct {
	*strings.Reader
	closed int
}

func (b *reasoningGuardBody) Close() error { b.closed++; return nil }

func reasoningGuardFrame(delta map[string]any) string {
	value := map[string]any{"id": "reasoning-guard-fixture", "choices": []any{map[string]any{"index": 0, "delta": delta}}}
	raw, _ := json.Marshal(value)
	return "data: " + string(raw) + "\n\n"
}

func reasoningGuardLines(count int) string {
	return strings.Repeat("checking the same step once more\n", count)
}

func reasoningGuardTool(index int, id, name, arguments string) string {
	return reasoningGuardFrame(map[string]any{"tool_calls": []any{map[string]any{
		"index": index, "id": id, "type": "function",
		"function": map[string]any{"name": name, "arguments": arguments},
	}}})
}

func reasoningGuardFinish(payload, reason string) string {
	return payload + reasoningGuardFrame(map[string]any{"content": "after-guard-marker"}) +
		postreleaseFinish(reason) + postreleaseUsageOnly + "data: [DONE]\n\n"
}

func reasoningGuardFixture(t *testing.T, payload, model string) (*Handler, *usage.Store, *reasoningGuardBody, *int, *bindStore) {
	t.Helper()
	h, ledger, bodies, calls, bindings := reasoningGuardFixtureSequence(t, []string{payload}, model)
	return h, ledger, bodies[0], calls, bindings
}

// reasoningGuardFixtureSequence 让每次上游调用拿到**独立的**响应体，并记录调用次数。
// 保护命中即停止后每次请求只应调用上游一次；夹具仍支持多份 payload，便于断言网关
// 没有偷偷发起第二次调用（payloads 用完后重复最后一项）。
func reasoningGuardFixtureSequence(t *testing.T, payloads []string, model string) (*Handler, *usage.Store, []*reasoningGuardBody, *int, *bindStore) {
	t.Helper()
	if len(payloads) == 0 {
		t.Fatal("fixture needs at least one payload")
	}
	auth.SetGlobalEnabled(true)
	t.Cleanup(func() { auth.SetGlobalEnabled(true) })
	h, ledger := postreleaseUsageHandler(t, payloads[0])
	domain := ""
	if strings.HasPrefix(strings.ToLower(strings.TrimSpace(model)), "global:") {
		domain = "www.workbuddy.ai"
	}
	h.cfg.Pool = testPoolWith(
		&auth.Auth{UID: "guard-first", Domain: domain, AccessToken: "fixture-first", ExpiresAt: 9999999999},
		&auth.Auth{UID: "guard-second", Domain: domain, AccessToken: "fixture-second", ExpiresAt: 9999999999},
	)
	h.cfg.MaxRotate = 3
	h.cfg.GlobalEnabled = true
	var bodies []*reasoningGuardBody
	calls := 0
	h.cfg.Upstream.GlobalEnabled = true
	h.cfg.Upstream.ChatBaseGlobal = "https://fixture.invalid"
	// 预建 body：单 payload 的便捷夹具要在发请求前就能返回 bodies[0]；重发超出
	// payload 数量时再用最后一段重建，保证每次上游调用都拿到未消费的 SSE。
	for _, payload := range payloads {
		bodies = append(bodies, &reasoningGuardBody{Reader: strings.NewReader(payload)})
	}
	h.cfg.Upstream.HTTP.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
		index := calls
		calls++
		if index >= len(bodies) {
			bodies = append(bodies, &reasoningGuardBody{Reader: strings.NewReader(payloads[len(payloads)-1])})
		}
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: bodies[index]}, nil
	})
	bindings := newBindStore()
	h.cfg.Session = session.New(session.Config{TTL: time.Hour, Store: bindings,
		Available: func() []string { return []string{"guard-first", "guard-second"} }})
	h.cfg.Session.Bind("reasoning-guard-session", "guard-first")
	return h, ledger, bodies, &calls, bindings
}

func reasoningGuardRequest(path, model string, streaming bool, tools bool) *http.Request {
	value := map[string]any{"model": model, "stream": streaming, "conversation_id": "reasoning-guard-session",
		"metadata": map[string]any{"conversation_id": "reasoning-guard-session"}}
	if path == "/v1/responses" {
		value["input"] = "continue the fixture task"
	} else {
		value["messages"] = []any{map[string]any{"role": "user", "content": "continue the fixture task"}}
	}
	if tools {
		function := map[string]any{"name": "lookup", "parameters": map[string]any{"type": "object", "properties": map[string]any{}}}
		if path == "/v1/responses" {
			function["type"] = "function"
			value["tools"] = []any{function}
		} else {
			value["tools"] = []any{map[string]any{"type": "function", "function": function}}
		}
	}
	raw, _ := json.Marshal(value)
	return httptest.NewRequest(http.MethodPost, path, strings.NewReader(string(raw)))
}

func assertReasoningGuardFailure(t *testing.T, recorder *httptest.ResponseRecorder, path string, streaming bool) {
	t.Helper()
	assertReasoningGuardFailureStatus(t, recorder, path, streaming, http.StatusUnprocessableEntity)
}

// loopGuardErrorObjects 从响应体里抽出每个 `"error":{...}` 对象（Chat 的错误帧与
// Responses 的 response.error 都走这个形状），用于断言错误本身没有回显被监控的文本。
func loopGuardErrorObjects(t *testing.T, body string) []string {
	t.Helper()
	var objects []string
	for offset := 0; ; {
		index := strings.Index(body[offset:], `"error":`)
		if index < 0 {
			break
		}
		start := offset + index + len(`"error":`)
		depth, end := 0, -1
		for position := start; position < len(body); position++ {
			switch body[position] {
			case '{':
				depth++
			case '}':
				depth--
				if depth == 0 {
					end = position + 1
				}
			}
			if end >= 0 {
				break
			}
		}
		if end < 0 {
			break
		}
		objects = append(objects, body[start:end])
		offset = end
	}
	if len(objects) == 0 {
		t.Fatal("response contained no error object")
	}
	return objects
}

// assertReasoningGuardFailureStatus 断言循环最终**如实回报**（重发也没救回来时）。
// wantStatus 区分两类出口：重发建立失败走 502，重发后仍循环走 422。
func assertReasoningGuardFailureStatus(t *testing.T, recorder *httptest.ResponseRecorder, path string, streaming bool, wantStatus int) {
	assertLoopGuardFailureCode(t, recorder, path, streaming, wantStatus, "upstream_reasoning_loop")
}

// assertLoopGuardFailureCode 断言循环最终**如实回报**，并核对稳定错误码属于哪条文本流。
// 正文侧单独设码，调用方据此区分「模型在思考里打转」与「重复正文正在刷屏」。
func assertLoopGuardFailureCode(t *testing.T, recorder *httptest.ResponseRecorder, path string, streaming bool, wantStatus int, wantCode string) {
	t.Helper()
	expect := wantStatus
	if streaming {
		expect = http.StatusOK
	}
	if recorder.Code != expect {
		t.Errorf("status=%d want=%d", recorder.Code, expect)
	}
	output := recorder.Body.String()
	if !strings.Contains(output, `"code":"`+wantCode+`"`) {
		t.Errorf("missing stable loop error %q, output bytes=%d", wantCode, len(output))
	}
	if strings.Contains(output, "after-guard-marker") {
		t.Error("stream continued after the guarded loop")
	}
	if streaming && path == "/v1/responses" {
		if strings.Count(output, "event: response.failed\n") != 1 {
			t.Error("Responses did not emit exactly one failed terminal")
		}
		if strings.Contains(output, "event: response.completed\n") {
			t.Error("guarded Responses emitted a success terminal")
		}
	} else if streaming && strings.Contains(output, `"finish_reason":"stop"`) {
		t.Error("guarded Chat emitted a successful finish")
	}
}

func assertReasoningGuardAccountUnchanged(t *testing.T, before, after pool.Status) {
	t.Helper()
	if after.ErrTotal != before.ErrTotal || after.BreakerFails != before.BreakerFails || after.Disabled || after.Cooling ||
		!after.BreakerUntil.Equal(before.BreakerUntil) || !after.LastErrTime.Equal(before.LastErrTime) ||
		after.SuccessCount != before.SuccessCount || after.InFlight != 0 {
		t.Errorf("loop guard changed account health/accounting: before=%+v after=%+v", before, after)
	}
}

func assertReasoningGuardSuccess(t *testing.T, recorder *httptest.ResponseRecorder, path string, streaming bool, ledger *usage.Store) {
	t.Helper()
	output := recorder.Body.String()
	if recorder.Code != http.StatusOK || strings.Contains(output, `"code":"upstream_reasoning_loop"`) ||
		!strings.Contains(output, "after-guard-marker") {
		t.Errorf("valid output was interrupted: status=%d bytes=%d", recorder.Code, len(output))
	}
	if path == "/v1/responses" && streaming && (strings.Count(output, "event: response.completed\n") != 1 ||
		strings.Contains(output, "event: response.failed\n")) {
		t.Error("valid Responses did not emit exactly one successful terminal")
	}
	want := usage.Totals{Requests: 1, PromptTokens: 5000, CompletionTokens: 120, CachedTokens: 4096, TotalTokens: 5120, Credit: 1.25}
	if got := ledger.Snapshot().Totals; got != want {
		t.Errorf("valid output changed raw usage: got=%+v want=%+v", got, want)
	}
}

func TestReasoningLoopGuardFourExitsAndUsage(t *testing.T) {
	for _, path := range []string{"/v1/chat/completions", "/v1/responses"} {
		for _, streaming := range []bool{false, true} {
			for _, earlyUsage := range []string{"none", "partial", "complete"} {
				t.Run(fmt.Sprintf("%s/stream=%t/earlyUsage=%s", path, streaming, earlyUsage), func(t *testing.T) {
					payload := reasoningGuardFrame(map[string]any{"role": "assistant"})
					if earlyUsage == "partial" {
						payload += "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":120,\"prompt_cache_hit_tokens\":64,\"credit\":0.5}}\n\n"
					} else if earlyUsage == "complete" {
						payload += "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":120,\"completion_tokens\":8,\"prompt_cache_hit_tokens\":64,\"credit\":0.5}}\n\n"
					}
					payload += reasoningGuardFrame(map[string]any{"reasoning_content": reasoningGuardLines(300)})
					payload += reasoningGuardFrame(map[string]any{"content": "after-guard-marker"}) + postreleaseFinish("stop") + "data: [DONE]\n\n"
					model := "global:deepseek-v4.1-flash"
					// 命中即停止：网关只调用上游一次，如实回报错误，不做同账号重发。
					h, ledger, bodies, calls, bindings := reasoningGuardFixtureSequence(t, []string{payload}, model)
					beforeFirst, _ := h.cfg.Pool.Status("guard-first")
					beforeSecond, _ := h.cfg.Pool.Status("guard-second")
					recorder := httptest.NewRecorder()
					h.ServeHTTP(recorder, reasoningGuardRequest(path, model, streaming, false))
					assertReasoningGuardFailureStatus(t, recorder, path, streaming, http.StatusUnprocessableEntity)
					if *calls != 1 {
						t.Errorf("loop stop must not re-send the request: calls=%d", *calls)
					}
					for i, body := range bodies {
						if body.closed == 0 {
							t.Errorf("upstream body %d was not closed", i)
						}
					}
					afterFirst, firstPresent := h.cfg.Pool.Status("guard-first")
					afterSecond, secondPresent := h.cfg.Pool.Status("guard-second")
					if !firstPresent || !secondPresent {
						t.Fatal("loop guard removed an account from the pool")
					}
					assertReasoningGuardAccountUnchanged(t, beforeFirst, afterFirst)
					assertReasoningGuardAccountUnchanged(t, beforeSecond, afterSecond)
					if uid, ok := bindings.lastUID("reasoning-guard-session"); !ok || uid != "guard-first" {
						t.Error("loop guard invalidated the existing sticky binding")
					}
					// 账本按「一次请求」记账：失败也保留已返回的用量，但不重复累计。
					want := usage.Totals{Requests: 1, FailedRequests: 1, UnreportedRequests: 1}
					if earlyUsage != "none" {
						want.PromptTokens = 120
						want.CachedTokens = 64
						want.TotalTokens = 120
						want.Credit = 0.5
					}
					if earlyUsage == "complete" {
						want.CompletionTokens = 8
						want.TotalTokens = 128
					}
					if got := ledger.Snapshot().Totals; got != want {
						t.Errorf("partial usage lost or fabricated: got=%+v want=%+v", got, want)
					}
				})
			}
		}
	}
}

// TestOutputLoopGuardReportsContentCode 复现线上「疯狂输出」的真实形态：重复发生在
// 正文里（观测样本是 1864 行「我执行。」），推理侧几乎没有重复，旧实现因此既不中断
// 也不报错。现在命中即停止，并用正文侧错误码如实回报。
func TestOutputLoopGuardReportsContentCode(t *testing.T) {
	payload := reasoningGuardFinish(
		reasoningGuardFrame(map[string]any{"content": strings.Repeat("我执行。\n", 400)}), "stop")
	for _, path := range []string{"/v1/chat/completions", "/v1/responses"} {
		for _, streaming := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%t", path, streaming), func(t *testing.T) {
				model := "global:deepseek-v4.1-flash"
				h, _, _, calls, bindings := reasoningGuardFixtureSequence(t, []string{payload}, model)
				recorder := httptest.NewRecorder()
				h.ServeHTTP(recorder, reasoningGuardRequest(path, model, streaming, false))
				assertLoopGuardFailureCode(t, recorder, path, streaming,
					http.StatusUnprocessableEntity, "upstream_output_loop")
				if *calls != 1 {
					t.Errorf("content loop stop must not re-send the request: calls=%d", *calls)
				}
				if uid, ok := bindings.lastUID("reasoning-guard-session"); !ok || uid != "guard-first" {
					t.Error("content loop guard invalidated the existing sticky binding")
				}
				// 错误对象里只能有计数，不能把重复正文回显给调用方。流式下 output 字段
				// 本来就带模型已经写出的正文，因此这里只检查 error 对象本身。
				for _, object := range loopGuardErrorObjects(t, recorder.Body.String()) {
					if strings.Contains(object, "upstream_output_loop") && strings.Contains(object, "我执行") {
						t.Error("content loop error exposed output text")
					}
				}
			})
		}
	}
}

func TestReasoningLoopGuardNormalOutputAndProgress(t *testing.T) {
	var unique strings.Builder
	for i := 0; i < 400; i++ {
		fmt.Fprintf(&unique, "different reasoning item %04d\n", i)
	}
	longLine := strings.Repeat("推", 12000)
	before := reasoningGuardFrame(map[string]any{"reasoning_content": reasoningGuardLines(200)})
	after := reasoningGuardFrame(map[string]any{"reasoning_content": reasoningGuardLines(200)})
	cases := []struct {
		name, payload, finish, preserve string
		tools                           bool
	}{
		{name: "long-unbroken-reasoning", payload: reasoningGuardFrame(map[string]any{"reasoning_content": longLine}), preserve: longLine},
		{name: "repeated-lines-over-32-characters", payload: reasoningGuardFrame(map[string]any{"reasoning_content": strings.Repeat(strings.Repeat("m", 33)+"\n", 300)}), preserve: strings.Repeat("m", 33)},
		{name: "distinct-short-reasoning-lines", payload: reasoningGuardFrame(map[string]any{"reasoning_content": unique.String()})},
		// 正文侧的多行正常输出：不同短行很快超过闸门上限，必须实时放行。
		{name: "distinct-short-content-lines", payload: reasoningGuardFrame(map[string]any{"content": unique.String()})},
		// 正文里的长行（超过 32 字符）永远不会被计为重复，必须立即放行。
		{name: "long-content-line", payload: reasoningGuardFrame(map[string]any{"content": strings.Repeat("正常正文片段 ", 400)})},
		{name: "content-resets-window", payload: before + reasoningGuardFrame(map[string]any{"content": "visible progress"}) + after},
		{name: "refusal-resets-window", payload: before + reasoningGuardFrame(map[string]any{"refusal": "refusal progress"}) + after},
		{name: "new-tool-resets-window", payload: before + reasoningGuardTool(0, "call_guard", "lookup", "{}") + after, finish: "tool_calls", tools: true},
		{name: "tool-arguments-reset-window", payload: reasoningGuardTool(0, "call_guard", "lookup", "{") + before + reasoningGuardTool(0, "", "", "}") + after, finish: "tool_calls", tools: true},
	}
	for _, path := range []string{"/v1/chat/completions", "/v1/responses"} {
		for _, streaming := range []bool{false, true} {
			for _, tc := range cases {
				t.Run(fmt.Sprintf("%s/stream=%t/%s", path, streaming, tc.name), func(t *testing.T) {
					finish := tc.finish
					if finish == "" {
						finish = "stop"
					}
					model := "global:deepseek-v4.1-flash"
					h, ledger, body, calls, _ := reasoningGuardFixture(t, reasoningGuardFinish(tc.payload, finish), model)
					recorder := httptest.NewRecorder()
					h.ServeHTTP(recorder, reasoningGuardRequest(path, model, streaming, tc.tools))
					assertReasoningGuardSuccess(t, recorder, path, streaming, ledger)
					if tc.preserve != "" && !strings.Contains(recorder.Body.String(), tc.preserve) {
						t.Error("long reasoning text was truncated")
					}
					if *calls != 1 || body.closed == 0 {
						t.Errorf("unexpected upstream lifecycle: calls=%d closed=%d", *calls, body.closed)
					}
				})
			}
		}
	}
}

func TestReasoningLoopGuardMetadataDoesNotReset(t *testing.T) {
	before := reasoningGuardFrame(map[string]any{"reasoning_content": reasoningGuardLines(200)})
	after := reasoningGuardFrame(map[string]any{"reasoning_content": reasoningGuardLines(200)})
	cases := []struct {
		name, prefix, metadata string
		tools                  bool
	}{
		{name: "role", metadata: reasoningGuardFrame(map[string]any{"role": "assistant"})},
		{name: "empty-content", metadata: reasoningGuardFrame(map[string]any{"content": "", "refusal": ""})},
		{name: "whitespace-content", metadata: reasoningGuardFrame(map[string]any{"content": " \t\n", "refusal": " \r\n"})},
		{name: "usage-only", metadata: "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":120,\"completion_tokens\":8}}\n\n"},
		{name: "empty-tool-fields", metadata: reasoningGuardTool(0, "", "", ""), tools: true},
		{name: "type-and-index", metadata: reasoningGuardFrame(map[string]any{"tool_calls": []any{map[string]any{"index": 0, "type": "function"}}}), tools: true},
		{name: "repeated-tool-identity", prefix: reasoningGuardTool(0, "call_guard", "lookup", "{}"), metadata: reasoningGuardTool(0, "call_guard", "lookup", ""), tools: true},
	}
	for _, path := range []string{"/v1/chat/completions", "/v1/responses"} {
		for _, streaming := range []bool{false, true} {
			for _, tc := range cases {
				t.Run(fmt.Sprintf("%s/stream=%t/%s", path, streaming, tc.name), func(t *testing.T) {
					model := "global:deepseek-v4.1-flash"
					payload := reasoningGuardFinish(tc.prefix+before+tc.metadata+after, "stop")
					// 命中即停止：只调用上游一次，如实回报，不做同账号重发。
					h, ledger, bodies, calls, _ := reasoningGuardFixtureSequence(t, []string{payload, payload}, model)
					recorder := httptest.NewRecorder()
					h.ServeHTTP(recorder, reasoningGuardRequest(path, model, streaming, tc.tools))
					assertReasoningGuardFailure(t, recorder, path, streaming)
					if *calls != 1 {
						t.Errorf("unexpected upstream calls=%d want=1", *calls)
					}
					for i, body := range bodies[:min(*calls, len(bodies))] {
						if body.closed == 0 {
							t.Errorf("upstream body %d was not closed", i)
						}
					}
					got := ledger.Snapshot().Totals
					if got.Requests != 1 || got.FailedRequests != 1 || got.UnreportedRequests != 1 {
						t.Errorf("metadata hid guarded failure outcome: %+v", got)
					}
				})
			}
		}
	}
}

func TestReasoningLoopGuardModelScope(t *testing.T) {
	for _, model := range []string{
		"deepseek-v4.1-flash", "cn:deepseek-v4.1-flash", "sg:deepseek-v4.1-flash",
		"cn:deepseek-v4.1-flash-extra", "cn:gpt-5.3-codex",
	} {
		for _, path := range []string{"/v1/chat/completions", "/v1/responses"} {
			for _, streaming := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%s/stream=%t", model, path, streaming), func(t *testing.T) {
					payload := reasoningGuardFinish(reasoningGuardFrame(map[string]any{"reasoning_content": reasoningGuardLines(300)}), "stop")
					h, ledger, _, calls, _ := reasoningGuardFixtureSequence(t, []string{payload, payload}, model)
					recorder := httptest.NewRecorder()
					h.ServeHTTP(recorder, reasoningGuardRequest(path, model, streaming, false))
					wantCalls := 1
					if model == "cn:deepseek-v4.1-flash-extra" || model == "cn:gpt-5.3-codex" {
						assertReasoningGuardSuccess(t, recorder, path, streaming, ledger)
					} else {
						assertReasoningGuardFailure(t, recorder, path, streaming)
					}
					if *calls != wantCalls {
						t.Errorf("unexpected upstream calls=%d want=%d", *calls, wantCalls)
					}
				})
			}
		}
	}
}

func TestReasoningLoopGuardDisabled(t *testing.T) {
	for _, path := range []string{"/v1/chat/completions", "/v1/responses"} {
		for _, streaming := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%t", path, streaming), func(t *testing.T) {
				model := "global:deepseek-v4.1-flash"
				payload := reasoningGuardFinish(reasoningGuardFrame(map[string]any{"reasoning_content": reasoningGuardLines(300)}), "stop")
				h, ledger, body, calls, _ := reasoningGuardFixture(t, payload, model)
				disabled := false
				h.cfg.ReasoningLoopGuard = &disabled
				recorder := httptest.NewRecorder()
				h.ServeHTTP(recorder, reasoningGuardRequest(path, model, streaming, false))
				assertReasoningGuardSuccess(t, recorder, path, streaming, ledger)
				if *calls != 1 || body.closed == 0 {
					t.Errorf("disabled guard changed upstream lifecycle: calls=%d closed=%d", *calls, body.closed)
				}
			})
		}
	}
}

// TestReasoningLoopGuardStopsWithoutSecondCall 验证退回后的核心语义：命中循环时即使
// 上游「下一轮本可以成功」，网关也不再用第二个请求去救，客户端拿到的是明确的循环错误。
func TestReasoningLoopGuardStopsWithoutSecondCall(t *testing.T) {
	loopPayload := reasoningGuardFinish(reasoningGuardFrame(map[string]any{"reasoning_content": reasoningGuardLines(400)}), "stop")
	cleanPayload := reasoningGuardFinish(reasoningGuardFrame(map[string]any{"content": "after-guard-marker"}), "stop")
	outputLoopPayload := reasoningGuardFinish(
		reasoningGuardFrame(map[string]any{"content": strings.Repeat("我执行。\n", 400)}), "stop")
	for _, path := range []string{"/v1/chat/completions", "/v1/responses"} {
		for _, streaming := range []bool{false, true} {
			for _, tc := range []struct{ name, loop, code string }{
				{"reasoning", loopPayload, "upstream_reasoning_loop"},
				{"content", outputLoopPayload, "upstream_output_loop"},
			} {
				t.Run(fmt.Sprintf("%s/stream=%t/%s", path, streaming, tc.name), func(t *testing.T) {
					model := "global:deepseek-v4.1-flash"
					h, ledger, bodies, calls, _ := reasoningGuardFixtureSequence(t, []string{tc.loop, cleanPayload}, model)
					recorder := httptest.NewRecorder()
					h.ServeHTTP(recorder, reasoningGuardRequest(path, model, streaming, false))
					assertLoopGuardFailureCode(t, recorder, path, streaming, http.StatusUnprocessableEntity, tc.code)
					if *calls != 1 {
						t.Errorf("loop stop must not send a second upstream request: calls=%d", *calls)
					}
					// 客户端不该看到那份本可用于重发的干净输出。
					if strings.Contains(recorder.Body.String(), "after-guard-marker") {
						t.Error("client received the unused second-attempt payload")
					}
					// 被截断的那一轮确实计了费，只是没能读到完整 usage，因此如实标记
					// 为「用量不完整」，不能伪装成完整账单。
					if got := ledger.Snapshot().Totals.UnreportedRequests; got != 1 {
						t.Errorf("interrupted attempt should be flagged as unreported usage: %d", got)
					}
					for i, body := range bodies[:min(*calls, len(bodies))] {
						if body.closed == 0 {
							t.Errorf("upstream body %d was not closed", i)
						}
					}
				})
			}
		}
	}
}

// TestReasoningLoopGuardStopsAfterVisibleOutput 验证「已经出过正文之后才命中」的路径：
// 客户端已经收到内容，此时同样只如实回报错误，不补发、不重发。
func TestReasoningLoopGuardStopsAfterVisibleOutput(t *testing.T) {
	payload := reasoningGuardFrame(map[string]any{"content": "visible-first 正文已经放行，这一段明显超过三十二个字符的闸门上限。"}) +
		reasoningGuardFrame(map[string]any{"reasoning_content": reasoningGuardLines(400)}) +
		postreleaseFinish("stop") + "data: [DONE]\n\n"
	for _, path := range []string{"/v1/chat/completions", "/v1/responses"} {
		for _, streaming := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%t", path, streaming), func(t *testing.T) {
				model := "global:deepseek-v4.1-flash"
				h, _, bodies, calls, _ := reasoningGuardFixtureSequence(t, []string{payload, payload}, model)
				recorder := httptest.NewRecorder()
				h.ServeHTTP(recorder, reasoningGuardRequest(path, model, streaming, false))
				assertReasoningGuardFailure(t, recorder, path, streaming)
				if *calls != 1 {
					t.Errorf("unexpected upstream calls=%d want=1", *calls)
				}
				for i, body := range bodies[:min(*calls, len(bodies))] {
					if body.closed == 0 {
						t.Errorf("upstream body %d was not closed", i)
					}
				}
			})
		}
	}
}
