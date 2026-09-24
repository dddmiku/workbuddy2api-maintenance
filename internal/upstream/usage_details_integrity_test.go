// ═══ 更新日志 ═══
// 2026-09-25：回放后帧用量明细类型异常，验证已观测缓存和推理用量不会被非法占位值抹掉。
package upstream

import (
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestMalformedUsageDetailsDoNotEraseMeasurements(t *testing.T) {
	for _, invalid := range []string{`"unknown"`, `false`, `0`, `[]`} {
		for _, mode := range []string{"aggregate", "stream"} {
			t.Run(mode+"/"+invalid, func(t *testing.T) {
				raw := "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"}}],\"usage\":{\"prompt_tokens\":100,\"completion_tokens\":20,\"prompt_tokens_details\":{\"cached_tokens\":80},\"completion_tokens_details\":{\"reasoning_tokens\":15}}}\n\n" +
					fmt.Sprintf("data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"credit\":0.25,\"prompt_tokens_details\":%s,\"completion_tokens_details\":%s}}\n\n", invalid, invalid) +
					"data: [DONE]\n\n"
				var observed map[string]any
				if mode == "aggregate" {
					response, err := Aggregate(strings.NewReader(raw))
					if err != nil {
						t.Fatal(err)
					}
					observed, _ = response["usage"].(map[string]any)
				} else {
					writer := httptest.NewRecorder()
					if err := Stream(writer, strings.NewReader(raw)); err != nil {
						t.Fatal(err)
					}
					if err := readSSE(strings.NewReader(writer.Body.String()), func(ev sseEvent) (bool, error) {
						frame, done, err := decodeSSEEvent(ev)
						if done || err != nil {
							return true, err
						}
						if usage, ok := frame["usage"].(map[string]any); ok {
							observed = usage
						}
						return false, nil
					}, nil); err != nil {
						t.Fatal(err)
					}
				}
				if cached, ok := CachedInputTokens(observed); !ok || cached != 80 {
					t.Errorf("malformed late detail erased observed cached input: %v", observed)
				}
				detail, _ := observed["completion_tokens_details"].(map[string]any)
				if reasoning, ok := UsageCount(detail["reasoning_tokens"]); !ok || reasoning != 15 {
					t.Errorf("malformed late detail erased observed reasoning output: %v", observed)
				}
				if credit, ok := UsageCredit(observed["credit"]); !ok || credit != 0.25 {
					t.Errorf("valid fields from the same late frame were lost: %v", observed)
				}
			})
		}
	}
}
