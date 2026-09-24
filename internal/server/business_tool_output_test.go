// ═══ 更新日志 ═══
// 2026-09-25：锁定业务工具结果的完整JSON往返，避免泛型type被误判为协议内容而拒绝或丢字段。
package server

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/upstream"
)

func businessToolRequest(t *testing.T, kind string, output any) []byte {
	t.Helper()
	call := map[string]any{"type": kind, "call_id": "metadata_1", "name": "inspect_metadata"}
	if kind == "custom_tool_call" {
		call["input"] = "inspect"
	} else {
		call["arguments"] = "{}"
	}
	body, err := json.Marshal(map[string]any{"model": "cn:fixture", "input": []any{
		map[string]any{"role": "user", "content": "Inspect the metadata."},
		call,
		map[string]any{"type": kind + "_output", "call_id": "metadata_1", "output": output},
		map[string]any{"role": "user", "content": "Keep every metadata field."},
	}})
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func assertBusinessJSONPreserved(t *testing.T, chat map[string]any, expected any) {
	t.Helper()
	want, err := json.Marshal(expected)
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, raw := range chat["messages"].([]any) {
		message := raw.(map[string]any)
		if message["role"] != "tool" {
			continue
		}
		count++
		content, ok := message["content"].(string)
		if !ok {
			t.Fatalf("business JSON became protocol content: %#v", message["content"])
		}
		var object any
		decoder := json.NewDecoder(strings.NewReader(content))
		decoder.UseNumber()
		if err := decoder.Decode(&object); err != nil {
			t.Fatalf("business object was reduced to plain text %q: %v", content, err)
		}
		got, _ := json.Marshal(object)
		if !bytes.Equal(got, want) {
			t.Fatalf("business fields changed: got %s want %s", got, want)
		}
	}
	if count != 1 {
		t.Fatalf("tool result count=%d want 1", count)
	}
}

func TestBusinessToolOutputTypesKeepAllFields(t *testing.T) {
	cases := map[string]map[string]any{
		"file":            {"type": "file", "path": "report.txt", "size": 12},
		"audio":           {"type": "audio", "format": "wav", "duration": 3},
		"image":           {"type": "image", "path": "diagram.png", "width": 640, "height": 480},
		"document":        {"type": "document", "title": "Report", "pages": 12},
		"text":            {"type": "text", "text": "report body", "line_count": 12},
		"refusal":         {"type": "refusal", "refusal": "missing permission", "retryable": false},
		"summary_text":    {"type": "summary_text", "text": "summary", "source_count": 7},
		"business_source": {"type": "file", "source": map[string]any{"type": "filesystem", "data": map[string]any{"rows": 2}}, "size": json.Number("9007199254740993")},
	}
	for _, kind := range []string{"function_call", "custom_tool_call"} {
		for name, output := range cases {
			t.Run(kind+"/"+name, func(t *testing.T) {
				converted, _, err := responsesToChat(businessToolRequest(t, kind, output))
				if err != nil {
					t.Fatalf("business object was rejected: %v", err)
				}
				var chat map[string]any
				decoder := json.NewDecoder(bytes.NewReader(converted))
				decoder.UseNumber()
				if err := decoder.Decode(&chat); err != nil {
					t.Fatal(err)
				}
				assertBusinessJSONPreserved(t, chat, output)
			})
		}
	}
}

func TestBusinessToolOutputSurvivesHTTPAndUpstreamNormalization(t *testing.T) {
	for _, output := range []map[string]any{
		{"type": "file", "path": "report.txt", "size": 12},
		{"type": "text", "text": "result", "metadata": map[string]any{"row_id": json.Number("9007199254740993"), "complete": true}},
		{"type": "refusal", "refusal": "file not found", "path": "report.txt", "retryable": false},
	} {
		t.Run(output["type"].(string), func(t *testing.T) {
			var captured map[string]any
			calls := 0
			client := &upstream.Client{ChatBaseCN: "https://business-json.invalid", HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				calls++
				decoder := json.NewDecoder(r.Body)
				decoder.UseNumber()
				if err := decoder.Decode(&captured); err != nil {
					t.Fatal(err)
				}
				return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(sseOK))}, nil
			})}}
			h := NewHandler(Config{Pool: testPoolWith(&auth.Auth{UID: "fixture", AccessToken: "fixture", ExpiresAt: 9999999999}), Upstream: client})
			w := httptest.NewRecorder()
			h.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(businessToolRequest(t, "function_call", output))))
			if w.Code != 200 || calls != 1 {
				t.Fatalf("HTTP=%d upstream_calls=%d body=%s", w.Code, calls, w.Body)
			}
			assertBusinessJSONPreserved(t, captured, output)
		})
	}
}

func TestExplicitSingleImageToolOutputRemainsMultimodal(t *testing.T) {
	uri := "data:image/png;base64,iVBORw0KGgo="
	converted, _, err := responsesToChat(businessToolRequest(t, "function_call", map[string]any{"type": "input_image", "image_url": uri, "detail": "high"}))
	if err != nil {
		t.Fatal(err)
	}
	var chat map[string]any
	if err := json.Unmarshal(converted, &chat); err != nil {
		t.Fatal(err)
	}
	for _, raw := range chat["messages"].([]any) {
		message := raw.(map[string]any)
		if message["role"] != "tool" {
			continue
		}
		parts, ok := message["content"].([]any)
		if !ok || len(parts) != 1 {
			t.Fatalf("explicit image became text: %#v", message["content"])
		}
		part := parts[0].(map[string]any)
		image := part["image_url"].(map[string]any)
		if part["type"] != "image_url" || image["url"] != uri || image["detail"] != "high" {
			t.Fatalf("image fields changed: %#v", part)
		}
		return
	}
	t.Fatal("explicit image tool result missing")
}
