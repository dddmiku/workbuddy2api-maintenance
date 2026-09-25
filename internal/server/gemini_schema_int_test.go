// ═══ 更新日志 ═══
// 2026-09-26：锁定 Gemini 字符串形式的计数约束可用，非法值仍拒绝。
package server

import (
	"encoding/json"
	"testing"
)

func TestGeminiSchemaAcceptsStringCounts(t *testing.T) {
	var params any
	_ = json.Unmarshal([]byte(`{"type":"OBJECT","properties":{"tags":{"type":"ARRAY","minItems":"1","maxItems":"5","items":{"type":"STRING","maxLength":"20"}}},"required":["tags"]}`), &params)
	schema, err := geminiSchema(params, "tools[0].functionDeclarations[0].parameters")
	if err != nil {
		t.Fatalf("string counts rejected: %v", err)
	}
	if _, err := compileResponseSchema(schema); err != nil {
		t.Fatalf("converted schema does not compile: %v", err)
	}
	tags := schema["properties"].(map[string]any)["tags"].(map[string]any)
	if tags["minItems"] != json.Number("1") || tags["items"].(map[string]any)["maxLength"] != json.Number("20") {
		t.Fatalf("counts not converted: %v", tags)
	}
	_ = json.Unmarshal([]byte(`{"type":"ARRAY","minItems":"-1"}`), &params)
	if _, err := geminiSchema(params, "p"); err == nil {
		t.Fatal("negative count accepted")
	}
}
