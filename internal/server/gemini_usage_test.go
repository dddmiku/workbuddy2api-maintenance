// ═══ 更新日志 ═══
// 2026-09-25：覆盖 Gemini 已知/未知/部分用量及累计帧，避免缓存和推理重复计数。
package server

import (
	"strings"
	"testing"
)

func TestGeminiWriterUsageKeepsKnownFieldsAndNeverEstimates(t *testing.T) {
	for _, tc := range []struct {
		name, usage string
		want, omit  []string
	}{
		{"legacy_reasoning", `{"prompt_tokens":100,"completion_tokens":20,"completion_thinking_tokens":5}`,
			[]string{`"candidatesTokenCount":15`, `"thoughtsTokenCount":5`, `"totalTokenCount":120`}, nil},
		{"partial_reasoning", `{"prompt_tokens":100,"completion_tokens_details":{"reasoning_tokens":5}}`,
			[]string{`"promptTokenCount":100`, `"thoughtsTokenCount":5`, `"complete":false`}, []string{"candidatesTokenCount", "totalTokenCount", "outputTokenCount"}},
		{"unknown_reasoning", `{"prompt_tokens":100,"completion_tokens":20,"prompt_cache_hit_tokens":80}`,
			[]string{`"candidatesTokenCount":20`, `"cachedContentTokenCount":80`, `"totalTokenCount":120`, `"candidatesIncludeThoughts":true`, `"thoughtsReported":false`}, []string{"thoughtsTokenCount"}},
		{"explicit_zero", `{"prompt_tokens":0,"completion_tokens":0,"completion_tokens_details":{"reasoning_tokens":0}}`,
			[]string{`"promptTokenCount":0`, `"candidatesTokenCount":0`, `"thoughtsTokenCount":0`, `"totalTokenCount":0`, `"complete":true`}, nil},
		{"inconsistent_reasoning", `{"prompt_tokens":100,"completion_tokens":20,"completion_tokens_details":{"reasoning_tokens":30}}`,
			[]string{`"candidatesTokenCount":20`, `"totalTokenCount":120`, `"inconsistent":true`}, []string{"thoughtsTokenCount"}},
		{"missing", "null", nil, []string{"usageMetadata", "promptTokenCount", "candidatesTokenCount", "totalTokenCount"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g, rec := geminiStreamFixture()
			raw := "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}],\"usage\":" + tc.usage + "}\n\ndata: [DONE]\n\n"
			if _, err := g.Write([]byte(raw)); err != nil {
				t.Fatal(err)
			}
			g.finish()
			for _, want := range tc.want {
				if !strings.Contains(rec.Body.String(), want) {
					t.Errorf("missing %s: %s", want, rec.Body.String())
				}
			}
			for _, omit := range tc.omit {
				if strings.Contains(rec.Body.String(), omit) {
					t.Errorf("unmeasured field %s was invented: %s", omit, rec.Body.String())
				}
			}
		})
	}
}

func TestGeminiWriterMergesCumulativeUsageWithoutDoubleCounting(t *testing.T) {
	g, rec := geminiStreamFixture()
	raw := "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"}}],\"usage\":{\"prompt_tokens\":100,\"completion_tokens\":20,\"prompt_tokens_details\":{\"cached_tokens\":60},\"completion_tokens_details\":{\"reasoning_tokens\":12}}}\n\n" +
		"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":100,\"completion_tokens\":21,\"completion_tokens_details\":\"invalid-placeholder\"}}\n\n" +
		"data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"
	_, _ = g.Write([]byte(raw))
	g.finish()
	for _, expected := range []string{`"promptTokenCount":100`, `"candidatesTokenCount":9`, `"thoughtsTokenCount":12`, `"cachedContentTokenCount":60`, `"totalTokenCount":121`} {
		if !strings.Contains(rec.Body.String(), expected) {
			t.Fatalf("cumulative usage was summed or lost %s: %s", expected, rec.Body.String())
		}
	}
}
