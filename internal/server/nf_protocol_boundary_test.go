// ═══ 更新日志 ═══
// 2026-09-26：仅含密文的推理条目改为跳过（不再算「静默忽略能力」）。
// 2026-09-26：禁止并行时多余调用只交付第一个，不再整轮失败。
// 2026-09-25: 重放 NF 提前执行工具的真实模式，锁定整组校验、声明能力和无损推理历史边界。
package server

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func nfBoundaryRequest(t *testing.T, choice string, parallel bool, strict bool) *responsesRequest {
	t.Helper()
	body, _ := json.Marshal(map[string]any{
		"model": "global:hy3", "input": "read", "stream": true, "tool_choice": choice, "parallel_tool_calls": parallel,
		"tools": []any{map[string]any{"type": "function", "name": "Read", "strict": strict,
			"parameters": map[string]any{"type": "object", "properties": map[string]any{"path": map[string]any{"type": "string"}}, "required": []string{"path"}, "additionalProperties": false}}},
	})
	_, req, err := responsesToChat(body)
	if err != nil {
		t.Fatal(err)
	}
	return req
}

func nfBoundaryToolFrame(index int, name, arguments string) string {
	body, _ := json.Marshal(map[string]any{"id": "chat_nf", "choices": []any{map[string]any{"index": 0, "delta": map[string]any{"tool_calls": []any{map[string]any{"index": index, "id": "call_" + name + string(rune('a'+index)), "type": "function", "function": map[string]any{"name": name, "arguments": arguments}}}}, "finish_reason": nil}}})
	return string(body)
}

func nfAssertNoToolPayload(t *testing.T, body string) {
	t.Helper()
	for _, marker := range []string{`"type":"function_call"`, `"type":"custom_tool_call"`, `"type":"response.function_call_arguments.delta"`, `"tool_calls":[`, `"function_call":{`} {
		if strings.Contains(body, marker) {
			t.Fatalf("unvalidated executable tool payload escaped (%s)", marker)
		}
	}
}

func TestNFResponsesToolsWaitForWholeValidatedGroup(t *testing.T) {
	for _, tc := range []struct {
		name, choice, tool, args, suffix string
		parallel, strict, valid          bool
	}{
		{"valid", "auto", "Read", `{"path":"file.txt"}`, "", true, false, true},
		{"invalid suffix", "auto", "Read", `{"path":"file.txt"}`, "trailing", true, false, false},
		{"undeclared", "auto", "Write", `{"path":"file.txt"}`, "", true, false, false},
		{"undeclared required", "required", "Write", `{"path":"file.txt"}`, "", true, false, false},
		{"none", "none", "Read", `{"path":"file.txt"}`, "", true, false, false},
		{"strict", "auto", "Read", `{"path":3}`, "", true, true, false},
		{"null arguments", "auto", "Read", `null`, "", true, false, false},
		{"array arguments", "auto", "Read", `[]`, "", true, false, false},
		// 禁止并行而上游仍返回两个调用：只交付第一个，不再整轮失败（见 keepFirstToolCall）。
		{"parallel", "auto", "Read", `{"path":"file.txt"}`, "", false, false, true},
		{"duplicate identity", "auto", "Read", `{"path":"file.txt"}`, "", true, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			writer := newResponsesWriter(recorder, nfBoundaryRequest(t, tc.choice, tc.parallel, tc.strict))
			writer.Header().Set("Content-Type", "text/event-stream")
			_, _ = writer.Write([]byte("data: " + `{"choices":[{"index":0,"delta":{"content":"visible","reasoning_content":"thinking"}}]}` + "\n\n"))
			if !strings.Contains(recorder.Body.String(), "visible") || !strings.Contains(recorder.Body.String(), "thinking") {
				t.Fatal("text or reasoning was buffered")
			}
			_, _ = writer.Write([]byte("data: " + nfBoundaryToolFrame(0, tc.tool, tc.args) + "\n\n"))
			nfAssertNoToolPayload(t, recorder.Body.String())
			if tc.suffix != "" {
				_, _ = writer.Write([]byte("data: " + nfBoundaryToolFrame(0, tc.tool, tc.suffix) + "\n\n"))
			}
			if tc.name == "parallel" {
				_, _ = writer.Write([]byte("data: " + nfBoundaryToolFrame(1, "Read", `{"path":"another"}`) + "\n\n"))
			}
			if tc.name == "duplicate identity" {
				second := strings.ReplaceAll(nfBoundaryToolFrame(1, "Read", `{"path":"another"}`), "call_Readb", "call_Reada")
				_, _ = writer.Write([]byte("data: " + second + "\n\n"))
			}
			_, _ = writer.Write(sseStream(`{"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`))
			nfAssertNoToolPayload(t, recorder.Body.String())
			err := writer.CompletionError()
			writer.finish()
			if tc.valid {
				if err != nil || !strings.Contains(recorder.Body.String(), `"type":"function_call"`) {
					t.Fatalf("valid call lost: %v %s", err, recorder.Body)
				}
				if tc.name == "parallel" && strings.Contains(recorder.Body.String(), "another") {
					t.Fatalf("second parallel call delivered despite parallel_tool_calls=false: %s", recorder.Body)
				}
			} else {
				if err == nil {
					t.Fatal("invalid group was accepted")
				}
				nfAssertNoToolPayload(t, recorder.Body.String())
			}
		})
	}
}

func TestNFChatToolsWaitForWholeValidatedGroup(t *testing.T) {
	for _, valid := range []bool{true, false} {
		t.Run(map[bool]string{true: "valid", false: "late invalid suffix"}[valid], func(t *testing.T) {
			var fields map[string]json.RawMessage
			_ = json.Unmarshal([]byte(`{"model":"global:hy3","stream":true,"messages":[],"tools":[{"type":"function","function":{"name":"Read","parameters":{"type":"object"}}}],"tool_choice":"auto"}`), &fields)
			contract, err := newChatOutputContract(fields)
			if err != nil || contract == nil {
				t.Fatalf("ordinary auto tools must install the output gate: %v", err)
			}
			recorder := httptest.NewRecorder()
			writer := &chatContractWriter{inner: recorder, req: contract}
			writer.Header().Set("Content-Type", "text/event-stream")
			_, _ = writer.Write([]byte("data: " + `{"choices":[{"index":0,"delta":{"content":"visible","reasoning_content":"thinking"}}]}` + "\n\n"))
			if !strings.Contains(recorder.Body.String(), "visible") || !strings.Contains(recorder.Body.String(), "thinking") {
				t.Fatal("text or reasoning was buffered")
			}
			_, _ = writer.Write([]byte("data: " + nfBoundaryToolFrame(0, "Read", `{"path":"file.txt"}`) + "\n\n"))
			nfAssertNoToolPayload(t, recorder.Body.String())
			if !valid {
				_, _ = writer.Write([]byte("data: " + nfBoundaryToolFrame(0, "Read", "trailing") + "\n\n"))
			}
			_, _ = writer.Write(sseStream(`{"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`))
			nfAssertNoToolPayload(t, recorder.Body.String())
			err = writer.CompletionError()
			if valid {
				if err != nil || !strings.Contains(recorder.Body.String(), `"tool_calls":[`) {
					t.Fatalf("valid Chat call lost: %v %s", err, recorder.Body)
				}
			} else {
				if err == nil {
					t.Fatal("invalid Chat call was accepted")
				}
				nfAssertNoToolPayload(t, recorder.Body.String())
			}
		})
	}
}

func TestNFDefaultCodexDeclarationsRemainUsable(t *testing.T) {
	body := []byte(`{"model":"global:hy3","stream":true,"store":false,"input":"hi","tools":[{"type":"function","name":"Read","strict":false,"parameters":{"type":"object"}},{"type":"web_search"},{"type":"image_generation","output_format":"png","partial_images":2}],"tool_choice":"auto","parallel_tool_calls":true,"reasoning":{"effort":"high","summary":"auto"},"include":["reasoning.encrypted_content"],"text":{"verbosity":"low"}}`)
	chat, req, err := responsesToChat(body)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(chat), "image_generation") || strings.Contains(string(chat), "web_search") {
		t.Fatalf("unsupported tools reached upstream: %s", chat)
	}
	w := httptest.NewRecorder()
	warnIgnoredBuiltinTools(w, httptest.NewRequest("POST", "/v1/responses", nil), req.Tools)
	if !strings.Contains(w.Header().Get("X-WB2API-Ignored-Tools"), "image_generation") {
		t.Fatalf("ignored capability not disclosed: %v", w.Header())
	}
	for _, choice := range []string{`{"type":"image_generation"}`, `"required"`} {
		request := `{"model":"m","input":"draw","tools":[{"type":"image_generation"}],"tool_choice":` + choice + `}`
		if _, _, err := responsesToChat([]byte(request)); err == nil {
			t.Fatalf("forced image generation was accepted: %s", request)
		}
	}
}

func TestNFUnsupportedResponsesSemanticsAreExplicit(t *testing.T) {
	for _, body := range []string{
		`{"model":"m","input":"hi","truncation":"auto"}`,
		`{"model":"m","input":"hi","reasoning":{"effort":"high","context":"all_turns"}}`,
		`{"model":"m","input":"hi","reasoning":{"mode":"pro"}}`,
		// 仅含密文的推理项不在其列：它来自别的服务、上游也读不懂，跳过比让整个会话
		// 永久 400 更可用（见 TestReasoningTextlessEncryptedItemIsSkipped）。
	} {
		if _, _, err := responsesToChat([]byte(body)); err == nil {
			t.Fatalf("semantic capability silently ignored: %s", body)
		}
	}
	if _, _, err := responsesToChat([]byte(`{"model":"m","truncation":"disabled","input":[{"type":"reasoning","summary":[{"type":"summary_text","text":"keep thinking"}],"encrypted_content":"opaque"},{"role":"assistant","content":"answer"},{"role":"user","content":"hi"}]}`)); err != nil {
		t.Fatalf("readable reasoning history must remain usable: %v", err)
	}
}

func TestNFBufferedToolProgressAndLimits(t *testing.T) {
	for _, protocol := range []string{"responses", "chat"} {
		t.Run(protocol, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			req := nfBoundaryRequest(t, "auto", true, false)
			var write func([]byte) (int, error)
			var finish func() error
			var expireProgress func()
			if protocol == "responses" {
				w := newResponsesWriter(recorder, req)
				w.Header().Set("Content-Type", "text/event-stream")
				write, finish = w.Write, w.FinishResponse
				expireProgress = func() { w.lastWrite = time.Now().Add(-2 * protocolProgressInterval) }
			} else {
				w := &chatContractWriter{inner: recorder, req: req}
				w.Header().Set("Content-Type", "text/event-stream")
				write, finish = w.Write, w.FinishResponse
				expireProgress = func() { w.lastWrite = time.Now().Add(-2 * protocolProgressInterval) }
			}
			_, _ = write([]byte("data: " + nfBoundaryToolFrame(0, "Read", `{"path":"`) + "\n\n"))
			expireProgress()
			_, _ = write([]byte("data: " + nfBoundaryToolFrame(0, "Read", "fragment") + "\n\n"))
			if !strings.Contains(recorder.Body.String(), ": keepalive") {
				t.Fatal("buffering tools swallowed progress without a heartbeat")
			}
			nfAssertNoToolPayload(t, recorder.Body.String())
			chunk := []byte("data: " + nfBoundaryToolFrame(0, "Read", strings.Repeat("x", 1<<20)) + "\n\n")
			var err error
			for count := 0; count < 18; count++ {
				if _, err = write(chunk); err != nil {
					break
				}
			}
			if err == nil {
				t.Fatal("oversized buffered tool was not bounded")
			}
			if finish() == nil {
				t.Fatal("buffer failure was forgotten by finalization")
			}
			length := recorder.Body.Len()
			if finish() == nil || recorder.Body.Len() != length {
				t.Fatal("error finalization is not idempotent")
			}
			nfAssertNoToolPayload(t, recorder.Body.String())
		})
	}
}

func TestNFIncompleteJSONWithholdsToolsAndFinalizesOnce(t *testing.T) {
	req := nfBoundaryRequest(t, "auto", true, false)
	req.Stream = false
	chat := map[string]any{"choices": []any{map[string]any{"index": 0, "finish_reason": "length", "message": map[string]any{"role": "assistant", "content": "partial", "tool_calls": []any{map[string]any{"id": "c", "type": "function", "function": map[string]any{"name": "Read", "arguments": `{"path":"x"}`}}}}}}}
	for _, protocol := range []string{"responses", "chat"} {
		t.Run(protocol, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			if protocol == "responses" {
				writer := newResponsesWriter(recorder, req)
				writer.Header().Set("Content-Type", "application/json")
				body, _ := json.Marshal(chat)
				_, _ = writer.Write(body)
				if err := writer.FinishResponse(); err != nil {
					t.Fatal(err)
				}
				length := recorder.Body.Len()
				if err := writer.FinishResponse(); err != nil || recorder.Body.Len() != length {
					t.Fatal("JSON finalization is not idempotent")
				}
			} else {
				writer := &chatContractWriter{inner: recorder, req: req}
				writer.PrepareCompletion(chat)
				body, _ := json.Marshal(chat)
				_, _ = writer.Write(body)
			}
			if !strings.Contains(recorder.Body.String(), "partial") {
				t.Fatal("partial visible text was lost")
			}
			nfAssertNoToolPayload(t, recorder.Body.String())
		})
	}
}

func TestNFDiscardedToolsDoNotLeaveOutputIndexHoles(t *testing.T) {
	for _, ending := range []string{`{"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`, `{"choices":[{"index":0,"delta":{},"finish_reason":"length"}]}`, `{"error":{"code":"upstream_error","message":"failed"}}`} {
		recorder := httptest.NewRecorder()
		writer := newResponsesWriter(recorder, nfBoundaryRequest(t, "auto", true, false))
		writer.Header().Set("Content-Type", "text/event-stream")
		_, _ = writer.Write(sseStream(nfBoundaryToolFrame(0, "Read", `{"path":"x"}`), `{"choices":[{"index":0,"delta":{"content":"visible","reasoning_content":"thinking"}}]}`, ending))
		_ = writer.FinishResponse()
		names, events := eventsOf(t, recorder.Body.String())
		final := events[len(events)-1]["response"].(map[string]any)["output"].([]any)
		for i, name := range names {
			if name != evItemAdded {
				continue
			}
			index := int(events[i]["output_index"].(float64))
			if index >= len(final) || final[index].(map[string]any)["id"] != events[i]["item"].(map[string]any)["id"] {
				t.Fatalf("announced output_index %d no longer points at the final item", index)
			}
		}
	}
}
