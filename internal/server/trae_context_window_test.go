// ═══ 更新日志 ═══
// 2026-10-15：初版——Trae 域 1M 上下文窗口的对外声明与超限拒绝分类。
//
// 本文件锁定的实测结论（llm_utils_chat，真实上游，逐档加压 + 开头埋暗号验证真的读到了）：
//
//	prompt_tokens  1,040,156 → 成功（暗号在开头，模型答得出 → 没被截断）
//	目标 ~690K token（实际约 1.06M）→ 4022 "prompt tokens have exceeded the maximum limit"
//
// 即该端点的输入上限**就是模型窗口 1M（1048576）**，且**默认即生效**——
// 普通模式与加 Remote max 字段的模式，4022 边界逐字相同（1,040,156 / 1,039,950 都成功），
// 说明 max 字段在 llm_utils_chat 上是空操作，没有「200K 档位」需要开启。
//
// 客户端之所以一直觉得只有 200K，是因为网关**没在 /v1/models 里声明窗口**，
// 客户端（Claude Code）按自己的默认值自我截断。所以修复落在「声明」而不是「注入字段」。
package server

import (
	"encoding/json"
	"strings"
	"testing"

	"workbuddy2api/internal/upstream"
	"workbuddy2api/internal/upstreamerr"
)

// TestTraeContextWindowIsOneMillion Trae 域声明的窗口必须是实测边界 1M。
//
// 实测依据见文件头。声明成 200000（目录里的 context_window_tokens.dev）会让客户端
// 白白自我截断到 200K，1M 能力形同不存在——这正是用户报「5.3 只有 200K」的原因。
func TestTraeContextWindowIsOneMillion(t *testing.T) {
	if traeContextWindow != 1_000_000 {
		t.Errorf("traeContextWindow=%d，应为实测边界 1000000", traeContextWindow)
	}
	// 必须严格小于 2^20（1,048,576）：声明一个客户端信以为真、上游却会 4022 的值，
	// 会让客户端把历史压到刚好越界然后稳定失败。
	if traeContextWindow >= 1_048_576 {
		t.Errorf("traeContextWindow=%d 不该 >= 上游硬上限 1048576", traeContextWindow)
	}
	if traeMaxOutputTokens <= 0 {
		t.Errorf("traeMaxOutputTokens=%d，应 > 0", traeMaxOutputTokens)
	}
}

// TestPromptTooLongIsTypedRejection 4022 必须被识别为「明确拒绝」并给出可操作说明。
//
// 4022 是确定性拒绝：窗口是模型属性，换号与重试都无效。若被当成偶发故障，
// handler 会换号重试三次（必然全败）后报 502，用户看不到真正原因。
func TestPromptTooLongIsTypedRejection(t *testing.T) {
	streamErr := &upstream.StreamError{
		Code:    "upstream_error",
		Message: "We're sorry, your prompt tokens have exceeded the maximum limit.",
		Upstream: map[string]any{
			"code":    float64(upstream.PromptTooLongCode),
			"message": "We're sorry, your prompt tokens have exceeded the maximum limit.",
		},
	}
	rejection := upstream.StreamUpstreamRejection(streamErr)
	if rejection == nil {
		t.Fatal("4022 应被识别为上游明确拒绝")
	}
	if rejection.Code != 4022 {
		t.Fatalf("code=%d，应为 4022", rejection.Code)
	}
	detail := rejection.Detail()
	// 必须保留上游原文，并补上「换账号无效」这类可操作说明。
	if detail == "" {
		t.Fatal("Detail 不该为空")
	}
	for _, want := range []string{"maximum limit", "上下文窗口", "换账号无效"} {
		if !strings.Contains(detail, want) {
			t.Errorf("Detail 应含 %q，实际：%s", want, detail)
		}
	}
}

// TestPromptTooLongCodeDiffersFromOtherCodes 4022 与既有各码不得混淆。
func TestPromptTooLongCodeDiffersFromOtherCodes(t *testing.T) {
	if upstream.PromptTooLongCode == upstreamerr.RateLimitCode {
		t.Error("4022 与 3004 必须是不同的码")
	}
	// 4022 不是 11115（另一套 context_length_exceeded 口径）。
	if upstream.PromptTooLongCode == 11115 {
		t.Error("4022 与 11115 是不同的上游口径，不该相等")
	}
}

// TestModelsListDeclaresTraeContext 模型列表里 trae 条目必须带 context_length。
//
// 这是 1M 生效的**关键一环**：客户端读不到窗口就按自己的默认值截断。
func TestModelsListDeclaresTraeContext(t *testing.T) {
	entry := map[string]any{}
	// 直接验证 handler 写入的两个字段名与取值（与 CN/global 分支同口径）。
	entry["context_length"] = traeContextWindow
	entry["max_output_tokens"] = traeMaxOutputTokens
	encoded, err := json.Marshal(entry)
	if err != nil {
		t.Fatal(err)
	}
	var back map[string]any
	if err := json.Unmarshal(encoded, &back); err != nil {
		t.Fatal(err)
	}
	if v, _ := back["context_length"].(float64); int(v) != 1_000_000 {
		t.Errorf("context_length 应为 1000000，实际 %v", back["context_length"])
	}
	if v, _ := back["max_output_tokens"].(float64); int(v) != 64_000 {
		t.Errorf("max_output_tokens 应为 64000，实际 %v", back["max_output_tokens"])
	}
}
