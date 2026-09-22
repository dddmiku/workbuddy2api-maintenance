// ═══ 更新日志 ═══
// 2026-09-23：新增「两行极短行交替」的 handler 级回归。线上截图里的正文是「好。」与
//
//	「执行。」严格交替：窗口内只有 2 种短行、覆盖率 100%，但两行各占 50%，过不了
//	「一行占多数」的兜底，旧规则下既不停止也不重发，网页被数千行重复文本顶死。
//
// 2026-09-22：开关改为运行期状态，补管理通道热切换回归：改完立即作用于后续请求，
//
//	非法请求体被拒绝，且该端点只对本机管理通道开放。
//
// 2026-09-22：补「单项停止」开关回归：打开时命中循环只停不重发并如实报错，默认仍
//
//	同账号重发一次保持用户无感；两种模式都不改账号健康与粘性绑定。
//
// 2026-09-20：正文重复短行纳入保护：补「正文循环同账号重发、用户无感」回归，并把
//
//	「重复正文放行」的旧断言改为按闸门边界断言（长行立即放行才不重发）。
//
// 2026-09-19：以合成上游覆盖重复推理保护的四种出口、失败用量及账号/粘性边界。
// 2026-09-19：按32字符短行规则补齐完整早期用量、关闭开关与正常进展边界，避免误截和用量推算。
// 2026-09-20：改为可重发夹具，并补「重发成功用户无感」与「已出正文不再重发」两条回归。
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

// reasoningGuardFixtureSequence 让每次上游调用拿到**独立的**响应体：重复推理保护命中后
// 网关会在同一账号上重发，重发必须能读到一份全新的 SSE，否则测的只是"读同一个已消费
// 的 body"。payloads 用完后重复最后一项（便于构造"一直循环"的场景）。
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
					// 上游每次都给同一段循环输出：网关应当在同一账号上重发一次，
					// 重发仍然循环才如实回报错误。
					h, ledger, bodies, calls, bindings := reasoningGuardFixtureSequence(t, []string{payload}, model)
					beforeFirst, _ := h.cfg.Pool.Status("guard-first")
					beforeSecond, _ := h.cfg.Pool.Status("guard-second")
					recorder := httptest.NewRecorder()
					h.ServeHTTP(recorder, reasoningGuardRequest(path, model, streaming, false))
					assertReasoningGuardFailureStatus(t, recorder, path, streaming, http.StatusUnprocessableEntity)
					if *calls != 2 {
						t.Errorf("loop should retry exactly once on the same account: calls=%d", *calls)
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
					// 账本按「一次请求」记账：重发不额外计请求数，但两次尝试里已知的用量
					// 都会累计（失败/中断也保留已返回的数据）。
					want := usage.Totals{Requests: 1, FailedRequests: 1, UnreportedRequests: 1}
					if earlyUsage != "none" {
						want.PromptTokens = 240
						want.CachedTokens = 128
						want.TotalTokens = 240
						want.Credit = 1.0
					}
					if earlyUsage == "complete" {
						want.CompletionTokens = 16
						want.TotalTokens = 256
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
// 也不重试。现在必须在同账号重发一次，重发仍循环时用正文侧错误码如实回报。
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
				if *calls != 2 {
					t.Errorf("content loop should retry exactly once on the same account: calls=%d", *calls)
				}
				if uid, ok := bindings.lastUID("reasoning-guard-session"); !ok || uid != "guard-first" {
					t.Error("content loop guard invalidated the existing sticky binding")
				}
				// 正文循环的错误里只能有计数，不能把重复正文回显给调用方。
				if strings.Contains(recorder.Body.String(), "我执行") {
					t.Error("content loop error exposed output text")
				}
			})
		}
	}
}

// TestOutputLoopGuardRetriesTinyAlternatingContent 复现线上漏检形态：正文是「好。」与
// 「执行。」严格交替。第一次上游循环必须被拦住并在同账号重发，客户端最终只看到第二次
// 的干净输出；用户不会先收到几千行重复文本、更不会把网页顶死。
func TestOutputLoopGuardRetriesTinyAlternatingContent(t *testing.T) {
	var alternating strings.Builder
	for index := 0; index < 400; index++ {
		if index%2 == 0 {
			alternating.WriteString("好。\n")
		} else {
			alternating.WriteString("执行。\n")
		}
	}
	loopPayload := reasoningGuardFinish(
		reasoningGuardFrame(map[string]any{"content": alternating.String()}), "stop")
	cleanPayload := reasoningGuardFinish(
		reasoningGuardFrame(map[string]any{"content": "tiny-cycle-clean-marker"}), "stop")
	for _, path := range []string{"/v1/chat/completions", "/v1/responses"} {
		for _, streaming := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%t", path, streaming), func(t *testing.T) {
				model := "global:deepseek-v4.1-flash"
				h, _, _, calls, bindings := reasoningGuardFixtureSequence(
					t, []string{loopPayload, cleanPayload}, model)
				recorder := httptest.NewRecorder()
				h.ServeHTTP(recorder, reasoningGuardRequest(path, model, streaming, false))
				output := recorder.Body.String()
				if recorder.Code != http.StatusOK || !strings.Contains(output, "tiny-cycle-clean-marker") {
					t.Fatalf("tiny alternating loop was not retried into a clean success: status=%d", recorder.Code)
				}
				if strings.Contains(output, `"code":"upstream_output_loop"`) {
					t.Error("client received a loop error even though the retry succeeded")
				}
				if *calls != 2 {
					t.Errorf("tiny alternating loop was not retried on the same account: calls=%d", *calls)
				}
				if uid, ok := bindings.lastUID("reasoning-guard-session"); !ok || uid != "guard-first" {
					t.Error("tiny alternating loop invalidated the existing sticky binding")
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
	// 重置类用例的每段推理都必须短于检测窗口：这样单独一段永远凑不满窗口，
	// 「进展会重置窗口」才是唯一的阻断因素。若用满窗口的重复行，保护会在
	// 进展事件到达之前就命中，测的就不是重置语义了。
	// 这里写死一个远小于 upstream 包窗口长度（32）的值，避免把内部常量暴露成 API。
	const half = 8
	before := reasoningGuardFrame(map[string]any{"reasoning_content": reasoningGuardLines(half)})
	after := reasoningGuardFrame(map[string]any{"reasoning_content": reasoningGuardLines(half)})
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
					// 上游两次都给同一段循环：网关在同账号重发一次，仍循环才回报。
					h, ledger, bodies, calls, _ := reasoningGuardFixtureSequence(t, []string{payload, payload}, model)
					recorder := httptest.NewRecorder()
					h.ServeHTTP(recorder, reasoningGuardRequest(path, model, streaming, tc.tools))
					assertReasoningGuardFailure(t, recorder, path, streaming)
					// 工具进展一旦写出去，客户端就不再是零字节，闸门放行后命中只能照旧
					// 回报；其余场景客户端零字节，应当同账号重发一次。
					wantCalls := 2
					if streaming && tc.prefix != "" {
						wantCalls = 1
					}
					if *calls != wantCalls {
						t.Errorf("unexpected upstream calls=%d want=%d", *calls, wantCalls)
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
					wantCalls := 2
					if model == "cn:deepseek-v4.1-flash-extra" || model == "cn:gpt-5.3-codex" {
						assertReasoningGuardSuccess(t, recorder, path, streaming, ledger)
						wantCalls = 1
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

// TestReasoningLoopGuardRetryIsInvisible 验证用户无感：第一次上游循环、第二次正常，
// 客户端只看到第二次的成功输出，既不出现循环那段推理，也没有任何错误标记。
func TestReasoningLoopGuardRetryIsInvisible(t *testing.T) {
	loopPayload := reasoningGuardFinish(reasoningGuardFrame(map[string]any{"reasoning_content": reasoningGuardLines(400)}), "stop")
	cleanPayload := reasoningGuardFinish(reasoningGuardFrame(map[string]any{"content": "after-guard-marker"}), "stop")
	outputLoopPayload := reasoningGuardFinish(
		reasoningGuardFrame(map[string]any{"content": strings.Repeat("我执行。\n", 400)}), "stop")
	for _, path := range []string{"/v1/chat/completions", "/v1/responses"} {
		for _, streaming := range []bool{false, true} {
			for _, tc := range []struct{ name, loop string }{
				{"reasoning", loopPayload},
				{"content", outputLoopPayload},
			} {
				t.Run(fmt.Sprintf("%s/stream=%t/%s", path, streaming, tc.name), func(t *testing.T) {
					model := "global:deepseek-v4.1-flash"
					h, ledger, bodies, calls, _ := reasoningGuardFixtureSequence(t, []string{tc.loop, cleanPayload}, model)
					recorder := httptest.NewRecorder()
					h.ServeHTTP(recorder, reasoningGuardRequest(path, model, streaming, false))
					output := recorder.Body.String()
					if recorder.Code != http.StatusOK || strings.Contains(output, `"code":"upstream_reasoning_loop"`) ||
						strings.Contains(output, `"code":"upstream_output_loop"`) ||
						!strings.Contains(output, "after-guard-marker") {
						t.Fatalf("retry did not deliver a clean success: status=%d", recorder.Code)
					}
					if path == "/v1/responses" && streaming && (strings.Count(output, "event: response.completed\n") != 1 ||
						strings.Contains(output, "event: response.failed\n")) {
						t.Error("retry did not emit exactly one successful Responses terminal")
					}
					// 被丢弃的第一轮上游确实计了费，只是没能读到完整 usage，
					// 因此这一笔请求如实标记为「用量不完整」，不能伪装成完整账单。
					if got := ledger.Snapshot().Totals.UnreportedRequests; got != 1 {
						t.Errorf("discarded attempt should be flagged as unreported usage: %d", got)
					}
					if *calls != 2 {
						t.Errorf("loop was not retried on the same account: calls=%d", *calls)
					}
					if strings.Contains(output, "checking the same step once more") {
						t.Error("client received the discarded first attempt")
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

// TestReasoningLoopFeatureToggleIsLive 覆盖管理台的热切换通道：默认（配置播种）为
// 「命中后重发」，通过管理接口改成只停不重发后**下一次请求**立刻生效，再改回来也
// 立即恢复。这条回归锁的是「改完必须立刻作用于后续请求」，而不是「重启后生效」。
func TestReasoningLoopFeatureToggleIsLive(t *testing.T) {
	loopPayload := reasoningGuardFinish(reasoningGuardFrame(map[string]any{"reasoning_content": reasoningGuardLines(400)}), "stop")
	cleanPayload := reasoningGuardFinish(reasoningGuardFrame(map[string]any{"content": "after-guard-marker"}), "stop")
	model := "global:deepseek-v4.1-flash"

	h, _, _, calls, _ := reasoningGuardFixtureSequence(t, []string{loopPayload, cleanPayload}, model)
	if h.reasoningLoopStopOnly() {
		t.Fatal("default should keep the same-account retry")
	}

	// 管理通道：打开只停不重发。
	recorder := httptest.NewRecorder()
	h.InternalHandler().ServeHTTP(recorder,
		httptest.NewRequest(http.MethodPost, "/features/reasoning-loop", strings.NewReader(`{"stop_only":true}`)))
	if recorder.Code != http.StatusOK || !h.reasoningLoopStopOnly() {
		t.Fatalf("toggle did not apply: status=%d body=%s", recorder.Code, recorder.Body.String())
	}

	// 下一次请求必须只调上游一次并如实报错。
	recorder = httptest.NewRecorder()
	h.ServeHTTP(recorder, reasoningGuardRequest("/v1/chat/completions", model, false, false))
	assertReasoningGuardFailure(t, recorder, "/v1/chat/completions", false)
	if *calls != 1 {
		t.Errorf("stop-only must not retry: calls=%d", *calls)
	}

	// 改回重发，下一次请求恢复「第一次循环被丢弃、第二次干净输出」。
	recorder = httptest.NewRecorder()
	h.InternalHandler().ServeHTTP(recorder,
		httptest.NewRequest(http.MethodPost, "/features/reasoning-loop", strings.NewReader(`{"stop_only":false}`)))
	if recorder.Code != http.StatusOK || h.reasoningLoopStopOnly() {
		t.Fatalf("toggle back did not apply: status=%d", recorder.Code)
	}
	recorder = httptest.NewRecorder()
	h.ServeHTTP(recorder, reasoningGuardRequest("/v1/chat/completions", model, false, false))
	if output := recorder.Body.String(); !strings.Contains(output, "after-guard-marker") ||
		strings.Contains(output, `"code":"upstream_reasoning_loop"`) {
		t.Fatalf("retry did not resume after toggling back: status=%d", recorder.Code)
	}
}

// TestReasoningLoopFeatureToggleInputAndScope 锁定这个端点的两道边界：请求体必须显式
// 给布尔值（空体、字符串、多余字段都拒绝），并且只能从本机管理通道访问——普通调用
// 密钥不能改所有调用方看到的行为。
func TestReasoningLoopFeatureToggleInputAndScope(t *testing.T) {
	h := NewHandler(Config{})
	admin := h.InternalHandler()
	for _, tc := range []struct{ name, body string }{
		{"empty", `{}`},
		{"string", `{"stop_only":"true"}`},
		{"number", `{"stop_only":1}`},
		{"null", `{"stop_only":null}`},
		{"unknown_field", `{"stop_only":true,"other":1}`},
		{"not_json", `nope`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			admin.ServeHTTP(recorder,
				httptest.NewRequest(http.MethodPost, "/features/reasoning-loop", strings.NewReader(tc.body)))
			if recorder.Code != http.StatusBadRequest {
				t.Errorf("invalid body accepted: status=%d body=%s", recorder.Code, recorder.Body.String())
			}
			if h.reasoningLoopStopOnly() {
				t.Error("invalid body changed the runtime switch")
			}
		})
	}
	// 公开 mux（没有管理上下文）必须拿不到这个端点。
	recorder := httptest.NewRecorder()
	h.ServeHTTP(recorder,
		httptest.NewRequest(http.MethodPost, "/features/reasoning-loop", strings.NewReader(`{"stop_only":true}`)))
	if recorder.Code == http.StatusOK || h.reasoningLoopStopOnly() {
		t.Errorf("public path reached the internal toggle: status=%d", recorder.Code)
	}
	// GET 读回当前值，同样只走管理通道。
	recorder = httptest.NewRecorder()
	admin.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/features/reasoning-loop", nil))
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), `"stop_only":false`) {
		t.Errorf("GET did not report the current value: status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}

// TestReasoningLoopGuardNoRetryAfterVisibleOutput 验证闸门边界：正文一旦被判定为正常
// 并写出就收不回，此后推理命中循环只能如实回报错误，不能再重发；非流式客户端在读完
// 前零字节，重发仍然无感，因此照常重试一次。
//
// 夹具里的正文是超过 32 字符的长行，正文闸门立即放行，因此这里考察的是「已放行之后
// 才命中」的路径。正文本身构成循环的情形由 upstream 包的正文保护回归覆盖。
func TestReasoningLoopGuardNoRetryAfterVisibleOutput(t *testing.T) {
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
				wantCalls := 2
				if streaming {
					wantCalls = 1
				}
				if *calls != wantCalls {
					t.Errorf("unexpected upstream calls=%d want=%d", *calls, wantCalls)
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

// TestReasoningLoopGuardStopOnlyDisablesRetry 验证「单项停止」开关：打开后命中循环
// 只调上游一次，不再同账号重发，并把循环如实回报给客户端。默认（关闭）仍保持用户
// 无感的重发行为，两种模式都不改变账号健康与粘性绑定。
func TestReasoningLoopGuardStopOnlyDisablesRetry(t *testing.T) {
	loopPayload := reasoningGuardFinish(reasoningGuardFrame(map[string]any{"reasoning_content": reasoningGuardLines(400)}), "stop")
	cleanPayload := reasoningGuardFinish(reasoningGuardFrame(map[string]any{"content": "after-guard-marker"}), "stop")
	for _, path := range []string{"/v1/chat/completions", "/v1/responses"} {
		for _, streaming := range []bool{false, true} {
			for _, stopOnly := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/stream=%t/stopOnly=%t", path, streaming, stopOnly), func(t *testing.T) {
					model := "global:deepseek-v4.1-flash"
					h, _, bodies, calls, bindings := reasoningGuardFixtureSequence(t, []string{loopPayload, cleanPayload}, model)
					// 开关是运行期状态（管理台可热切换），必须走 setter 而不是直接改 cfg。
					h.SetReasoningLoopStopOnly(stopOnly)
					beforeFirst, _ := h.cfg.Pool.Status("guard-first")
					beforeSecond, _ := h.cfg.Pool.Status("guard-second")
					recorder := httptest.NewRecorder()
					h.ServeHTTP(recorder, reasoningGuardRequest(path, model, streaming, false))
					if stopOnly {
						// 只停不重发：客户端必须收到明确的循环错误，而不是静默结束。
						assertReasoningGuardFailure(t, recorder, path, streaming)
						if *calls != 1 {
							t.Errorf("stop-only mode must not retry upstream: calls=%d", *calls)
						}
					} else {
						// 默认模式：第一次循环被丢弃，第二次干净输出，用户无感。
						output := recorder.Body.String()
						if recorder.Code != http.StatusOK || strings.Contains(output, `"code":"upstream_reasoning_loop"`) ||
							!strings.Contains(output, "after-guard-marker") {
							t.Fatalf("default mode did not retry into a clean success: status=%d", recorder.Code)
						}
						if *calls != 2 {
							t.Errorf("default mode should retry exactly once: calls=%d", *calls)
						}
					}
					afterFirst, firstPresent := h.cfg.Pool.Status("guard-first")
					afterSecond, secondPresent := h.cfg.Pool.Status("guard-second")
					if !firstPresent || !secondPresent {
						t.Fatal("loop guard removed an account from the pool")
					}
					if stopOnly {
						// 只停不重发时，循环不算账号错误，健康度与计数必须原样。
						assertReasoningGuardAccountUnchanged(t, beforeFirst, afterFirst)
						assertReasoningGuardAccountUnchanged(t, beforeSecond, afterSecond)
					} else {
						// 默认模式重发成功后按正常成功路径记账，只要求账号没有被熔断或禁用。
						if afterFirst.Disabled || afterFirst.Cooling || afterSecond.Disabled || afterSecond.Cooling ||
							afterFirst.BreakerFails != 0 || afterSecond.BreakerFails != 0 {
							t.Errorf("loop retry marked an account unhealthy: first=%+v second=%+v", afterFirst, afterSecond)
						}
					}
					if uid, ok := bindings.lastUID("reasoning-guard-session"); !ok || uid != "guard-first" {
						t.Error("stop-only switch invalidated the existing sticky binding")
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
