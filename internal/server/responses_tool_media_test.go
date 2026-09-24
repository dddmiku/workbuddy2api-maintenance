// ═══ 更新日志 ═══
// 2026-09-25：通过真实 Handler 出站链检查工具图片的多模态形状及工具调用配对，避免仅验证第一层转换。
package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/upstream"
)

func responsesToolMediaRequest(t *testing.T, input []any, imageBudget int) (map[string]any, *httptest.ResponseRecorder, int) {
	t.Helper()
	var captured map[string]any
	calls := 0
	client := &upstream.Client{
		ChatBaseCN: "https://tool-media.invalid",
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			calls++
			if err := json.NewDecoder(r.Body).Decode(&captured); err != nil {
				t.Errorf("decode outbound body: %v", err)
			}
			return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(sseOK))}, nil
		})},
		OutboundImageBudgetBytes: imageBudget,
	}
	handler := NewHandler(Config{Pool: testPoolWith(&auth.Auth{UID: "tool-media-fixture", AccessToken: "fixture", ExpiresAt: 9999999999}), Upstream: client})
	body, err := json.Marshal(map[string]any{"model": "cn:deepseek-v4.1-flash", "stream": true, "input": input})
	if err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body)))
	return captured, recorder, calls
}

func TestResponsesToolMediaWirePreservesImagesAndParallelPairing(t *testing.T) {
	for _, custom := range []bool{false, true} {
		kind := "function_call"
		if custom {
			kind = "custom_tool_call"
		}
		t.Run(kind, func(t *testing.T) {
			input := []any{map[string]any{"role": "user", "content": "Inspect two images in each group."}}
			uris := make(map[string]bool)
			toolTexts := make(map[string]string)
			for group := 0; group < 2; group++ {
				for index := 0; index < 2; index++ {
					input = append(input, map[string]any{"type": kind, "call_id": fmt.Sprintf("g%d_c%d", group, index), "name": "view_image", "arguments": "{}", "input": "image"})
				}
				for index := 0; index < 2; index++ {
					if index == 1 {
						input = append(input, map[string]any{"role": "developer", "content": "image resize notice"})
					}
					uri := "data:image/png;base64," + strings.Repeat(string(rune('A'+group*2+index)), 64*1024)
					uris[uri] = false
					callID := fmt.Sprintf("g%d_c%d", group, index)
					toolTexts[callID] = fmt.Sprintf("loaded screenshot %d/%d", group, index)
					input = append(input, map[string]any{"type": kind + "_output", "call_id": callID, "output": []any{
						map[string]any{"type": "input_text", "text": toolTexts[callID]},
						map[string]any{"type": "input_image", "image_url": uri, "detail": "high"},
					}})
				}
			}
			input = append(input, map[string]any{"role": "user", "content": "Compare all four images."})
			chat, recorder, calls := responsesToolMediaRequest(t, input, 0)
			if recorder.Code != http.StatusOK || calls != 1 {
				t.Fatalf("unexpected request result: status=%d upstream_calls=%d", recorder.Code, calls)
			}
			pending := make(map[string]bool)
			callCount, resultCount, imageCount, imageChars, textChars := 0, 0, 0, 0, 0
			imageRoles := make(map[string]int)
			for _, raw := range chat["messages"].([]any) {
				message := raw.(map[string]any)
				role, _ := message["role"].(string)
				if role != "tool" && len(pending) > 0 {
					t.Fatalf("tool-result block interrupted by role=%s with %d pending results", role, len(pending))
				}
				if role == "assistant" {
					toolCalls, _ := message["tool_calls"].([]any)
					for _, rawCall := range toolCalls {
						call := rawCall.(map[string]any)
						pending[call["id"].(string)] = true
						callCount++
					}
				}
				if role == "tool" {
					id, _ := message["tool_call_id"].(string)
					if !pending[id] {
						t.Fatalf("tool result has no matching call: %s", id)
					}
					delete(pending, id)
					resultCount++
					toolText, isText := message["content"].(string)
					if !isText {
						var pieces []string
						parts, _ := message["content"].([]any)
						for _, rawPart := range parts {
							part, _ := rawPart.(map[string]any)
							if text, ok := part["text"].(string); ok {
								pieces = append(pieces, text)
							}
						}
						toolText = strings.Join(pieces, "\n")
					}
					if toolText != toolTexts[id] {
						t.Fatalf("text changed or moved out of its tool result: %s", id)
					}
				}
				if text, ok := message["content"].(string); ok {
					textChars += len(text)
					if strings.Contains(text, "data:image/") {
						t.Fatal("image was serialized into a content string")
					}
				}
				parts, _ := message["content"].([]any)
				for _, rawPart := range parts {
					part := rawPart.(map[string]any)
					if text, ok := part["text"].(string); ok {
						textChars += len(text)
						if strings.Contains(text, "data:image/") {
							t.Fatal("image was serialized into a text part")
						}
					}
					if part["type"] != "image_url" {
						continue
					}
					image := part["image_url"].(map[string]any)
					uri := image["url"].(string)
					if seen, known := uris[uri]; !known || seen || image["detail"] != "high" {
						t.Fatal("image duplicated, changed, or lost its detail")
					}
					uris[uri] = true
					imageCount++
					imageChars += len(uri)
					imageRoles[role]++
				}
			}
			if len(pending) != 0 || callCount != 4 || resultCount != 4 || imageCount != 4 {
				t.Fatalf("history lost items: pending=%d calls=%d results=%d images=%d", len(pending), callCount, resultCount, imageCount)
			}
			t.Logf("wire: calls=%d results=%d images=%d image_url_chars=%d text_chars=%d image_roles=%v", callCount, resultCount, imageCount, imageChars, textChars, imageRoles)
		})
	}
}
