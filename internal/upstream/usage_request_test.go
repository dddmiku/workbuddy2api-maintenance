// ═══ 更新日志 ═══
// 2026-09-25：模拟遵守 include_usage 的真实出站服务，防止客户端展示选项让内部计量缺失。
package upstream

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"workbuddy2api/internal/auth"
)

func TestChatStreamAlwaysRequestsMeasuredUsage(t *testing.T) {
	for _, tc := range []struct{ name, options string }{
		{"missing", ""},
		{"disabled", `,"stream_options":{"include_usage":false}`},
		{"empty", `,"stream_options":{}`},
		{"null", `,"stream_options":null`},
		{"other_option", `,"stream_options":{"include_usage":false,"include_obfuscation":true}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := testClient(func(r *http.Request) (*http.Response, error) {
				var sent map[string]any
				if err := json.NewDecoder(r.Body).Decode(&sent); err != nil {
					t.Fatal(err)
				}
				options, _ := sent["stream_options"].(map[string]any)
				if tc.name == "other_option" && options["include_obfuscation"] != true {
					t.Error("unrelated stream option was overwritten")
				}
				frames := "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\n"
				if options["include_usage"] == true {
					frames += "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":123,\"completion_tokens\":4,\"total_tokens\":127}}\n\n"
				}
				frames += "data: [DONE]\n\n"
				return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(frames))}, nil
			})
			source := `{"model":"model","messages":[{"role":"user","content":"fixture"}]` + tc.options + `}`
			body := []byte(source)
			reader, status, errorBody, err := client.ChatStreamContext(context.Background(), &auth.Auth{UID: "test-account"}, body, "", ChatMeta{})
			if err != nil || status != http.StatusOK {
				t.Fatalf("chat request: status=%d errorBody=%s err=%v", status, errorBody, err)
			}
			defer reader.Close()
			response, err := Aggregate(reader)
			if err != nil {
				t.Fatal(err)
			}
			usage, _ := response["usage"].(map[string]any)
			if prompt, ok := UsageCount(usage["prompt_tokens"]); !ok || prompt != 123 {
				t.Errorf("client display preference prevented measured upstream usage: %v", usage)
			}
			if output, ok := UsageCount(usage["completion_tokens"]); !ok || output != 4 {
				t.Errorf("measured output missing: %v", usage)
			}
			if string(body) != source {
				t.Error("outbound adaptation changed original caller request")
			}
		})
	}
}
