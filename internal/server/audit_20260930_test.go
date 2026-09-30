// ═══ 更新日志 ═══
// 2026-09-30：深度体检确认缺陷的回归测试（Gemini/Anthropic/ActNote 三组）。
package server

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestGeminiZeroArgumentToolCallIsDelivered 无参数工具被调用时上游发
// arguments:""，这是上游层认可的合法形状（Chat/Responses 早已接受）。
// Gemini 此前用 jsonutil.Decode("") 判死（返回 EOF），整轮失败：
// 非流 502、流内 finishReason=OTHER。同一上游帧在别的协议都能交付。
//
// 2026-09-30 深度体检发现。
func TestGeminiZeroArgumentToolCallIsDelivered(t *testing.T) {
	reply := "data: {\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"type\":\"function\",\"function\":{\"name\":\"ping\",\"arguments\":\"\"}}]},\"finish_reason\":\"tool_calls\"}]}\n\ndata: [DONE]\n\n"
	input := `{"contents":[{"parts":[{"text":"ping"}]}],"tools":[{"functionDeclarations":[{"name":"ping"}]}]}`
	for _, action := range []string{"generateContent", "streamGenerateContent"} {
		t.Run(action, func(t *testing.T) {
			h, _, calls, _ := messagesFixture(t, reply)
			w := httptest.NewRecorder()
			geminiTestServe(h, w, action, input)
			if *calls == 0 {
				t.Fatal("请求未到达上游")
			}
			if w.Code != 200 {
				t.Fatalf("无参数工具调用被拒: status=%d body=%s", w.Code, w.Body.String())
			}
			if !strings.Contains(w.Body.String(), "functionCall") {
				t.Fatalf("响应缺少 functionCall: %s", w.Body.String())
			}
		})
	}
}

// TestAnthropicZeroArgumentToolCallIsDelivered Anthropic 路径的同类问题：
// messagesTool.input() 对空参数同样返回 EOF 错误。审计只报了 Gemini，
// 复核发现 Messages 也中招（同一上游帧在这里同样整轮失败）。
func TestAnthropicZeroArgumentToolCallIsDelivered(t *testing.T) {
	reply := "data: {\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"type\":\"function\",\"function\":{\"name\":\"ping\",\"arguments\":\"\"}}]},\"finish_reason\":\"tool_calls\"}]}\n\ndata: [DONE]\n\n"
	h, _, calls, _ := messagesFixture(t, reply)
	rec := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(`{"model":"cn:fixture","stream":true,"max_tokens":64,"messages":[{"role":"user","content":"ping"}],"tools":[{"name":"ping","input_schema":{"type":"object","properties":{}}}]}`))
	r.Header.Set("X-API-Key", "fixture-key")
	h.ServeHTTP(rec, r)
	_ = calls
	body := rec.Body.String()
	if rec.Code != 200 || strings.Contains(body, "incomplete or invalid tool arguments") {
		t.Fatalf("无参数工具调用被拒: status=%d body=%s", rec.Code, body)
	}
	if !strings.Contains(body, "tool_use") {
		t.Fatalf("响应缺少 tool_use 块: %s", body)
	}
}

// TestGeminiLegacyFunctionCallFinishReason function_call 是上游 validFinishReason
// 接受的旧式结束原因，Anthropic 与 Chat 都认得；Gemini 此前只映射 stop/tool_calls，
// 于是已完整成功的一轮被报成失败（非流 502 / 流内 OTHER）。
//
// 2026-09-30 深度体检发现。
func TestGeminiLegacyFunctionCallFinishReason(t *testing.T) {
	reply := "data: {\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"type\":\"function\",\"function\":{\"name\":\"read\",\"arguments\":\"{\\\"id\\\":1}\"}}]},\"finish_reason\":\"function_call\"}]}\n\ndata: [DONE]\n\n"
	input := `{"contents":[{"parts":[{"text":"go"}]}],"tools":[{"functionDeclarations":[{"name":"read","parameters":{"type":"OBJECT","properties":{"id":{"type":"INTEGER"}}}}]}]}`
	for _, action := range []string{"generateContent", "streamGenerateContent"} {
		t.Run(action, func(t *testing.T) {
			h, _, _, _ := messagesFixture(t, reply)
			w := httptest.NewRecorder()
			geminiTestServe(h, w, action, input)
			if w.Code != 200 {
				t.Fatalf("function_call 结束原因被拒: status=%d body=%s", w.Code, w.Body.String())
			}
			if !strings.Contains(w.Body.String(), "functionCall") {
				t.Fatalf("响应缺少 functionCall: %s", w.Body.String())
			}
		})
	}
}

// TestGeminiThinkingBudgetAutomaticIsAccepted -1 是官方 SDK 的 AUTOMATIC，
// 也是文档承诺支持的取值；通用数值校验器要求非负整数，抢在区间检查前把它拒掉，
// 按文档使用的客户端直接 400 且请求到不了上游。
//
// 2026-09-30 深度体检发现。
func TestGeminiThinkingBudgetAutomaticIsAccepted(t *testing.T) {
	for _, budget := range []string{"-1", "0"} {
		t.Run(budget, func(t *testing.T) {
			h, _, calls, _ := messagesFixture(t, sseOK)
			w := httptest.NewRecorder()
			body := `{"contents":[{"parts":[{"text":"x"}]}],"generationConfig":{"thinkingConfig":{"thinkingBudget":` + budget + `}}}`
			geminiTestServe(h, w, "generateContent", body)
			if w.Code != 200 || *calls == 0 {
				t.Fatalf("thinkingBudget=%s 被拒: status=%d calls=%d body=%s", budget, w.Code, *calls, w.Body.String())
			}
		})
	}
	// 越界值仍须拒绝，且不能到上游。
	h, _, calls, _ := messagesFixture(t, sseOK)
	w := httptest.NewRecorder()
	geminiTestServe(h, w, "generateContent", `{"contents":[{"parts":[{"text":"x"}]}],"generationConfig":{"thinkingConfig":{"thinkingBudget":1024}}}`)
	if w.Code != 400 || *calls != 0 {
		t.Fatalf("越界 thinkingBudget 应被拒: status=%d calls=%d", w.Code, *calls)
	}
}

// TestGeminiNullableUnionSchemaIsAccepted @google/genai 会为可空联合发
// {"nullable":true,"anyOf":[…]}、为可空枚举发 {"nullable":true,"enum":[…]}。
// 此前要求「单一非空 type」，把 SDK 正常产出的合法 schema 一律 400。
//
// 2026-09-30 深度体检发现。
func TestGeminiNullableUnionSchemaIsAccepted(t *testing.T) {
	for _, schema := range []string{
		`{"nullable":true,"anyOf":[{"type":"STRING"},{"type":"NUMBER"}]}`,
		`{"nullable":true,"enum":["a","b"]}`,
		`{"type":"STRING","nullable":true}`,
	} {
		t.Run(schema, func(t *testing.T) {
			h, _, calls, _ := messagesFixture(t, sseOK)
			w := httptest.NewRecorder()
			body := `{"contents":[{"parts":[{"text":"x"}]}],"tools":[{"functionDeclarations":[{"name":"read","parameters":` + schema + `}]}]}`
			geminiTestServe(h, w, "generateContent", body)
			if w.Code != 200 || *calls == 0 {
				t.Fatalf("可空 schema 被拒: schema=%s status=%d calls=%d body=%s", schema, w.Code, *calls, w.Body.String())
			}
		})
	}
}

// TestMessagesErrorToolResultOmitsEmptyTextPart tool_result 标记 is_error 且正文为空时，
// 此前会发 `[{text:"[tool execution error]"},{text:""}]`——正是本仓库注释点名要避免的
// 上游 400 形状（空 text part）。
//
// 2026-09-30 深度体检发现。
func TestMessagesErrorToolResultOmitsEmptyTextPart(t *testing.T) {
	body := `{"model":"cn:fixture","max_tokens":64,"messages":[
		{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"read","input":{}}]},
		{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","is_error":true,"content":""}]}]}`
	_, _, _, err := messagesToChat([]byte(body))
	if err != nil {
		t.Fatalf("转换失败: %v", err)
	}
	chat, _, _, err := messagesToChat([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(chat, &out); err != nil {
		t.Fatal(err)
	}
	for _, raw := range out["messages"].([]any) {
		message, _ := raw.(map[string]any)
		if message["role"] != "tool" {
			continue
		}
		parts, ok := message["content"].([]any)
		if !ok {
			continue
		}
		for _, rawPart := range parts {
			part, _ := rawPart.(map[string]any)
			if part["type"] != "text" {
				continue
			}
			if text, _ := part["text"].(string); strings.TrimSpace(text) == "" {
				t.Fatalf("转出了空 text part（上游 400 形状）: %v", parts)
			}
		}
	}
}
