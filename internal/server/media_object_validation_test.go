// ═══ 更新日志 ═══
// 2026-09-25：工具结果单个媒体对象必须经过内容校验，业务JSON对象仍完整保留。
package server

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestResponsesSingleToolOutputMediaValidated(t *testing.T) {
	for _, kind := range []string{"input_file", "input_audio", "input_image"} {
		t.Run(kind, func(t *testing.T) {
			raw, _ := json.Marshal(map[string]any{"model": "fixture", "input": []any{
				map[string]any{"type": "function_call_output", "call_id": "c1", "output": map[string]any{"type": kind, "data": "opaque"}},
			}})
			if _, _, err := responsesToChat(raw); err == nil {
				t.Fatal("unsupported or malformed single media object was accepted as text")
			}
		})
	}
	for _, output := range []map[string]any{{"type": "business_result", "value": 42}, {"count": 3}} {
		raw, _ := json.Marshal(map[string]any{"model": "fixture", "input": []any{
			map[string]any{"type": "function_call_output", "call_id": "c1", "output": output},
		}})
		converted, _, err := responsesToChat(raw)
		if err != nil || !strings.Contains(string(converted), "content") {
			t.Fatalf("business result lost: %s, %v", converted, err)
		}
	}
}
