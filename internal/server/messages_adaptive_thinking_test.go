// ═══ 更新日志 ═══
// 2026-09-26：adaptive 思考映射为 enabled 的回归。
package server

import (
	"encoding/json"
	"testing"
)

func TestMessagesAdaptiveThinkingBecomesEnabled(t *testing.T) {
	body := `{"model":"global:deepseek-v4.1-flash","max_tokens":2048,"thinking":{"type":"adaptive"},"messages":[{"role":"user","content":"hi"}]}`
	chat, _, _, err := messagesToChat([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(chat, &out); err != nil {
		t.Fatal(err)
	}
	thinking, _ := out["thinking"].(map[string]any)
	if thinking["type"] != "enabled" || thinking["budget_tokens"] != nil {
		t.Fatalf("adaptive thinking forwarded as %v", out["thinking"])
	}
	if out["reasoning_effort"] != "high" {
		t.Fatalf("reasoning effort = %v", out["reasoning_effort"])
	}
}
