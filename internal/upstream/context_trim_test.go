// ═══ 更新日志 ═══
// 2026-09-23：新增上下文超限裁剪的回归测试（轮边界、tool 配对完整、无效输入零改动）。
package upstream

import (
	"encoding/json"
	"testing"
)

// buildTurns 造 n 轮「user → assistant(tool_calls) → tool」对话，前面带一条 system。
func buildTurns(t *testing.T, n int) []byte {
	t.Helper()
	msgs := []any{
		map[string]any{"role": "system", "content": "sys"},
	}
	for i := 0; i < n; i++ {
		msgs = append(msgs,
			map[string]any{"role": "user", "content": "question"},
			map[string]any{"role": "assistant", "content": "",
				"tool_calls": []any{map[string]any{
					"id": "c", "type": "function",
					"function": map[string]any{"name": "exec", "arguments": "{}"},
				}}},
			map[string]any{"role": "tool", "tool_call_id": "c", "content": "out"},
		)
	}
	body, err := json.Marshal(map[string]any{"model": "deepseek-v4.1-flash", "messages": msgs})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return body
}

// TestTrimOldestContextKeepsToolPairing 裁剪后每条 assistant.tool_calls 都有对应 tool 结果。
func TestTrimOldestContextKeepsToolPairing(t *testing.T) {
	body := buildTurns(t, 8)
	out, changed := TrimOldestContext(body, 0.5)
	if !changed {
		t.Fatalf("expected trim to change the body")
	}
	if len(out) >= len(body) {
		t.Fatalf("trimmed body should be smaller: %d -> %d", len(body), len(out))
	}
	var obj struct {
		Messages []map[string]any `json:"messages"`
	}
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	pending := map[string]bool{}
	for _, message := range obj.Messages {
		role, _ := message["role"].(string)
		switch role {
		case "assistant":
			calls, _ := message["tool_calls"].([]any)
			for _, raw := range calls {
				call, _ := raw.(map[string]any)
				id, _ := call["id"].(string)
				pending[id] = true
			}
		case "tool":
			id, _ := message["tool_call_id"].(string)
			if !pending[id] {
				t.Fatalf("orphan tool result for call %q after trim", id)
			}
			delete(pending, id)
		}
	}
	if len(pending) != 0 {
		t.Fatalf("tool calls left without results after trim: %v", pending)
	}
}

// TestTrimOldestContextKeepsSystemAndNewestTurn 前导 system 与最后一轮必须保留。
func TestTrimOldestContextKeepsSystemAndNewestTurn(t *testing.T) {
	body := buildTurns(t, 8)
	out, changed := TrimOldestContext(body, 0.25)
	if !changed {
		t.Fatalf("expected trim to change the body")
	}
	var obj struct {
		Messages []map[string]any `json:"messages"`
	}
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(obj.Messages) == 0 {
		t.Fatalf("trimmed body has no messages")
	}
	if role, _ := obj.Messages[0]["role"].(string); role != "system" {
		t.Fatalf("preamble system message dropped: first role=%q", role)
	}
	last := obj.Messages[len(obj.Messages)-1]
	if role, _ := last["role"].(string); role != "tool" {
		t.Fatalf("newest turn dropped: last role=%q", role)
	}
	users := 0
	for _, message := range obj.Messages {
		if role, _ := message["role"].(string); role == "user" {
			users++
		}
	}
	if users != 2 {
		t.Fatalf("keepRatio=0.25 of 8 turns should keep 2 turns, got %d", users)
	}
}

// TestTrimOldestContextNoopCases 不该改动的输入一律原样返回。
func TestTrimOldestContextNoopCases(t *testing.T) {
	cases := []struct {
		name  string
		body  string
		ratio float64
	}{
		{"not json", "not json at all", 0.5},
		{"no messages", `{"model":"m"}`, 0.5},
		{"single turn", `{"messages":[{"role":"user","content":"hi"}]}`, 0.5},
		{"ratio zero", `{"messages":[{"role":"user","content":"a"},{"role":"user","content":"b"}]}`, 0},
		{"ratio one", `{"messages":[{"role":"user","content":"a"},{"role":"user","content":"b"}]}`, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, changed := TrimOldestContext([]byte(tc.body), tc.ratio)
			if changed {
				t.Fatalf("expected no change")
			}
			if string(got) != tc.body {
				t.Fatalf("body must be returned unchanged")
			}
		})
	}
}

// TestContextTrimLevelsMatchesRatios 档位数与比例表一致，避免调用方越界。
func TestContextTrimLevelsMatchesRatios(t *testing.T) {
	if ContextTrimLevels() != len(contextTrimKeepRatios) {
		t.Fatalf("ContextTrimLevels()=%d ratios=%d", ContextTrimLevels(), len(contextTrimKeepRatios))
	}
	if ContextTrimLevels() == 0 {
		t.Fatalf("expected at least one trim level")
	}
}
