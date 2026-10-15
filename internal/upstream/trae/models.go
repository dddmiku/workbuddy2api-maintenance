// models.go — Trae 域的模型目录。
//
// 上游按 config_name **精确匹配**（大小写敏感），所以客户端写的宽松名（gpt-4o、
// claude-sonnet-4-5）必须先映射到 Trae 的真实模型名，否则请求会被上游拒掉。
// 这张表来自 Trae2api-cn 的 MODEL_ALIASES（实测整理），保留其映射方向。
//
// 本文件是**静态别名表**：客户端写的宽松名（gpt-4o、claude-sonnet-4-5）在这里
// 映射到 Trae 的真实 config_name。它是**回落**路径——生产实际优先用动态目录
// （catalog.go 的 /api/ide/v1/get_detail_param，见 ChatStream 的 ResolveFromCatalog），
// 只有目录不可用时才走这张表。静态表的好处是 /v1/models 与出站映射永远一致，
// 不会出现"列表里有、发过去被拒"的裂缝。
package trae

import "strings"

// modelAliases 把外部模型名映射到 Trae 的真实 config_name。
//
// 键一律小写（查表前归一化），值保持上游要求的大小写。
var modelAliases = map[string]string{
	"auto": "glm-5.2",

	"glm-5.2": "glm-5.2",
	"glm-5.3": "glm-5.3",
	"glm-5.1": "glm-5.1",
	"glm-5":   "glm-5",
	"glm-4.7": "glm-4.7",
	"glm-4.6": "glm-4.6",

	// Anthropic 外部名 → Trae 内部可用模型。
	"claude-opus-4-7":         "glm-5.2",
	"claude-opus-4-6":         "glm-5.2",
	"claude-opus-4-5":         "glm-5.2",
	"claude-sonnet-4-6":       "glm-5.2",
	"claude-sonnet-4-5":       "glm-5.2",
	"claude-sonnet-4":         "glm-5.2",
	"claude-3.7-sonnet":       "glm-5.2",
	"claude-3-7-sonnet":       "glm-5.2",
	"claude-3.5-sonnet":       "glm-5.2",
	"claude3.5":               "glm-5.2",
	"aws_sdk_claude37_sonnet": "glm-5.2",
	"claude-haiku-4-5":        "glm-5.1",

	// Trae 真实模型名（直通，保留上游大小写）。
	"deepseek-v4-pro":            "DeepSeek-V4-Pro",
	"deepseek-v4-flash":          "DeepSeek-V4-Flash",
	"deepseek-v4-pro-official":   "DeepSeek-V4-Pro-Official",
	"deepseek-v4-flash-official": "DeepSeek-V4-Flash-Official",
	"deepseek-v3":                "DeepSeek-V4-Pro",
	"deepseek-r1":                "DeepSeek-V4-Pro",
	"doubao-seed-2.1-pro":        "Doubao-Seed-2.1-Pro",
	"doubao-seed-2.1-turbo":      "Doubao-Seed-2.1-Turbo",
	"doubao-seed-code":           "Doubao-Seed-Code",
	"doubao-seed-evolving":       "Doubao-Seed-Evolving",
	"seed-2.1-pro":               "Doubao-Seed-2.1-Pro",
	"seed-2.1-turbo":             "Doubao-Seed-2.1-Turbo",
	"seed-code":                  "Doubao-Seed-Code",
	"seed-evolving":              "Doubao-Seed-Evolving",
	"qwen3.7-plus":               "qwen-3.7-plus",
	"qwen-3.7-plus":              "qwen-3.7-plus",
	"qwen-3.6-plus":              "qwen-3.6-plus",
	"qwen3.8-max":                "qwen3.8-max",
	"qwen-3.5":                   "qwen-3.5",
	"qwen3-coder":                "qwen3-coder",
	"qwen36-35b":                 "qwen36-35b",
	"kimi-k2.6":                  "kimi-k2.6",
	"kimi-k3":                    "kimi-k3",
	"kimi-k2.7-code":             "kimi-k2.7-code",
	"kimi-k2":                    "kimi-k2",
	"kimi-k2.5":                  "kimi-k2.5",
	"minimax-m3":                 "minimax-m3",
	"minimax-m2.7":               "minimax-m2.7",
	"minimax-m2.1":               "minimax-m2.1",
	"minimax-m25":                "minimax-m25",
	"mimo-v2.5-pro":              "mimo-v2.5-pro",
	"mimo-v2.5":                  "mimo-v2.5",

	// OpenAI 外部名 → Trae 内部可用模型。
	"gpt-4o":        "DeepSeek-V4-Pro",
	"gpt-4o-latest": "DeepSeek-V4-Pro",
	"gpt-4.1":       "DeepSeek-V4-Pro",
	"gpt-4o-mini":   "DeepSeek-V4-Flash",

	"work": "work",
}

// ModelAliases 返回模型名映射的副本（供 /v1/models 与诊断使用）。
func ModelAliases() map[string]string {
	out := make(map[string]string, len(modelAliases))
	for k, v := range modelAliases {
		out[k] = v
	}
	return out
}

// ResolveModelName 把客户端写的模型名映射成 Trae 的 config_name。
//
// 查表按小写归一（客户端大小写不定）；查不到时**原样返回**——Trae 的模型名
// 在持续增加，把未知名直接透传比拒绝它更可用：上游会用一句明确的错误告诉我们
// 这个名字不存在，比网关自己猜一个替代模型要好。
func ResolveModelName(name string) string {
	trimmed := strings.TrimSpace(name)
	if trimmed == "" {
		return ""
	}
	if mapped, ok := modelAliases[strings.ToLower(trimmed)]; ok {
		return mapped
	}
	return trimmed
}

// ModelIDs 返回该域对外公布的模型名清单（排序后，供 /v1/models）。
//
// 公布的是**客户端可写的名字**（别名表的键）与 Trae 真实名，两者都列出：
// 别名便于接入常见客户端，真实名便于精确指定上游模型。
func ModelIDs() []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(modelAliases))
	for key, value := range modelAliases {
		if !seen[key] {
			seen[key] = true
			out = append(out, key)
		}
		if !seen[value] {
			seen[value] = true
			out = append(out, value)
		}
	}
	sortStrings(out)
	return out
}

// sortStrings 就地插入排序（清单只有几十项，且只在构造目录时跑一次；
// 避免为一个调用引入 sort 依赖以外的复杂度）。
func sortStrings(values []string) {
	for i := 1; i < len(values); i++ {
		current := values[i]
		j := i - 1
		for j >= 0 && values[j] > current {
			values[j+1] = values[j]
			j--
		}
		values[j+1] = current
	}
}

// modelFromSource 从已解析的出站请求体里取模型名。
func modelFromSource(src map[string]any) string {
	model, _ := src["model"].(string)
	return model
}

// staticModelKnown 报告一个模型名是否被静态别名表认识（键或值任一命中）。
//
// Remote 的 custom_model 绑定在目录不可用时按这张表判断「是否已知模型」：
// 已知 → 构造一个最小 custom_model（上游按 name/config_name 匹配）；
// 未知 → 不带 custom_model，让上游走 auto 路由而不是硬绑一个不存在的名字。
func staticModelKnown(name string) bool {
	trimmed := strings.TrimSpace(name)
	if trimmed == "" {
		return false
	}
	if _, ok := modelAliases[strings.ToLower(trimmed)]; ok {
		return true
	}
	for _, value := range modelAliases {
		if strings.EqualFold(value, trimmed) {
			return true
		}
	}
	return false
}
