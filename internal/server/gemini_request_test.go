// ═══ 更新日志 ═══
// 2026-09-25：非法 Gemini 请求与未实现平台能力在上游消费前拒绝，工具输出仍受原有合同检查。
package server

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestGeminiRequestInvalidInputsNeverReachUpstream(t *testing.T) {
	for _, body := range []string{
		`{"contents":[]}`,
		`{"contents":[{"role":"assistant","parts":[{"text":"bad role"}]}]}`,
		`{"contents":[{"parts":[{"text":7}]}]}`,
		`{"contents":[{"parts":[{"text":"x","functionCall":{"name":"read","args":{}}}]}]}`,
		`{"contents":[{"parts":[{"functionResponse":{"name":"read","response":{"output":"orphan"}}}]}]}`,
		`{"contents":[{"role":"model","parts":[{"functionCall":{"id":"one","name":"read","args":{}}}]}]}`,
		`{"contents":[{"role":"model","parts":[{"functionCall":{"id":"same","name":"read","args":{}}},{"functionCall":{"id":"same","name":"read","args":{}}}]}]}`,
		`{"contents":[{"role":"model","parts":[{"functionCall":{"id":"one","name":"read","args":[]}}]}]}`,
		`{"contents":[{"role":"model","parts":[{"functionCall":{"id":"one","name":"read","args":{}}}]},{"parts":[{"functionResponse":{"id":"one","name":"write","response":{}}}]}]}`,
		`{"contents":[{"role":"model","parts":[{"thought":true,"thoughtSignature":"opaque","text":"old"}]},{"parts":[{"text":"next"}]}]}`,
		`{"contents":[{"parts":[{"thought":true,"text":"wrong role"}]}]}`,
		`{"contents":[{"parts":[{"fileData":{"mimeType":"image/png","fileUri":"https://example.invalid/image"}}]}]}`,
		`{"contents":[{"parts":[{"inlineData":{"mimeType":"audio/wav","data":"YWJj"}}]}]}`,
		`{"contents":[{"parts":[{"inlineData":{"mimeType":"image/png","data":"invalid!"}}]}]}`,
		`{"contents":[{"parts":[{"text":"x"}]}],"cachedContent":"caches/opaque"}`,
		`{"contents":[{"parts":[{"text":"x"}]}],"tools":[{"googleSearch":{}}]}`,
		`{"contents":[{"parts":[{"text":"x"}]}],"tools":[{"codeExecution":{}}]}`,
		`{"contents":[{"parts":[{"text":"x"}]}],"generationConfig":{"candidateCount":2}}`,
		`{"contents":[{"parts":[{"text":"x"}]}],"generationConfig":{"maxOutputTokens":0}}`,
		`{"contents":[{"parts":[{"text":"x"}]}],"generationConfig":{"thinkingConfig":{"thinkingBudget":1024}}}`,
		`{"contents":[{"parts":[{"text":"x"}]}],"generationConfig":{"thinkingConfig":{"thinkingBudget":0,"thinkingLevel":"high"}}}`,
		`{"contents":[{"parts":[{"text":"x"}]}],"generationConfig":{"responseSchema":{"type":"OBJECT"}}}`,
		`{"contents":[{"parts":[{"text":"x"}]}],"generationConfig":{"responseMimeType":"application/json","responseSchema":{},"responseJsonSchema":{}}}`,
		`{"contents":[{"parts":[{"text":"x"}]}],"toolConfig":{"functionCallingConfig":{"mode":"ANY","allowedFunctionNames":["missing"]}}}`,
		`{"contents":[{"parts":[{"text":"x"}]}],"toolConfig":{"functionCallingConfig":{"mode":"VALIDATED"}}}`,
		`{"contents":[{"parts":[{"text":"x"}]}],"tools":[{"functionDeclarations":[{"name":"read","parameters":{},"parametersJsonSchema":{}}]}]}`,
	} {
		h, _, calls, _ := messagesFixture(t, sseOK)
		rec := httptest.NewRecorder()
		geminiTestServe(h, rec, "streamGenerateContent", body)
		if rec.Code != 400 || *calls != 0 || !strings.Contains(rec.Body.String(), "INVALID_ARGUMENT") {
			t.Fatalf("invalid request consumed upstream or lost Google error: %s -> %d calls=%d %s", body, rec.Code, *calls, rec.Body.String())
		}
	}
}

func TestGeminiSelectedFunctionAndSchemaRemainEnforced(t *testing.T) {
	for _, fn := range []string{
		`{"name":"write","arguments":"{\"id\":1}"}`,
		`{"name":"read","arguments":"{\"id\":\"wrong-type\"}"}`,
	} {
		reply := "data: {\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"one\",\"type\":\"function\",\"function\":" + fn + "}]},\"finish_reason\":\"tool_calls\"}]}\n\ndata: [DONE]\n\n"
		h, _, _, _ := messagesFixture(t, reply)
		rec := httptest.NewRecorder()
		body := `{"contents":[{"parts":[{"text":"read"}]}],"tools":[{"functionDeclarations":[{"name":"read","parameters":{"type":"OBJECT","properties":{"id":{"type":"INTEGER"}},"required":["id"]}}]}],"toolConfig":{"functionCallingConfig":{"mode":"ANY","allowedFunctionNames":["read"]}}}`
		geminiTestServe(h, rec, "streamGenerateContent", body)
		if !strings.Contains(rec.Body.String(), `"error"`) || strings.Contains(rec.Body.String(), "functionCall") || strings.Contains(rec.Body.String(), `"finishReason":"STOP"`) {
			t.Fatalf("an invalid executable function escaped Chat validation: %s", rec.Body.String())
		}
	}
}

func TestGeminiThinkingDisabledDoesNotExposeThoughts(t *testing.T) {
	reply := "data: {\"choices\":[{\"index\":0,\"delta\":{\"reasoning_content\":\"private thought\",\"content\":\"answer\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"
	for _, config := range []string{`{}`, `{"thinkingConfig":{"includeThoughts":false,"thinkingLevel":"high"}}`, `{"thinkingConfig":{"thinkingBudget":0,"includeThoughts":true}}`} {
		h, _, _, _ := messagesFixture(t, reply)
		rec := httptest.NewRecorder()
		geminiTestServe(h, rec, "streamGenerateContent", `{"contents":[{"parts":[{"text":"x"}]}],"generationConfig":`+config+"}")
		if rec.Code != 200 || strings.Contains(rec.Body.String(), "private thought") || !strings.Contains(rec.Body.String(), "answer") {
			t.Fatalf("includeThoughts/disabled thinking not honored: %s", rec.Body.String())
		}
	}
}

func TestGeminiSchemaDepthIsBoundedBeforeUpstream(t *testing.T) {
	var schema any = map[string]any{"type": "OBJECT"}
	for i := 0; i < 66; i++ {
		schema = map[string]any{"type": "OBJECT", "properties": map[string]any{"child": schema}}
	}
	body, _ := json.Marshal(map[string]any{
		"contents": []any{map[string]any{"parts": []any{map[string]any{"text": "x"}}}},
		"tools":    []any{map[string]any{"functionDeclarations": []any{map[string]any{"name": "nested", "parameters": schema}}}},
	})
	h, _, calls, _ := messagesFixture(t, sseOK)
	rec := httptest.NewRecorder()
	geminiTestServe(h, rec, "generateContent", string(body))
	if rec.Code != 400 || *calls != 0 || !strings.Contains(rec.Body.String(), "depth") {
		t.Fatalf("deep schema consumed upstream without a bound: %d calls=%d %s", rec.Code, *calls, rec.Body.String())
	}
}

func TestGeminiThinkingLevelAcceptsSDKEnumsAndNFCasing(t *testing.T) {
	for _, level := range []string{"low", "LOW", "medium", "MEDIUM", "high", "HIGH", "MINIMAL", "unknown"} {
		h, outbound, calls, _ := messagesFixture(t, sseOK)
		body, _ := json.Marshal(map[string]any{
			"contents":         []any{map[string]any{"parts": []any{map[string]any{"text": "x"}}}},
			"generationConfig": map[string]any{"thinkingConfig": map[string]any{"thinkingLevel": level, "includeThoughts": true}},
		})
		rec := httptest.NewRecorder()
		geminiTestServe(h, rec, "generateContent", string(body))
		if level == "MINIMAL" || level == "unknown" {
			if rec.Code != 400 || *calls != 0 {
				t.Fatalf("unsupported thinking level gained an invented mapping: %s %d %s", level, rec.Code, rec.Body.String())
			}
			continue
		}
		if rec.Code != 200 || *calls != 1 || (*outbound)["reasoning_effort"] != strings.ToLower(level) {
			t.Fatalf("SDK/NF enum differs: %s %d outbound=%v body=%s", level, rec.Code, *outbound, rec.Body.String())
		}
	}
}
