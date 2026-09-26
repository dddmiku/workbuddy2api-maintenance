// ═══ 更新日志 ═══
// 2026-09-26：adaptive 思考映射为 enabled；历史里不支持的内容块用文字占位。
package server

import (
	"encoding/json"
	"strings"
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

// 读过一次 PDF 后，历史里的 document 块不应让整段会话永久 400。
func TestMessagesUnsupportedHistoryBlocksBecomePlaceholders(t *testing.T) {
	body := `{"model":"global:deepseek-v4.1-flash","max_tokens":1024,"messages":[
	  {"role":"assistant","content":[{"type":"tool_use","id":"toolu_1","name":"Read","input":{"path":"a.pdf"}}]},
	  {"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":[
	     {"type":"document","source":{"type":"base64","media_type":"application/pdf","data":"JVBERi0="}},
	     {"type":"tool_reference","tool_name":"Read"},
	     {"type":"text","text":"read the pdf"}]}]},
	  {"role":"user","content":"继续"}]}`
	chat, _, _, err := messagesToChat([]byte(body))
	if err != nil {
		t.Fatalf("history with a document block was rejected: %v", err)
	}
	converted := string(chat)
	if !strings.Contains(converted, "unsupported content block: document") ||
		!strings.Contains(converted, "unsupported content block: tool_reference") {
		t.Fatalf("placeholders missing: %s", converted)
	}
	if !strings.Contains(converted, "read the pdf") {
		t.Fatalf("supported sibling block was dropped: %s", converted)
	}
}
