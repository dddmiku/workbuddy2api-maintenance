// ═══ 更新日志 ═══
// 2026-09-25：通过内容入口验证 Gemini 转换保留正文、模型路由和真实推理用量。
package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func geminiTestServe(h *Handler, w http.ResponseWriter, action, body string) {
	target := "/v1beta/models/cn:fixture:" + action
	if action == "streamGenerateContent" {
		target += "?alt=sse"
	}
	r := httptest.NewRequest("POST", target, strings.NewReader(body))
	r.SetPathValue("modelAction", "cn:fixture:"+action)
	r.Header.Set("Authorization", "Bearer fixture-key")
	gw := newGeminiWriter(w)
	h.withAuth(h.withDecodedRequest(h.geminiContent))(gw, r)
	gw.finish()
}

func TestGeminiToolHistoryAndParallelOutput(t *testing.T) {
	reply := "data: {\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"next_a\",\"type\":\"function\",\"function\":{\"name\":\"read\",\"arguments\":\"{\\\"id\\\":9007199254740993}\"}},{\"index\":1,\"id\":\"next_b\",\"type\":\"function\",\"function\":{\"name\":\"read\",\"arguments\":\"{\\\"id\\\":2}\"}}]},\"finish_reason\":\"tool_calls\"}]}\n\ndata: [DONE]\n\n"
	input := `{"systemInstruction":{"parts":[{"text":"keep all results"}]},"contents":[{"role":"user","parts":[{"text":"read both"}]},{"role":"model","parts":[{"functionCall":{"name":"read","args":{"id":9007199254740993}}},{"functionCall":{"name":"read","args":{"id":2}}}]},{"role":"user","parts":[{"functionResponse":{"name":"read","response":{"type":"file","id":9007199254740993,"output":{"text":"business"}}}},{"functionResponse":{"name":"read","response":{"error":"failed"}}},{"text":"read again"}]}],"tools":[{"functionDeclarations":[{"name":"read","parameters":{"type":"OBJECT","properties":{"id":{"type":"INTEGER"}},"required":["id"]}}]}]}`
	for _, action := range []string{"generateContent", "streamGenerateContent"} {
		t.Run(action, func(t *testing.T) {
			h, out, calls, _ := messagesFixture(t, reply)
			w := httptest.NewRecorder()
			geminiTestServe(h, w, action, input)
			if w.Code != 200 || *calls != 1 || strings.Contains(w.Body.String(), `"error"`) {
				t.Fatalf("NF function history rejected: %d %d %s", w.Code, *calls, w.Body.String())
			}
			if strings.Count(w.Body.String(), `"functionCall"`) != 2 || !strings.Contains(w.Body.String(), `9007199254740993`) {
				t.Fatalf("tools duplicated, omitted or rounded: %s", w.Body.String())
			}
			messages := (*out)["messages"].([]any)
			toolMessages := []map[string]any{}
			for _, raw := range messages {
				m := raw.(map[string]any)
				if m["role"] == "tool" {
					toolMessages = append(toolMessages, m)
				}
			}
			if len(toolMessages) != 2 || toolMessages[0]["tool_call_id"] == toolMessages[1]["tool_call_id"] {
				t.Fatalf("same-name tool results not paired: %#v", toolMessages)
			}
			encoded, _ := json.Marshal(toolMessages[0])
			if !strings.Contains(string(encoded), `9007199254740993`) || !strings.Contains(string(encoded), `business`) || !strings.Contains(string(encoded), `file`) {
				t.Fatalf("business result was interpreted as media or rounded: %s", encoded)
			}
		})
	}
}

func TestGeminiGenerateContentTextAndUsage(t *testing.T) {
	for _, action := range []string{"generateContent", "streamGenerateContent"} {
		t.Run(action, func(t *testing.T) {
			h, out, calls, _ := messagesFixture(t, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hello\"},\"finish_reason\":null}]}\n\ndata: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":100,\"completion_tokens\":7,\"prompt_cache_hit_tokens\":60,\"completion_tokens_details\":{\"reasoning_tokens\":2}}}\n\ndata: [DONE]\n\n")
			w := httptest.NewRecorder()
			geminiTestServe(h, w, action, `{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`)
			if w.Code != 200 || *calls != 1 || (*out)["model"] != "fixture" {
				t.Fatalf("request did not share Chat routing: status=%d calls=%d outbound=%v body=%s", w.Code, *calls, *out, w.Body.String())
			}
			for _, expected := range []string{`"text":"hello"`, `"finishReason":"STOP"`, `"promptTokenCount":100`, `"cachedContentTokenCount":60`, `"candidatesTokenCount":5`, `"thoughtsTokenCount":2`, `"totalTokenCount":107`} {
				if !strings.Contains(w.Body.String(), expected) {
					t.Fatalf("missing %s: %s", expected, w.Body.String())
				}
			}
			if action == "streamGenerateContent" && w.Header().Get("Content-Type") != "text/event-stream" {
				t.Fatalf("NF requires an actual SSE stream: %v", w.Header())
			}
		})
	}
}

func TestGeminiGenerationConfigThoughtsAndInlineImage(t *testing.T) {
	reply := "data: {\"choices\":[{\"index\":0,\"delta\":{\"reasoning_content\":\"reason\",\"content\":\"{\\\"ok\\\":true}\"},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":21,\"completion_tokens\":9}}\n\ndata: [DONE]\n\n"
	body := `{"contents":[{"role":"user","parts":[{"inlineData":{"mimeType":"image/png","data":"YWJj"}},{"text":"look"}]},{"role":"model","parts":[{"thought":true,"text":"old thought"},{"text":"old reply"}]},{"role":"user","parts":[{"text":"return json"}]}],"generationConfig":{"candidateCount":1,"maxOutputTokens":200,"temperature":0.5,"topP":0.8,"topK":10,"stopSequences":["END"],"thinkingConfig":{"includeThoughts":true,"thinkingLevel":"high"},"responseMimeType":"application/json","responseSchema":{"type":"OBJECT","properties":{"ok":{"type":"BOOLEAN"}},"required":["ok"]}}}`
	h, out, calls, _ := messagesFixture(t, reply)
	w := httptest.NewRecorder()
	geminiTestServe(h, w, "streamGenerateContent", body)
	if w.Code != 200 || *calls != 1 || strings.Contains(w.Body.String(), `"error"`) {
		t.Fatalf("generation config rejected: %d %d %s", w.Code, *calls, w.Body.String())
	}
	for _, expected := range []string{`"thought":true`, `"text":"reason"`, `"candidatesTokenCount":9`, `"candidatesIncludeThoughts":true`, `"totalTokenCount":30`} {
		if !strings.Contains(w.Body.String(), expected) {
			t.Fatalf("missing %s: %s", expected, w.Body.String())
		}
	}
	encoded, _ := json.Marshal(*out)
	for _, expected := range []string{"data:image/png;base64,YWJj", "old thought", "json_schema", `"reasoning_effort":"high"`, `"max_tokens":200`} {
		if !strings.Contains(string(encoded), expected) {
			t.Fatalf("lost generation or history semantics %s: %s", expected, encoded)
		}
	}
}

func TestGeminiToolChoiceRestrictsAllowedFunctions(t *testing.T) {
	reply := "data: {\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"one\",\"type\":\"function\",\"function\":{\"name\":\"read\",\"arguments\":\"{}\"}}]},\"finish_reason\":\"tool_calls\"}]}\n\ndata: [DONE]\n\n"
	body := `{"contents":[{"role":"user","parts":[{"text":"read"}]}],"tools":[{"functionDeclarations":[{"name":"read"},{"name":"write"}]}],"toolConfig":{"functionCallingConfig":{"mode":"ANY","allowedFunctionNames":["read"]}}}`
	h, out, calls, _ := messagesFixture(t, reply)
	w := httptest.NewRecorder()
	geminiTestServe(h, w, "streamGenerateContent", body)
	if w.Code != 200 || *calls != 1 || strings.Contains(w.Body.String(), `"error"`) {
		t.Fatalf("tool choice rejected: %d %d %s", w.Code, *calls, w.Body.String())
	}
	tools := (*out)["tools"].([]any)
	if len(tools) != 1 || tools[0].(map[string]any)["function"].(map[string]any)["name"] != "read" {
		t.Fatalf("allowed functions were not enforced: %#v", *out)
	}
}
